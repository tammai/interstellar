// Package pipeline runs the three stages: ingest, classify, compose.
//
// Each stage is independently restartable and safe to run twice. That is what
// makes POST /api/refresh cheap and what makes a container killed mid-run
// recoverable (§10).
package pipeline

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/tammai/interstellar/internal/config"
	"github.com/tammai/interstellar/internal/sources"
	"github.com/tammai/interstellar/internal/store"
)

// ingestWorkers bounds concurrent fetches. Twenty-odd feeds against a handful of
// hosts: enough to keep a run well under the three-minute budget, low enough to
// be a polite client.
const ingestWorkers = 6

// tooOld reports whether an item is archive rather than news.
//
// Feeds routinely serve their entire history — one seeded blog carries 212
// posts, and some vendor feeds carry over a thousand. Without this, a first run
// classifies years of writing, pays for all of it, and can render a 2017 post as
// today's signal, because the pipeline keys off when we *fetched* an item rather
// than when it was *written*.
//
// An item with no publication date is kept: plenty of feeds omit one, and
// dropping those would silently lose real news. Better to classify a few extras
// than to go blind.
func tooOld(published time.Time, maxAgeDays int) bool {
	if published.IsZero() || maxAgeDays <= 0 {
		return false
	}
	return published.Before(time.Now().AddDate(0, 0, -maxAgeDays))
}

// SourceFailure records one source that didn't make it, for the status banner.
type SourceFailure struct {
	SourceID int64  `json:"source_id"`
	Name     string `json:"name"`
	Err      string `json:"error"`
}

// IngestResult summarizes a run.
type IngestResult struct {
	SourcesTried int `json:"sources_tried"`
	SourcesOK    int `json:"sources_ok"`
	ItemsNew     int `json:"items_new"`
	ItemsSeen    int `json:"items_seen"`
	// ItemsTooOld is archive content skipped as not-news. Reported rather than
	// silently dropped, so a feed contributing nothing is visible.
	ItemsTooOld int `json:"items_too_old"`
	// ItemsMerged is fuzzy duplicates folded into an existing item: the same
	// article at a different URL. Counted separately from ItemsSeen because it is
	// the one dedup level that could be wrong, and a number that jumps is the only
	// way anyone would notice.
	ItemsMerged int             `json:"items_merged"`
	Failures    []SourceFailure `json:"failures,omitempty"`
}

type fetchOutcome struct {
	source store.Source
	items  []sources.Item
	err    error
}

// Ingest fetches every active source and stores what it finds.
//
// A source that fails is recorded and skipped -- never fatal (§10: "dead feed
// doesn't break ingest"). The only errors returned are ones that make the whole
// stage meaningless, like being unable to read the source list at all.
func Ingest(ctx context.Context, db *store.DB, cfg config.Runtime, log *slog.Logger) (IngestResult, error) {
	all, err := db.ActiveSources()
	if err != nil {
		return IngestResult{}, err
	}

	// The paste-a-link inbox is a source row that exists only to attribute items
	// to; it has no feed to fetch. It is created paused, so it should never reach
	// here -- this guard is for the day someone activates it in Settings, where it
	// would otherwise fail on every run and clutter the one list a genuinely broken
	// feed needs to stand out in.
	srcs := make([]store.Source, 0, len(all))
	for _, src := range all {
		if store.IsManualSource(src) {
			continue
		}
		srcs = append(srcs, src)
	}

	result := IngestResult{SourcesTried: len(srcs)}
	if len(srcs) == 0 {
		return result, nil
	}

	opts := sources.Options{HNMinPoints: cfg.HNMinPoints}

	// The fuzzy-dedup index, read once. A lookup per feed entry would be a query
	// inside the write loop, which already holds the only connection.
	known, err := db.RecentTitles(fuzzyWindowDays, fuzzyIndexLimit)
	if err != nil {
		return result, err
	}

	jobs := make(chan store.Source)
	outcomes := make(chan fetchOutcome)

	var wg sync.WaitGroup
	for i := 0; i < min(ingestWorkers, len(srcs)); i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for src := range jobs {
				outcomes <- fetch(ctx, src, opts)
			}
		}()
	}

	go func() {
		defer close(jobs)
		for _, s := range srcs {
			select {
			case jobs <- s:
			case <-ctx.Done():
				return
			}
		}
	}()

	go func() {
		wg.Wait()
		close(outcomes)
	}()

	// Every write happens here, on one goroutine. The pool is a single
	// connection anyway; funnelling deliberately keeps that explicit.
	for out := range outcomes {
		if out.err != nil {
			log.Warn("source failed", "source", out.source.Name, "id", out.source.ID, "err", out.err)
			result.Failures = append(result.Failures, SourceFailure{
				SourceID: out.source.ID,
				Name:     out.source.Name,
				Err:      out.err.Error(),
			})
			continue
		}

		stored, seen, old, merged := store_(db, out, cfg.MaxItemAgeDays, &known, log)
		result.ItemsNew += stored
		result.ItemsSeen += seen
		result.ItemsTooOld += old
		result.ItemsMerged += merged
		result.SourcesOK++

		if err := db.MarkSourceRun(out.source.ID, time.Now()); err != nil {
			log.Warn("mark source run", "source", out.source.Name, "err", err)
		}
	}

	log.Info("ingest complete",
		"sources_ok", result.SourcesOK,
		"sources_failed", len(result.Failures),
		"items_new", result.ItemsNew,
		"items_seen", result.ItemsSeen,
		"items_too_old", result.ItemsTooOld,
	)
	return result, ctx.Err()
}

func fetch(ctx context.Context, src store.Source, opts sources.Options) fetchOutcome {
	fetcher, err := sources.For(src.Kind)
	if err != nil {
		return fetchOutcome{source: src, err: err}
	}

	// Per-source deadline so one hung host can't stall the run past its budget.
	ctx, cancel := context.WithTimeout(ctx, sources.FetchTimeout)
	defer cancel()

	items, err := fetcher.Fetch(ctx, src.Config, opts)
	return fetchOutcome{source: src, items: items, err: err}
}

// store_ writes one source's items, returning counts of new, already-seen,
// skipped-as-archive and fuzzy-merged. A single malformed URL is skipped rather
// than failing the source: feeds are full of one-off junk entries.
//
// known is the fuzzy-dedup index, read once per run and grown in place as new
// items land -- so two feeds carrying the same republish in the same run collapse
// too, not just a republish of something ingested yesterday.
func store_(
	db *store.DB,
	out fetchOutcome,
	maxAgeDays int,
	known *[]store.TitleRef,
	log *slog.Logger,
) (newCount, seenCount, tooOldCount, mergedCount int) {
	for _, it := range out.items {
		if tooOld(it.PublishedAt, maxAgeDays) {
			tooOldCount++
			continue
		}

		canonical, err := sources.Canonical(it.URL)
		if err != nil {
			log.Debug("skip unusable url", "source", out.source.Name, "url", it.URL, "err", err)
			continue
		}

		// Level three of deduplication, after URL canonicalization and before the
		// synthesizer's clustering: the same article at a different URL. Attribution
		// rather than a discard, because two outlets carrying one story is precisely
		// what source_count measures and what makes a signal gold (§6).
		if dup, ok := findDuplicate(*known, it.Title); ok {
			if err := db.AttributeItem(dup.ID, out.source.ID, it.Detail); err != nil {
				log.Warn("attribute fuzzy duplicate", "source", out.source.Name, "err", err)
				continue
			}
			log.Debug("merged fuzzy duplicate",
				"source", out.source.Name, "title", it.Title, "into", dup.Title)
			mergedCount++
			continue
		}

		id, isNew, err := db.UpsertItem(store.NewItem{
			SourceID:     out.source.ID,
			CanonicalURL: canonical,
			Title:        it.Title,
			Summary:      it.Summary,
			PublishedAt:  it.PublishedAt,
			Detail:       it.Detail,
		})
		if err != nil {
			log.Warn("store item", "source", out.source.Name, "url", canonical, "err", err)
			continue
		}
		if isNew {
			newCount++
			*known = append(*known, store.TitleRef{ID: id, Title: it.Title})
		} else {
			seenCount++
		}
	}
	return newCount, seenCount, tooOldCount, mergedCount
}

// fuzzyWindowDays bounds how far back a republish is looked for. A week: syndication
// is fast, and a title matching something from a month ago is far more likely to be
// an unrelated story that reuses a phrase.
const fuzzyWindowDays = 7

// fuzzyIndexLimit bounds the in-memory index. At ~250 items/day a week is ~1750,
// so this is headroom rather than a real ceiling -- and it keeps the comparison
// O(index) per item instead of O(all items ever).
const fuzzyIndexLimit = 4000

// findDuplicate looks for an existing item that is the same article.
//
// Linear, which is fine: a few thousand normalized titles against a few hundred new
// items per run is microseconds, and the alternative -- an inverted index -- would
// be real machinery guarding a cost nobody has measured as a problem.
func findDuplicate(known []store.TitleRef, title string) (store.TitleRef, bool) {
	for _, k := range known {
		if sources.SameArticle(k.Title, title) {
			return k, true
		}
	}
	return store.TitleRef{}, false
}

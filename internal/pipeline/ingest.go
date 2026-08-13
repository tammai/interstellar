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
	ItemsTooOld int             `json:"items_too_old"`
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

		stored, seen, old := store_(db, out, cfg.MaxItemAgeDays, log)
		result.ItemsNew += stored
		result.ItemsSeen += seen
		result.ItemsTooOld += old
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

// store_ writes one source's items, returning counts of new, already-seen and
// skipped-as-archive. A single malformed URL is skipped rather than failing the
// source: feeds are full of one-off junk entries.
func store_(db *store.DB, out fetchOutcome, maxAgeDays int, log *slog.Logger) (newCount, seenCount, tooOldCount int) {
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

		_, isNew, err := db.UpsertItem(store.NewItem{
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
		} else {
			seenCount++
		}
	}
	return newCount, seenCount, tooOldCount
}

package pipeline

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
	"sync"

	"github.com/tammai/interstellar/internal/config"
	"github.com/tammai/interstellar/internal/llm"
	"github.com/tammai/interstellar/internal/sources"
	"github.com/tammai/interstellar/internal/store"
)

// extractWindowLimit caps how many items one pass considers, the same safety net
// classifyWindowLimit is. It matters more here than there, because the extract
// watermark seeds empty on an instance that already holds a month of items: the
// backfill is wanted, but it should arrive over a few runs rather than as one
// enormous pass.
const extractWindowLimit = 1000

// extractWorkers bounds concurrent model calls, matching classifyWorkers for the
// same reason: enough to hide per-call latency, few enough to stay clear of a
// provider's rate limit.
const extractWorkers = 6

// extractMaxSummaryChars truncates an item summary in the prompt.
const extractMaxSummaryChars = 600

// changeTypes is the whitelist. In Go rather than a CHECK constraint, because
// SQLite cannot widen a CHECK without a table rebuild and this list will grow --
// see migration 009.
var changeTypes = map[string]bool{
	"launch":      true,
	"pricing":     true,
	"acquisition": true,
	"deprecation": true,
	"benchmark":   true,
	"incident":    true,
	"integration": true,
	"none":        true,
}

// ExtractResult summarizes a pass.
type ExtractResult struct {
	ItemsConsidered int `json:"items_considered"`
	Batches         int `json:"batches"`
	ClaimsWritten   int `json:"claims_written"`
	// Mentions counts entity sightings, claims and change_type "none" alike. The
	// gap between this and ClaimsWritten is how much of the watchlist is merely
	// being talked about rather than doing anything.
	Mentions int `json:"mentions"`
	// Unresolved counts entity names the model returned that no alias matches.
	// These are the candidate table's input (design item 5), and until that exists
	// they are dropped -- but counted, because a large number here means the
	// watchlist has drifted behind the field, which is the single most useful
	// thing this stage can tell you before candidates are built.
	Unresolved       int      `json:"unresolved"`
	UnresolvedNames  []string `json:"unresolved_names,omitempty"`
	BatchesCommitted int      `json:"batches_committed"`
	Repairs          int      `json:"repairs"`
	Problems         []string `json:"problems,omitempty"`
}

// extracted is one item's answer.
type extracted struct {
	ItemID int64            `json:"item_id"`
	Claims []extractedClaim `json:"claims"`
}

type extractedClaim struct {
	Entity     string  `json:"entity"`
	ChangeType string  `json:"change_type"`
	Summary    string  `json:"summary"`
	Confidence float64 `json:"confidence"`
}

// maxUnresolvedReported bounds the names carried back in the result. The point is
// to notice drift, not to reproduce a day's noise in a status payload.
const maxUnresolvedReported = 20

// Extract turns items into claims about watchlist entities.
//
// It runs alongside classification rather than instead of it. The two overlap --
// both read a title and a summary and judge it -- and extraction is a plausible
// eventual replacement, but replacing it now would take out the sky, the weekly
// round-up, trending and Ask in one move. Running both costs a few calls a day,
// breaks nothing, and puts the evidence for that decision in llm_calls where
// ModelReliabilityOver can read it.
//
// The model names entities; it never receives or returns ids. Resolution happens
// here against the alias map, for the same reason the weekly round-up renders its
// own list from ids rather than asking a model to copy URLs back: identity is
// something the database knows and a model can only guess at.
func Extract(
	ctx context.Context,
	db *store.DB,
	client *llm.Client,
	cfg config.Runtime,
	log *slog.Logger,
) (ExtractResult, error) {
	var result ExtractResult

	if !cfg.ExtractEnabled {
		return result, nil
	}

	// Both maps are loaded whole, before anything else opens a result set: the
	// pool is capped at one connection, so a lookup issued mid-iteration would
	// wait on a connection the outer loop is holding and hang rather than error.
	aliases, err := db.EntityAliasMap()
	if err != nil {
		return result, err
	}
	if len(aliases) == 0 {
		// No watchlist yet. Not an error -- an instance that has never imported
		// one should not report a problem every morning.
		return result, nil
	}
	entities, err := db.Entities()
	if err != nil {
		return result, err
	}
	byID := make(map[int64]store.Entity, len(entities))
	vendorHosts := make(map[string]bool, len(entities))
	for _, e := range entities {
		byID[e.ID] = e
		if h := hostOf(e.FirstPartyURL); h != "" {
			vendorHosts[h] = true
		}
	}

	items, err := db.ItemsForExtraction(cfg.ExtractWatermark, extractWindowLimit)
	if err != nil {
		return result, err
	}
	result.ItemsConsidered = len(items)
	if len(items) == 0 {
		return result, nil
	}

	batchSize := cfg.ExtractBatchSize
	if batchSize <= 0 {
		batchSize = 30
	}

	type batch struct {
		items []store.ExtractableItem
		idx   int
		// end is the exclusive index into `items` this batch reaches. Carried
		// rather than recomputed, because the watermark is derived from it and
		// re-deriving an offset from a batch index and a size is the kind of
		// arithmetic that is wrong once and then silently skips items forever.
		end int
	}
	var queue []batch
	for start := 0; start < len(items); start += batchSize {
		end := min(start+batchSize, len(items))
		queue = append(queue, batch{items: items[start:end], idx: len(queue), end: end})
	}
	result.Batches = len(queue)

	type outcome struct {
		idx      int
		answers  []extracted
		repaired bool
		err      error
	}

	jobs := make(chan batch)
	results := make(chan outcome)

	var wg sync.WaitGroup
	for i := 0; i < min(extractWorkers, len(queue)); i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for b := range jobs {
				answers, repaired, err := extractBatch(ctx, client, b.items, log)
				results <- outcome{idx: b.idx, answers: answers, repaired: repaired, err: err}
			}
		}()
	}

	go func() {
		defer close(jobs)
		for _, b := range queue {
			select {
			case jobs <- b:
			case <-ctx.Done():
				return
			}
		}
	}()

	go func() {
		wg.Wait()
		close(results)
	}()

	var (
		claims    []store.NewClaim
		sightings []store.EntitySighting
		fatal     error
	)
	// done[idx] marks a batch that came back parseable, for the prefix below.
	done := make([]bool, len(queue))
	unresolved := map[string]bool{}
	// itemByID lets an answer find its own item's url and source without a query.
	itemByID := make(map[int64]store.ExtractableItem, len(items))
	for _, it := range items {
		itemByID[it.ID] = it
	}

	for out := range results {
		if out.err != nil {
			if errors.Is(out.err, llm.ErrCapReached) || errors.Is(out.err, context.Canceled) ||
				errors.Is(out.err, context.DeadlineExceeded) {
				if fatal == nil {
					fatal = out.err
				}
				continue
			}
			log.Warn("extract batch failed", "batch", out.idx, "err", out.err)
			result.Problems = append(result.Problems, fmt.Sprintf("batch %d: %v", out.idx, out.err))
			continue
		}
		if out.repaired {
			result.Repairs++
		}

		for _, ans := range out.answers {
			item, ok := itemByID[ans.ItemID]
			if !ok {
				continue // keepExtracted already whitelists; belt and braces
			}
			for _, c := range ans.Claims {
				entityID, ok := aliases[store.NormalizeEntityName(c.Entity)]
				if !ok {
					result.Unresolved++
					unresolved[strings.TrimSpace(c.Entity)] = true
					continue
				}
				changeType := strings.ToLower(strings.TrimSpace(c.ChangeType))
				if !changeTypes[changeType] {
					changeType = "none"
				}

				sourceType := classifySource(item, byID[entityID], vendorHosts)
				// Every mention is a sighting, including a "none": that is what
				// separates a quiet tool from a dead one, and it is the only input
				// the staleness job has.
				sightings = append(sightings, store.EntitySighting{
					EntityID: entityID,
					At:       item.FetchedAt,
					Verified: sourceType == "first_party" || sourceType == "vendor_blog",
				})
				result.Mentions++

				if changeType == "none" {
					// Nothing changed, so there is nothing to claim. Storing these
					// would be one row per mention per run and would make `claims`
					// mean two things.
					continue
				}
				summary := strings.TrimSpace(c.Summary)
				if summary == "" {
					continue // a change with nothing said about it is not usable
				}
				claims = append(claims, store.NewClaim{
					EntityID:   entityID,
					ItemID:     item.ID,
					ChangeType: changeType,
					SourceType: sourceType,
					Summary:    sources.Truncate(summary, 400),
					URL:        item.CanonicalURL,
					Confidence: c.Confidence,
					Hash:       claimHash(entityID, changeType, item.CanonicalURL),
					SeenAt:     item.FetchedAt,
				})
			}
		}
		done[out.idx] = true
	}

	if fatal != nil {
		// Neither claims nor watermark. An aborted pass leaves its items unclaimed
		// so the next run sees them again, and committing half of it would move
		// last_seen_at on evidence that is about to be re-read.
		return result, fatal
	}

	// The watermark advances over an unbroken PREFIX of batches, the same rule the
	// classify mark follows and for the same reason: it is a single high-water
	// mark, so a gap behind it can never be revisited. Simpler here, because
	// extraction has one consumer rather than one per topic -- a batch is either
	// answered or it is not.
	prefix := len(queue)
	for i, ok := range done {
		if !ok {
			prefix = i
			break
		}
	}
	result.BatchesCommitted = prefix

	watermark := ""
	if prefix > 0 {
		high := cfg.ExtractWatermark
		for _, it := range items[:queue[prefix-1].end] {
			if it.FetchedAt > high {
				high = it.FetchedAt
			}
		}
		if high != cfg.ExtractWatermark {
			watermark = high
		}
	}

	if err := db.CommitExtractPass(claims, sightings, watermark); err != nil {
		return result, err
	}
	result.ClaimsWritten = len(claims)

	for name := range unresolved {
		if len(result.UnresolvedNames) >= maxUnresolvedReported {
			break
		}
		result.UnresolvedNames = append(result.UnresolvedNames, name)
	}

	log.Info("extract complete",
		"items", result.ItemsConsidered,
		"batches", result.Batches,
		"committed", result.BatchesCommitted,
		"claims", result.ClaimsWritten,
		"mentions", result.Mentions,
		"unresolved", result.Unresolved,
	)
	return result, nil
}

// extractBatch runs one batch, with a single repair round-trip.
func extractBatch(
	ctx context.Context,
	client *llm.Client,
	batch []store.ExtractableItem,
	log *slog.Logger,
) (answers []extracted, repaired bool, err error) {
	prompt := extractPrompt(batch)

	// CompleteAs, not Complete: the chain is the CLASSIFIER's (see RoleExtract's
	// doc comment), while the accounting is extraction's own. Complete would look
	// up a chain named "extract", which no settings key defines.
	resp, err := client.CompleteAs(ctx, llm.RoleClassifier, llm.RoleExtract, llm.Request{
		Messages: []llm.Message{
			{Role: "system", Content: extractSystemPrompt},
			{Role: "user", Content: prompt},
		},
		JSONMode: true,
		Expect:   len(batch),
	})
	if err != nil {
		return nil, false, err
	}

	parsed, parseErr := parseExtracted(resp.Text)
	kept := keepExtracted(batch, parsed)
	client.ReportReply(resp.CallID, len(kept), parseErr == nil)
	if parseErr == nil {
		return kept, false, nil
	}

	log.Debug("extractor output unparseable, repairing", "err", parseErr)

	repairResp, repairErr := client.CompleteAs(ctx, llm.RoleClassifier, llm.RoleExtract, llm.Request{
		Messages: []llm.Message{
			{Role: "system", Content: extractSystemPrompt},
			{Role: "user", Content: prompt},
			{Role: "assistant", Content: resp.Text},
			{Role: "user", Content: "That was not valid JSON. Reply with only the JSON object, " +
				"no prose and no code fences."},
		},
		JSONMode: true,
		Expect:   len(batch),
		Repair:   true,
	})
	if repairErr != nil {
		return nil, true, fmt.Errorf("unparseable output and repair failed: %w", repairErr)
	}

	parsed, parseErr = parseExtracted(repairResp.Text)
	kept = keepExtracted(batch, parsed)
	client.ReportReply(repairResp.CallID, len(kept), parseErr == nil)
	if parseErr != nil {
		return nil, true, fmt.Errorf("unparseable output after repair: %w", parseErr)
	}
	return kept, true, nil
}

// keepExtracted reduces a reply to answers to THIS batch, counted once.
//
// Same rule as classify's keepAnswers, and it exists here for the same reason: an
// id the model invented attaches a claim to an unrelated article, and an id
// repeated pushes items_out above items_in so completeness reads over 100% and
// cancels out a batch that silently truncated in the same provider/model group.
// First answer for an id wins.
func keepExtracted(batch []store.ExtractableItem, answers []extracted) []extracted {
	known := make(map[int64]bool, len(batch))
	for _, it := range batch {
		known[it.ID] = true
	}
	seen := make(map[int64]bool, len(answers))
	var kept []extracted
	for _, a := range answers {
		if !known[a.ItemID] || seen[a.ItemID] {
			continue
		}
		seen[a.ItemID] = true
		kept = append(kept, a)
	}
	return kept
}

func parseExtracted(text string) ([]extracted, error) {
	elems, err := llm.ExtractArray(text)
	if err != nil {
		return nil, err
	}
	answers, dropped := llm.DecodeElements(elems, func(e extracted) error {
		if e.ItemID == 0 {
			return errors.New("missing item_id")
		}
		return nil
	})
	if dropped > 0 && len(answers) == 0 {
		return nil, fmt.Errorf("all %d answers malformed", dropped)
	}
	return answers, nil
}

// classifySource decides where a claim came from, locally.
//
// Never asked of the model: it is derived from the item's host and the source's
// kind, both of which are already known here, and a derived value cannot be
// hallucinated. It gates last_verified_at, so a model guessing "first_party"
// would make a staleness job report vendors as active on the strength of a
// stranger's blog post.
func classifySource(item store.ExtractableItem, entity store.Entity, vendorHosts map[string]bool) string {
	host := hostOf(item.CanonicalURL)
	switch {
	case host != "" && sameHost(host, hostOf(entity.FirstPartyURL)):
		return "first_party"
	case host != "" && vendorHosts[host]:
		// Some other vendor's own site talking about this entity. Still a primary
		// publication, still not the subject speaking about itself.
		return "vendor_blog"
	case item.SourceKind == "hn_search":
		return "aggregator"
	default:
		return "independent"
	}
}

// hostOf returns a URL's lowercased host without a leading www.
func hostOf(raw string) string {
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return ""
	}
	return strings.TrimPrefix(strings.ToLower(u.Hostname()), "www.")
}

// sameHost treats a subdomain as the same publication: a vendor's changelog often
// lives at docs. or blog. of its own domain, and calling that independent
// reporting would be wrong in the direction that matters.
func sameHost(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	return a == b || strings.HasSuffix(a, "."+b) || strings.HasSuffix(b, "."+a)
}

// claimHash identifies a change for the delta rule's "unseen in 14 days".
//
// Over the entity, the change type and the canonical url -- NOT the summary,
// which the model rewords on every pass and which would therefore defeat the
// check it exists to serve.
func claimHash(entityID int64, changeType, canonicalURL string) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%d\x00%s\x00%s", entityID, changeType, canonicalURL)))
	return hex.EncodeToString(sum[:16])
}

// The reply shape is an OBJECT wrapping an array, and that is load-bearing for
// the reason classifySystemPrompt spells out: JSONMode sends response_format
// {"type":"json_object"}, and asking for a top-level array under it does not fail
// loudly -- several providers return ONE element and silently drop the rest of
// the batch. Keep the prompt object-shaped.
const extractSystemPrompt = `You extract factual changes about named software products and companies.

Reply with ONLY a JSON object holding one entry per item you were given:
{"items": [{"item_id": <number>, "claims": [{"entity": "<name>", "change_type": "<type>", "summary": "<one sentence>", "confidence": <0-1>}]}]}

Every item_id you were given must appear exactly once, even when it mentions
nothing -- return "claims": [] for those. Most items will have none.

change_type is one of:
  launch       - a new product, model, version or feature is available
  pricing      - a price, tier or free allowance changed
  acquisition  - acquired, merged, or a funding round closed
  deprecation  - retired, sunset, discontinued, or renamed away
  benchmark    - a measured result was published
  incident     - an outage, breach, vulnerability or safety failure
  integration  - now works with, ships inside, or supports another named product
  none         - the item mentions it but nothing changed

Use the entity's name exactly as the item writes it -- do not normalize, expand or
correct it. Names are resolved against a list afterwards.

Only name something that the item actually says changed. An item that merely
discusses, compares or reviews a product is change_type "none". Opinion, tutorials
and roundups are almost always "none". Do not infer a change from a headline that
only implies one.

summary is one sentence stating what changed, specific enough to act on: name the
version, the price, the acquirer. Omit it for "none".

confidence is how certain you are that this change is real and correctly
attributed, 0 to 1.

Always write the summary in English.

No preamble, no code fences.`

func extractPrompt(batch []store.ExtractableItem) string {
	var b strings.Builder
	b.WriteString("ITEMS:\n")

	for _, it := range batch {
		summary := it.Summary
		if len(summary) > extractMaxSummaryChars {
			summary = sources.Truncate(summary, extractMaxSummaryChars)
		}
		fmt.Fprintf(&b, "\nitem_id: %d\ntitle: %s\n", it.ID, it.Title)
		if it.SourceName != "" {
			fmt.Fprintf(&b, "source: %s\n", it.SourceName)
		}
		if summary != "" {
			fmt.Fprintf(&b, "summary: %s\n", summary)
		}
	}

	fmt.Fprintf(&b, "\nReturn one entry for all %d items. Return the JSON object only.", len(batch))
	return b.String()
}

package pipeline

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/tammai/interstellar/internal/config"
	"github.com/tammai/interstellar/internal/llm"
	"github.com/tammai/interstellar/internal/store"
)

// classifyWindowLimit caps how many items one pass will consider. A safety net,
// not a normal path: at ~250 items/day the watermark keeps batches small, but a
// first run against a fresh volume can pull a source's entire backlog.
const classifyWindowLimit = 2000

// maxSummaryChars truncates summaries in the prompt. Relevance is decided by
// the first couple of sentences; the rest is tokens billed per topic per batch.
const maxSummaryChars = 400

// ClassifyResult summarizes a pass.
type ClassifyResult struct {
	ItemsConsidered int      `json:"items_considered"`
	TopicsRun       int      `json:"topics_run"`
	Batches         int      `json:"batches"`
	MatchesKept     int      `json:"matches_kept"`
	VerdictsDropped int      `json:"verdicts_dropped"`
	Repairs         int      `json:"repairs"`
	Problems        []string `json:"problems,omitempty"`
}

type verdict struct {
	ItemID int64   `json:"item_id"`
	Score  float64 `json:"score"`
	Reason string  `json:"reason"`
}

// Classify scores new items against each active topic.
//
// Per topic, not one mega-prompt across all seven (§5.2). The reason is verdict
// quality: a prompt holding one topic definition gets a sharper judgment than
// one juggling seven, and it makes per-topic hit-rate stats free later. It costs
// more calls, which is what the batch size and the watermark are for.
func Classify(
	ctx context.Context,
	db *store.DB,
	client *llm.Client,
	cfg config.Runtime,
	log *slog.Logger,
) (ClassifyResult, error) {
	var result ClassifyResult

	topics, err := db.ActiveTopics()
	if err != nil {
		return result, err
	}

	// A topic with no definition has nothing to classify against. This is the
	// first-run state, and the UI shows a setup hint for it (§7) -- burning
	// tokens asking a model to match against an empty string would be worse than
	// doing nothing.
	var ready []store.Topic
	for _, t := range topics {
		if t.NeedsSetup() {
			result.Problems = append(result.Problems,
				fmt.Sprintf("topic %q has no definition yet", t.Title))
			continue
		}
		ready = append(ready, t)
	}
	if len(ready) == 0 {
		return result, nil
	}

	items, err := db.ItemsSince(cfg.ClassifyWatermark, classifyWindowLimit)
	if err != nil {
		return result, err
	}
	result.ItemsConsidered = len(items)
	if len(items) == 0 {
		return result, nil
	}

	batchSize := cfg.ClassifyBatchSize
	if batchSize <= 0 {
		batchSize = 30
	}

	// Track the high-water mark as we go, but only commit it if the whole pass
	// completes. A partial pass that advanced the watermark would silently skip
	// items forever.
	highWater := cfg.ClassifyWatermark

	for _, topic := range ready {
		result.TopicsRun++

		for start := 0; start < len(items); start += batchSize {
			end := min(start+batchSize, len(items))
			batch := items[start:end]
			result.Batches++

			verdicts, repaired, err := classifyBatch(ctx, client, topic, batch, log)
			if err != nil {
				if errors.Is(err, llm.ErrCapReached) || errors.Is(err, context.Canceled) ||
					errors.Is(err, context.DeadlineExceeded) {
					// The guard stopping a run, or a shutdown: stop cleanly and
					// keep what we have. The watermark is deliberately not
					// advanced.
					return result, err
				}
				log.Warn("classify batch failed",
					"topic", topic.Title, "items", len(batch), "err", err)
				result.Problems = append(result.Problems,
					fmt.Sprintf("topic %q: %v", topic.Title, err))
				continue
			}
			if repaired {
				result.Repairs++
			}

			matches := make([]store.Match, 0, len(verdicts))
			known := itemIDSet(batch)
			for _, v := range verdicts {
				// A model can hallucinate an id that wasn't in the batch;
				// storing one would attach a verdict to an unrelated article.
				if !known[v.ItemID] {
					result.VerdictsDropped++
					continue
				}
				if v.Score < cfg.MatchThreshold {
					continue // §5.2 keeps >= 4 only
				}
				matches = append(matches, store.Match{
					ItemID:  v.ItemID,
					TopicID: topic.ID,
					Score:   v.Score,
					Reason:  strings.TrimSpace(v.Reason),
				})
			}

			if err := db.SaveMatches(matches); err != nil {
				return result, err
			}
			result.MatchesKept += len(matches)
		}
	}

	for _, it := range items {
		if it.FetchedAt > highWater {
			highWater = it.FetchedAt
		}
	}
	if highWater != cfg.ClassifyWatermark {
		if err := db.SetSetting(store.KeyClassifyWatermark, highWater); err != nil {
			return result, err
		}
	}

	log.Info("classify complete",
		"items", result.ItemsConsidered,
		"topics", result.TopicsRun,
		"batches", result.Batches,
		"matches", result.MatchesKept,
		"dropped", result.VerdictsDropped,
		"repairs", result.Repairs,
	)
	return result, nil
}

// classifyBatch runs one topic against one batch, with a single repair attempt if
// the first response cannot be parsed.
func classifyBatch(
	ctx context.Context,
	client *llm.Client,
	topic store.Topic,
	batch []store.ClassifiableItem,
	log *slog.Logger,
) (verdicts []verdict, repaired bool, err error) {
	prompt := classifyPrompt(topic, batch)

	resp, err := client.Complete(ctx, llm.RoleClassifier, llm.Request{
		Messages: []llm.Message{
			{Role: "system", Content: classifySystemPrompt},
			{Role: "user", Content: prompt},
		},
		JSONMode: true,
	})
	if err != nil {
		return nil, false, err
	}

	verdicts, parseErr := parseVerdicts(resp.Text)
	if parseErr == nil {
		return verdicts, false, nil
	}

	// One repair round-trip. Cheaper than losing thirty verdicts, and bounded so
	// a model that cannot produce JSON does not become an infinite bill.
	log.Debug("classifier output unparseable, repairing", "topic", topic.Title, "err", parseErr)

	repairResp, repairErr := client.Complete(ctx, llm.RoleClassifier, llm.Request{
		Messages: []llm.Message{
			{Role: "system", Content: classifySystemPrompt},
			{Role: "user", Content: prompt},
			{Role: "assistant", Content: resp.Text},
			{Role: "user", Content: "That was not valid JSON. Reply with only the JSON array, " +
				"no prose and no code fences."},
		},
		JSONMode: true,
	})
	if repairErr != nil {
		return nil, true, fmt.Errorf("unparseable output and repair failed: %w", repairErr)
	}

	verdicts, parseErr = parseVerdicts(repairResp.Text)
	if parseErr != nil {
		return nil, true, fmt.Errorf("unparseable output after repair: %w", parseErr)
	}
	return verdicts, true, nil
}

func parseVerdicts(text string) ([]verdict, error) {
	elems, err := llm.ExtractArray(text)
	if err != nil {
		return nil, err
	}
	verdicts, dropped := llm.DecodeElements(elems, func(v verdict) error {
		if v.ItemID == 0 {
			return errors.New("missing item_id")
		}
		return nil
	})
	if dropped > 0 && len(verdicts) == 0 {
		return nil, fmt.Errorf("all %d verdicts malformed", dropped)
	}
	return verdicts, nil
}

const classifySystemPrompt = `You score news items for relevance to a single topic.

Reply with ONLY a JSON array, one object per item you were given:
[{"item_id": <number>, "score": <1-5>, "reason": "<one sentence>"}]

Scoring:
5 - squarely about this topic, and substantive
4 - clearly relevant, worth the reader's time
3 - adjacent or tangential
2 - same broad field, not this topic
1 - unrelated

Judge the item, not the source's reputation. The reason must say what in the item
matched the topic definition, in one sentence, and must be specific enough to be
useful to someone deciding whether to click. No preamble, no code fences.`

func classifyPrompt(topic store.Topic, batch []store.ClassifiableItem) string {
	var b strings.Builder

	fmt.Fprintf(&b, "TOPIC: %s\n\nDEFINITION:\n%s\n\nITEMS:\n", topic.Title, topic.Definition)

	for _, it := range batch {
		summary := it.Summary
		if len(summary) > maxSummaryChars {
			summary = summary[:maxSummaryChars]
		}
		fmt.Fprintf(&b, "\nitem_id: %d\ntitle: %s\n", it.ID, it.Title)
		if it.SourceName != "" {
			fmt.Fprintf(&b, "source: %s\n", it.SourceName)
		}
		if summary != "" {
			fmt.Fprintf(&b, "summary: %s\n", summary)
		}
	}

	fmt.Fprintf(&b, "\nScore all %d items. Return the JSON array only.", len(batch))
	return b.String()
}

func itemIDSet(items []store.ClassifiableItem) map[int64]bool {
	set := make(map[int64]bool, len(items))
	for _, it := range items {
		set[it.ID] = true
	}
	return set
}

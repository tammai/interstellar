package pipeline

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"

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

// classifyWorkers bounds concurrent model calls.
//
// Batches are independent, so this is the difference between a refresh that
// fits §10's three-minute budget and one that takes half an hour. Six is a
// compromise: enough to hide per-call latency, low enough to stay clear of a
// provider's rate limit — and a 429 costs more than the parallelism saves,
// because it burns a retry and then the fallback chain.
const classifyWorkers = 6

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
	runDate string,
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

	// Every (topic, batch) pair is independent, so build the whole work list and
	// run it through a bounded pool. Sequentially this is topics x batches round
	// trips end to end -- measured at ~25s each against a real provider, which
	// blows §10's three-minute budget several times over before the day's second
	// topic is finished.
	type work struct {
		topic store.Topic
		batch []store.ClassifiableItem
	}
	var queue []work
	for _, topic := range ready {
		result.TopicsRun++
		for start := 0; start < len(items); start += batchSize {
			end := min(start+batchSize, len(items))
			queue = append(queue, work{topic: topic, batch: items[start:end]})
		}
	}
	result.Batches = len(queue)

	type outcome struct {
		matches  []store.Match
		dropped  int
		repaired bool
		topic    string
		topicID  int64
		// considered is the batch size, carried so per-topic stats can count only
		// the batches that actually came back parseable.
		considered int
		err        error
	}

	jobs := make(chan work)
	results := make(chan outcome)

	var wg sync.WaitGroup
	for i := 0; i < min(classifyWorkers, len(queue)); i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for w := range jobs {
				verdicts, repaired, err := classifyBatch(ctx, client, w.topic, w.batch, log)
				out := outcome{
					repaired:   repaired,
					err:        err,
					topic:      w.topic.Title,
					topicID:    w.topic.ID,
					considered: len(w.batch),
				}
				if err == nil {
					known := itemIDSet(w.batch)
					for _, v := range verdicts {
						// A model can hallucinate an id that wasn't in the
						// batch; storing one would attach a verdict to an
						// unrelated article.
						if !known[v.ItemID] {
							out.dropped++
							continue
						}
						if v.Score < cfg.MatchThreshold {
							continue // §5.2 keeps >= 4 only
						}
						out.matches = append(out.matches, store.Match{
							ItemID:  v.ItemID,
							TopicID: w.topic.ID,
							Score:   v.Score,
							Reason:  strings.TrimSpace(v.Reason),
						})
					}
				}
				results <- out
			}
		}()
	}

	go func() {
		defer close(jobs)
		for _, w := range queue {
			select {
			case jobs <- w:
			case <-ctx.Done():
				return
			}
		}
	}()

	go func() {
		wg.Wait()
		close(results)
	}()

	// All database writes happen here, on one goroutine.
	//
	// stats accrue per topic as batches land, so a topic's denominator counts the
	// batches that succeeded and nothing else. Keyed by id rather than title
	// because two topics may share a name.
	stats := map[int64]*store.TopicStat{}
	statOrder := make([]int64, 0, len(ready))

	var fatal error
	for out := range results {
		if out.err != nil {
			if errors.Is(out.err, llm.ErrCapReached) || errors.Is(out.err, context.Canceled) ||
				errors.Is(out.err, context.DeadlineExceeded) {
				// The guard stopping a run, or a shutdown. Keep draining so the
				// workers can exit, but remember to stop afterwards without
				// advancing the watermark.
				if fatal == nil {
					fatal = out.err
				}
				continue
			}
			log.Warn("classify batch failed", "topic", out.topic, "err", out.err)
			result.Problems = append(result.Problems,
				fmt.Sprintf("topic %q: %v", out.topic, out.err))
			continue
		}
		if out.repaired {
			result.Repairs++
		}
		result.VerdictsDropped += out.dropped

		if err := db.SaveMatches(out.matches); err != nil {
			return result, err
		}
		result.MatchesKept += len(out.matches)

		st := stats[out.topicID]
		if st == nil {
			st = &store.TopicStat{TopicID: out.topicID}
			stats[out.topicID] = st
			statOrder = append(statOrder, out.topicID)
		}
		st.ItemsConsidered += out.considered
		st.MatchesKept += len(out.matches)
		st.Batches++
	}
	if fatal != nil {
		// Deliberately no stats and no watermark. An aborted pass leaves its items
		// unclaimed, so the next run classifies them again -- recording this pass's
		// denominator now would count those items twice and understate the topic.
		return result, fatal
	}

	for _, it := range items {
		if it.FetchedAt > highWater {
			highWater = it.FetchedAt
		}
	}

	watermark := ""
	if highWater != cfg.ClassifyWatermark {
		watermark = highWater
	}
	pass := make([]store.TopicStat, 0, len(statOrder))
	for _, id := range statOrder {
		pass = append(pass, *stats[id])
	}
	if err := db.CommitClassifyPass(runDate, pass, watermark); err != nil {
		return result, err
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

Include "reason" ONLY for items you score 4 or 5. For anything scored 3 or below,
return just the item_id and score. Most items in a batch are not relevant, and a
justification for each one is text nobody will ever read -- it is the single
largest cost and the largest source of latency in a run.

Scoring:
5 - squarely about this topic, and substantive
4 - clearly relevant, worth the reader's time
3 - adjacent or tangential
2 - same broad field, not this topic
1 - unrelated

Judge the item, not the source's reputation. The reason must say what in the item
matched the topic definition, in one sentence, and must be specific enough to be
useful to someone deciding whether to click.

Always write the reason in English, even when the topic definition is written in
another language. The definition is the owner's private note to themselves; the
reason is read in the interface, which is English.

No preamble, no code fences.`

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

	fmt.Fprintf(&b, "\nScore all %d items. Include a reason only for those scoring 4 or 5. "+
		"Return the JSON array only.", len(batch))
	return b.String()
}

func itemIDSet(items []store.ClassifiableItem) map[int64]bool {
	set := make(map[int64]bool, len(items))
	for _, it := range items {
		set[it.ID] = true
	}
	return set
}

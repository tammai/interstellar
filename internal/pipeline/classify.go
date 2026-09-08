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
	"github.com/tammai/interstellar/internal/sources"
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
	ItemsConsidered int `json:"items_considered"`
	TopicsRun       int `json:"topics_run"`
	Batches         int `json:"batches"`
	MatchesKept     int `json:"matches_kept"`
	// RangesCommitted/RangesTotal say how much of the window the watermark could
	// claim. Anything short of the total means a topic failed and those items will
	// be offered again next run -- worth surfacing, because it is also the shape of
	// a stalled watermark.
	RangesCommitted int      `json:"ranges_committed"`
	RangesTotal     int      `json:"ranges_total"`
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
	// Item ranges are identical for every topic, so a range index identifies "this
	// slice of items" across all of them. That is what makes it possible to tell
	// whether a slice has been judged by everyone, which the watermark depends on.
	type itemRange struct{ start, end int }
	var ranges []itemRange
	for start := 0; start < len(items); start += batchSize {
		ranges = append(ranges, itemRange{start: start, end: min(start+batchSize, len(items))})
	}

	type work struct {
		topic    store.Topic
		batch    []store.ClassifiableItem
		rangeIdx int
	}
	var queue []work
	for _, topic := range ready {
		result.TopicsRun++
		for i, r := range ranges {
			queue = append(queue, work{topic: topic, batch: items[r.start:r.end], rangeIdx: i})
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
		rangeIdx   int
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
				verdicts, dropped, repaired, err := classifyBatch(ctx, client, w.topic, w.batch, log)
				out := outcome{
					dropped:    dropped,
					repaired:   repaired,
					err:        err,
					topic:      w.topic.Title,
					topicID:    w.topic.ID,
					considered: len(w.batch),
					rangeIdx:   w.rangeIdx,
				}
				if err == nil {
					// verdicts are already whitelisted and deduplicated against
					// the batch -- see keepAnswers, which classifyBatch applies
					// because it is also what it reports as answered. All that
					// is left here is §5.2's threshold.
					for _, v := range verdicts {
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
	// Successful batches are buffered rather than folded into stats immediately,
	// because whether their work counts depends on something not known until every
	// batch has landed: how far the watermark can move. See the prefix calculation
	// below.
	type success struct {
		topicID    int64
		rangeIdx   int
		considered int
		matches    int
	}
	var successes []success
	// judged[rangeIdx] counts how many topics returned a verdict for that slice.
	judged := make([]int, len(ranges))

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

		successes = append(successes, success{
			topicID: out.topicID, rangeIdx: out.rangeIdx,
			considered: out.considered, matches: len(out.matches),
		})
		judged[out.rangeIdx]++
	}
	if fatal != nil {
		// Deliberately no stats and no watermark. An aborted pass leaves its items
		// unclaimed, so the next run classifies them again -- recording this pass's
		// denominator now would count those items twice and understate the topic.
		return result, fatal
	}

	// The watermark may only pass items EVERY ready topic has judged.
	//
	// It is a single high-water mark, so it can only move over an unbroken prefix:
	// if the first slice went unjudged by one topic, nothing behind it can be
	// claimed either. Advancing regardless -- which is what this did before -- left
	// a permanent hole in that topic's coverage, visible only as a topic that
	// mysteriously matches nothing.
	//
	// The cost of the honest version is that a topic failing persistently on the
	// first slice stalls the watermark and re-classifies the same items every run.
	// That is the right way round: it is bounded by the per-run call cap, it is
	// loud (a problem reported every run, and a zero in the hit-rate view), and §10
	// gates the classifier on 100% JSON parsing precisely so this path stays rare.
	// Silent data loss is neither bounded nor visible.
	prefix := len(ranges)
	for i, n := range judged {
		if n < len(ready) {
			prefix = i
			break
		}
	}
	result.RangesCommitted = prefix
	result.RangesTotal = len(ranges)

	if prefix > 0 {
		for _, it := range items[:ranges[prefix-1].end] {
			if it.FetchedAt > highWater {
				highWater = it.FetchedAt
			}
		}
	}

	watermark := ""
	if highWater != cfg.ClassifyWatermark {
		watermark = highWater
	}

	// Stats cover EVERY successful batch, including ones behind an unadvanced
	// watermark -- deliberately unlike the aborted-pass path, which records nothing.
	//
	// The distinction is what can be attributed. An aborted pass has no idea how much
	// of the window it got through, so counting any of it is guesswork. A partially
	// judged pass knows exactly what each topic did, and that is real work worth
	// showing.
	//
	// The first version of this gated stats on the prefix too, and the result was
	// perverse: one topic failing on the first slice blanked the hit-rate view for
	// EVERY topic, destroying the one diagnostic that would have identified the
	// failing one. Re-classifying a slice next run does double-count it -- but it
	// double-counts numerator and denominator together, so the rate the view exists
	// to show is unchanged.
	stats := map[int64]*store.TopicStat{}
	statOrder := make([]int64, 0, len(ready))
	for _, sc := range successes {
		st := stats[sc.topicID]
		if st == nil {
			st = &store.TopicStat{TopicID: sc.topicID}
			stats[sc.topicID] = st
			statOrder = append(statOrder, sc.topicID)
		}
		st.ItemsConsidered += sc.considered
		st.MatchesKept += sc.matches
		st.Batches++
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
) (verdicts []verdict, dropped int, repaired bool, err error) {
	prompt := classifyPrompt(topic, batch)

	resp, err := client.Complete(ctx, llm.RoleClassifier, llm.Request{
		Messages: []llm.Message{
			{Role: "system", Content: classifySystemPrompt},
			{Role: "user", Content: prompt},
		},
		JSONMode: true,
		Expect:   len(batch),
	})
	if err != nil {
		return nil, 0, false, err
	}

	parsed, parseErr := parseVerdicts(resp.Text)
	kept, dropped := keepAnswers(batch, parsed)
	// Report before branching, so the reply that gets repaired is still on the
	// record as the one that needed it. This is the measurement that would have
	// caught the object-shape bug in the note below: a batch of thirty answered
	// with one verdict parses cleanly and looks like a working model, and only
	// asked-versus-answered says otherwise.
	//
	// It reports `kept`, never the raw parse. Answered has to be answers to THIS
	// batch, or the ratio it feeds stops meaning anything -- see keepAnswers.
	client.ReportReply(resp.CallID, len(kept), parseErr == nil)
	if parseErr == nil {
		return kept, dropped, false, nil
	}

	// One repair round-trip. Cheaper than losing thirty verdicts, and bounded so
	// a model that cannot produce JSON does not become an infinite bill.
	log.Debug("classifier output unparseable, repairing", "topic", topic.Title, "err", parseErr)

	repairResp, repairErr := client.Complete(ctx, llm.RoleClassifier, llm.Request{
		Messages: []llm.Message{
			{Role: "system", Content: classifySystemPrompt},
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
		return nil, 0, true, fmt.Errorf("unparseable output and repair failed: %w", repairErr)
	}

	parsed, parseErr = parseVerdicts(repairResp.Text)
	kept, dropped = keepAnswers(batch, parsed)
	client.ReportReply(repairResp.CallID, len(kept), parseErr == nil)
	if parseErr != nil {
		return nil, 0, true, fmt.Errorf("unparseable output after repair: %w", parseErr)
	}
	return kept, dropped, true, nil
}

// keepAnswers reduces a parsed reply to the verdicts that are actually answers to
// this batch: in it, and counted once. Everything else is dropped and counted.
//
// Two failure modes, and only one of them was handled before. A hallucinated id
// was already rejected, because storing one attaches a verdict to an unrelated
// article. A REPEATED id was not: it passed the whitelist twice, so one item
// answered twice was appended twice, counted twice in MatchesKept, and reported
// twice as answered.
//
// The matches table saved it from being data loss -- PRIMARY KEY (item_id,
// topic_id) with an upsert, so the second write updates the first row rather than
// adding one. What it corrupted was measurement, in the two places measurement is
// load-bearing. `items_out` could exceed `items_in`, so completeness read above
// 100% (observed: 3427 answered against 3423 asked), and a batch over-answering
// could cancel out another one silently truncating inside the same
// provider/model/role group -- which is the exact failure completeness exists to
// catch. And the hit-rate numerator inflated, on the diagnostic used to decide
// whether a topic definition is drawn too tight.
//
// First verdict for an id wins. A model repeating itself with two different scores
// is not expressing a considered second opinion, and taking the later one would
// make the stored score depend on reply ordering.
func keepAnswers(batch []store.ClassifiableItem, verdicts []verdict) (kept []verdict, dropped int) {
	known := itemIDSet(batch)
	seen := make(map[int64]bool, len(verdicts))
	for _, v := range verdicts {
		if !known[v.ItemID] || seen[v.ItemID] {
			dropped++
			continue
		}
		seen[v.ItemID] = true
		kept = append(kept, v)
	}
	return kept, dropped
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

// The reply shape is an OBJECT wrapping an array, and that is load-bearing:
// JSONMode sends response_format {"type":"json_object"}, whose contract is a
// top-level object. Asking for a top-level array under it does not fail loudly --
// the model returns ONE verdict object and silently discards the rest of the
// batch. Measured on openrouter (2026-08-21): a 12-item batch came back as a
// single verdict from both deepseek-v4-flash and gpt-5-mini; the same batch under
// this object shape returned all 12. It cost seven days of empty skies, because
// the seeded deepseek-chat happened to tolerate the array shape and nothing broke
// until the roles moved to other models. llm.ExtractArray finds the inner array
// either way, so the parse side needs nothing -- keep the PROMPTS object-shaped.
const classifySystemPrompt = `You score news items for relevance to a single topic.

Reply with ONLY a JSON object holding one entry per item you were given:
{"verdicts": [{"item_id": <number>, "score": <1-5>, "reason": "<one sentence>"}]}

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
			summary = sources.Truncate(summary, maxSummaryChars)
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
		"Return the JSON object only.", len(batch))
	return b.String()
}

func itemIDSet(items []store.ClassifiableItem) map[int64]bool {
	set := make(map[int64]bool, len(items))
	for _, it := range items {
		set[it.ID] = true
	}
	return set
}

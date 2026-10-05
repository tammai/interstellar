package pipeline

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/tammai/interstellar/internal/config"
	"github.com/tammai/interstellar/internal/llm"
	"github.com/tammai/interstellar/internal/sources"
	"github.com/tammai/interstellar/internal/store"
)

// classifyWindowLimit caps how many items one pass will consider. A safety net,
// not a normal path: at ~250 items/day the watermark keeps batches small, but a
// first run against a fresh volume can pull a source's entire backlog. The call
// budget usually binds first -- see classifyBudget.
const classifyWindowLimit = 2000

// composeCallsPerTopic is the budget classify leaves for compose: one call per
// topic plus its one repair. Compose runs after classify on the same per-run cap,
// and a classify pass that spends all of it leaves a backlog run with no sky.
const composeCallsPerTopic = 2

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
	// RangesCommitted/RangesTotal count batches: how many the topics' cursors could
	// claim, out of how many were queued. Anything short of the total means a batch
	// failed and its items will be offered again next run -- worth surfacing,
	// because it is also the shape of a stalled cursor.
	RangesCommitted int `json:"ranges_committed"`
	RangesTotal     int `json:"ranges_total"`
	// BudgetLimited means the queue was cut to fit the run's remaining call budget,
	// and ItemsPending is how many distinct items some topic has yet to judge.
	// Together they are the difference between "caught up" and "catching up".
	BudgetLimited   bool     `json:"budget_limited,omitempty"`
	ItemsPending    int      `json:"items_pending"`
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

	batchSize := cfg.ClassifyBatchSize
	if batchSize <= 0 {
		batchSize = 30
	}

	budget, limited, err := classifyBudget(client, len(ready))
	if err != nil {
		return result, err
	}
	floor := classifyFloor(cfg)

	// Each topic reads from its own cursor. On an ordinary day they all sit at the
	// same place and read the same items; a topic that was added, resumed or given
	// its definition since the last run sits further back and reads more -- that is
	// the backfill, and it costs only that topic's calls. The floor stops a topic
	// paused for months from re-reading all of it: nothing older than the sky's
	// window can reach the sky.
	type itemRange struct{ start, end int }
	type lane struct {
		topic  store.Topic
		items  []store.ClassifiableItem
		ranges []itemRange
		// done[i] is whether range i came back with verdicts. The cursor moves over
		// the unbroken prefix of these, and nothing else.
		done []bool
	}
	lanes := make([]*lane, 0, len(ready))
	for _, t := range ready {
		items, err := db.ItemsSince(store.LaterWatermark(t.ClassifiedThrough, floor), classifyWindowLimit)
		if err != nil {
			return result, err
		}
		l := &lane{topic: t, items: items}
		for start := 0; start < len(items); start += batchSize {
			l.ranges = append(l.ranges, itemRange{start: start, end: min(start+batchSize, len(items))})
		}
		l.done = make([]bool, len(l.ranges))
		lanes = append(lanes, l)
	}

	// Round-robin by range: every topic's range 0, then every topic's range 1.
	//
	// The order is what makes an aborted pass worth anything, since a cursor only
	// moves over a prefix. Topic-major order -- topic 1 through its whole window,
	// then topic 2 -- meant a pass the cap stopped part-way had committed nothing
	// for the later topics, and a first run over a big backlog aborted, re-ran from
	// the beginning, and aborted in the same place every time. It is also what
	// keeps a backfilling topic from starving the others: their few ranges of new
	// items are all in the first rounds, and the catch-up runs on after them.
	type work struct {
		lane     int
		rangeIdx int
	}
	var queue []work
	for i := 0; ; i++ {
		added := false
		for li, l := range lanes {
			if i < len(l.ranges) {
				queue = append(queue, work{lane: li, rangeIdx: i})
				added = true
			}
		}
		if !added {
			break
		}
	}
	if limited && len(queue) > budget {
		queue = queue[:budget]
		result.BudgetLimited = true
	}

	result.TopicsRun = len(ready)
	result.Batches = len(queue)
	result.RangesTotal = len(queue)
	distinct := map[int64]bool{}
	for _, w := range queue {
		l := lanes[w.lane]
		r := l.ranges[w.rangeIdx]
		for _, it := range l.items[r.start:r.end] {
			distinct[it.ID] = true
		}
	}
	result.ItemsConsidered = len(distinct)
	if len(queue) == 0 {
		return result, nil
	}

	// Every (topic, batch) pair is independent, so the whole work list runs through
	// a bounded pool. Sequentially this is topics x batches round trips end to end
	// -- measured at ~25s each against a real provider, which blows §10's
	// three-minute budget several times over before the day's second topic is
	// finished.
	type outcome struct {
		matches  []store.Match
		dropped  int
		repaired bool
		lane     int
		rangeIdx int
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
				l := lanes[w.lane]
				r := l.ranges[w.rangeIdx]
				batch := l.items[r.start:r.end]
				verdicts, dropped, repaired, err := classifyBatch(ctx, client, l.topic, batch, log)
				out := outcome{
					dropped:    dropped,
					repaired:   repaired,
					err:        err,
					lane:       w.lane,
					rangeIdx:   w.rangeIdx,
					considered: len(batch),
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
							TopicID: l.topic.ID,
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
	// because whether their work counts on an aborted pass depends on how far each
	// cursor can move, which is not known until every batch has landed.
	type success struct {
		lane       int
		rangeIdx   int
		considered int
		matches    int
	}
	var successes []success

	var fatal error
	finished := 0
	for out := range results {
		finished++
		reportProgress(ctx, finished, len(queue), "batches")
		topic := lanes[out.lane].topic
		if out.err != nil {
			if errors.Is(out.err, llm.ErrCapReached) || errors.Is(out.err, context.Canceled) ||
				errors.Is(out.err, context.DeadlineExceeded) {
				// The guard stopping a run, or a shutdown. Keep draining so the
				// workers can exit, then commit what was finished and stop.
				if fatal == nil {
					fatal = out.err
				}
				continue
			}
			log.Warn("classify batch failed", "topic", topic.Title, "err", out.err)
			result.Problems = append(result.Problems,
				fmt.Sprintf("topic %q: %v", topic.Title, out.err))
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
			lane: out.lane, rangeIdx: out.rangeIdx,
			considered: out.considered, matches: len(out.matches),
		})
		lanes[out.lane].done[out.rangeIdx] = true
	}

	// A topic's cursor may only pass items that topic has judged, so it moves over
	// the unbroken prefix of its ranges and stops at the first gap.
	//
	// Advancing regardless -- which the single shared watermark once did -- left a
	// permanent hole in that topic's coverage, visible only as a topic that
	// mysteriously matches nothing. The cost of the honest version is that a topic
	// failing persistently re-classifies the same items every run. That is the
	// right way round: it is bounded by the call cap, it is loud (a problem reported
	// every run, and a zero in the hit-rate view), and it now stalls only the topic
	// that is failing -- the others' cursors are their own.
	//
	// Items are read in (fetched_at, id) order, so a prefix's last item is the
	// cursor -- see store.FormatWatermark for why a timestamp alone is not.
	prefix := make([]int, len(lanes))
	cursors := map[int64]string{}
	for li, l := range lanes {
		prefix[li] = len(l.done)
		for i, ok := range l.done {
			if !ok {
				prefix[li] = i
				break
			}
		}
		result.RangesCommitted += prefix[li]
		if prefix[li] > 0 {
			last := l.items[l.ranges[prefix[li]-1].end-1]
			cursors[l.topic.ID] = store.FormatWatermark(last.FetchedAt, last.ID)
		}
	}

	// Stats cover EVERY successful batch of a completed pass, including ones behind
	// a cursor that could not move. A partially judged pass knows exactly what each
	// topic did, and that is real work worth showing.
	//
	// The first version of this gated stats on the prefix too, and the result was
	// perverse: one topic failing on the first slice blanked the hit-rate view for
	// EVERY topic, destroying the one diagnostic that would have identified the
	// failing one. Re-classifying a slice next run does double-count it -- but it
	// double-counts numerator and denominator together, so the rate the view exists
	// to show is unchanged.
	//
	// An ABORTED pass counts only the prefixes it commits. The ranges behind them go
	// back into the next run's window, and with round-robin order they are the
	// batches still in flight when the cap landed -- so leaving them out costs a
	// handful of batches of stats, and counting them would count them twice.
	stats := map[int64]*store.TopicStat{}
	statOrder := make([]int64, 0, len(ready))
	for _, sc := range successes {
		if fatal != nil && sc.rangeIdx >= prefix[sc.lane] {
			continue
		}
		id := lanes[sc.lane].topic.ID
		st := stats[id]
		if st == nil {
			st = &store.TopicStat{TopicID: id}
			stats[id] = st
			statOrder = append(statOrder, id)
		}
		st.ItemsConsidered += sc.considered
		st.MatchesKept += sc.matches
		st.Batches++
	}

	pass := make([]store.TopicStat, 0, len(statOrder))
	for _, id := range statOrder {
		pass = append(pass, *stats[id])
	}
	if err := db.CommitClassifyPass(runDate, pass, cursors); err != nil {
		return result, err
	}

	_, pending, err := ClassifyBacklog(db, cfg)
	if err != nil {
		return result, err
	}
	result.ItemsPending = pending

	if fatal != nil {
		log.Warn("classify stopped; finished ranges committed",
			"batches", result.RangesCommitted, "of", result.RangesTotal,
			"pending", result.ItemsPending, "err", fatal)
		return result, fatal
	}

	log.Info("classify complete",
		"items", result.ItemsConsidered,
		"topics", result.TopicsRun,
		"batches", result.Batches,
		"matches", result.MatchesKept,
		"dropped", result.VerdictsDropped,
		"repairs", result.Repairs,
		"pending", result.ItemsPending,
	)
	return result, nil
}

// classifyBudget is how many classifier calls this pass may make: what the run's
// remaining budget affords after compose's share.
//
// A full first-run window across seven topics is hundreds of calls -- more than
// the seeded per-run cap of 400, before compose has spent anything. Discovering
// that by hitting the cap meant a first run over many sources never reached the
// sky. So the pass takes what the budget affords and the rest waits for the next
// run, with the cursors saying exactly where.
//
// It always allows at least one batch, so a pass makes progress even when the
// budget is nearly gone; if that overruns, the abort path commits what finished.
// The estimate counts one call per batch -- repairs and fallback hops cost more,
// and that is also what the abort path is for.
func classifyBudget(client *llm.Client, topics int) (calls int, limited bool, err error) {
	left, capped, err := client.RunBudgetLeft()
	if err != nil || !capped {
		return 0, false, err
	}
	return max(left-composeCallsPerTopic*topics, 1), true, nil
}

// classifyFloor is the oldest item a topic will catch up on: the sky's window. A
// matched item older than that cannot reach a star, so judging it would spend
// calls on nothing the owner can see.
func classifyFloor(cfg config.Runtime) string {
	days := cfg.SkyDays
	if days <= 0 {
		days = 7
	}
	at := time.Now().UTC().AddDate(0, 0, -days).Format(time.DateTime)
	return store.FormatWatermark(at, 0)
}

// TopicBacklog is how far one topic is from caught up.
type TopicBacklog struct {
	TopicID int64  `json:"topic_id"`
	Title   string `json:"title"`
	Pending int    `json:"pending"`
	// CatchingUp marks a topic behind the others -- added, resumed or given its
	// definition since they last ran -- as opposed to one simply waiting on new
	// items like everyone else.
	CatchingUp bool `json:"catching_up"`
}

// ClassifyBacklog reports, per ready topic, how many items it has yet to judge,
// and how many distinct items are waiting for at least one topic.
//
// It reads the same cursors and floor the classify pass does, so the number on
// screen is the number the next run will work through.
func ClassifyBacklog(db *store.DB, cfg config.Runtime) ([]TopicBacklog, int, error) {
	topics, err := db.ActiveTopics()
	if err != nil {
		return nil, 0, err
	}
	floor := classifyFloor(cfg)

	var ready []store.Topic
	lead := ""
	for _, t := range topics {
		if t.NeedsSetup() {
			continue
		}
		ready = append(ready, t)
		lead = store.LaterWatermark(lead, t.ClassifiedThrough)
	}

	out := make([]TopicBacklog, 0, len(ready))
	earliest := ""
	for _, t := range ready {
		start := store.LaterWatermark(t.ClassifiedThrough, floor)
		n, err := db.CountItemsSince(start)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, TopicBacklog{
			TopicID: t.ID, Title: t.Title, Pending: n,
			CatchingUp: n > 0 && t.ClassifiedThrough != lead,
		})
		if earliest == "" || store.LaterWatermark(earliest, start) == earliest {
			earliest = start
		}
	}
	if earliest == "" {
		return out, 0, nil
	}
	total, err := db.CountItemsSince(earliest)
	if err != nil {
		return nil, 0, err
	}
	return out, total, nil
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

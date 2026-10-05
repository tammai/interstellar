package pipeline

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/tammai/interstellar/internal/llm"
	"github.com/tammai/interstellar/internal/store"
)

// seedBacklog seeds n items sharing one fetched_at -- the shape a first run over
// many sources has, since ingest lands each feed's items inside one second.
func seedBacklog(t *testing.T, db *store.DB, n int) []int64 {
	t.Helper()
	src := seedSource(t, db, "Blog")
	var ids []int64
	for i := range n {
		ids = append(ids, seedItem(t, db, src, fmt.Sprintf("https://x.test/%d", i), fmt.Sprintf("Post %d", i)))
	}
	if _, err := db.Exec(`UPDATE items SET fetched_at = datetime('now', '-1 hour')`); err != nil {
		t.Fatalf("pin fetched_at: %v", err)
	}
	return ids
}

// A first run over many sources costs more than one run's budget. It used to spend
// the whole cap in classify, abort before compose, and commit nothing -- so the
// re-run started from the beginning and aborted in the same place. Now it takes
// what the budget affords, leaves compose its share, and each run carries on from
// where the last one stopped without paying for anything twice.
func TestClassifyFitsABacklogToTheBudgetAndResumes(t *testing.T) {
	db := testDB(t)
	agents := seedTopic(t, db, "Agents", "Agent harnesses.")
	weights := seedTopic(t, db, "Open weights", "Open-weight releases.")
	seedBacklog(t, db, 20)

	model := newFakeModel(t, scoreAllHigh)
	cfg := testConfig()
	cfg.ClassifyBatchSize = 2 // 10 ranges x 2 topics = 20 calls; the cap is 10

	runs := 0
	for {
		runs++
		if runs > 10 {
			t.Fatal("the backlog never drained")
		}
		client := clientForCapped(t, db, model, 10)
		res, err := Classify(context.Background(), db, client, cfg, testRunDate, quietLogger())
		if err != nil {
			t.Fatalf("run %d: Classify = %v; a budget-sized pass must not hit the cap", runs, err)
		}
		// Compose's share is still there: 2 per topic.
		if left, _, _ := client.RunBudgetLeft(); left < composeCallsPerTopic*2 {
			t.Errorf("run %d left %d calls, want at least %d for compose", runs, left, composeCallsPerTopic*2)
		}
		if res.ItemsPending == 0 {
			break
		}
		if !res.BudgetLimited {
			t.Errorf("run %d left %d pending without saying it was budget-limited", runs, res.ItemsPending)
		}
		if cfg, err = configFromDB(db); err != nil {
			t.Fatal(err)
		}
		cfg.ClassifyBatchSize = 2
	}

	if got := model.callCount(); got != 20 {
		t.Errorf("classifier made %d calls over %d runs, want exactly 20: no batch judged twice", got, runs)
	}
	for _, topic := range []int64{agents, weights} {
		if n := countRows(t, db, "SELECT count(*) FROM matches WHERE topic_id = ?", topic); n != 20 {
			t.Errorf("topic %d has %d matches, want all 20 items", topic, n)
		}
	}
}

// When the cap lands anyway -- repairs and fallback hops cost more than the
// one-call-per-batch estimate -- the ranges every topic finished are committed,
// and the next run starts after them.
func TestClassifyAbortCommitsTheRangesItFinished(t *testing.T) {
	db := testDB(t)
	agents := seedTopic(t, db, "Agents", "Agent harnesses.")
	weights := seedTopic(t, db, "Open weights", "Open-weight releases.")
	seedBacklog(t, db, 40)

	// Every first answer needs a repair, so each batch costs two calls against an
	// estimate of one, and the pass overruns its window.
	model := newFakeModel(t, func(prompt string) string {
		if !strings.Contains(prompt, "That was not valid JSON") {
			return "Sure! Here are the scores."
		}
		return scoreAllHigh(prompt)
	})
	cfg := testConfig()
	cfg.ClassifyBatchSize = 1

	res, err := Classify(context.Background(), db, clientForCapped(t, db, model, 30), cfg, testRunDate, quietLogger())
	if !errors.Is(err, llm.ErrCapReached) {
		t.Fatalf("Classify = %v, want ErrCapReached", err)
	}
	if res.RangesCommitted == 0 {
		t.Fatal("an aborted pass committed nothing; the re-run would start from the beginning")
	}

	// With a batch size of one, a topic's committed count is its cursor's item
	// index -- and its stats must say exactly that: the rest is re-judged next run.
	committed := map[int64]int{}
	for _, topic := range []int64{agents, weights} {
		got, _ := db.Topic(topic)
		n := 0
		if got.ClassifiedThrough != "" {
			_, last := store.ParseWatermark(got.ClassifiedThrough)
			n = int(last) // items are ids 1..40 in this database
		}
		committed[topic] = n
		if s := statsFor(t, db, topic); s.ItemsConsidered != n {
			t.Errorf("topic %d stats considered %d, want the %d its cursor committed", topic, s.ItemsConsidered, n)
		}
	}
	behind := min(committed[agents], committed[weights])
	if res.ItemsPending != 40-behind {
		t.Errorf("pending = %d, want %d", res.ItemsPending, 40-behind)
	}

	cfg2, err := configFromDB(db)
	if err != nil {
		t.Fatal(err)
	}
	cfg2.ClassifyBatchSize = 1
	next, err := Classify(context.Background(), db, clientFor(t, db, newFakeModel(t, scoreAllHigh)), cfg2, testRunDate, quietLogger())
	if err != nil {
		t.Fatalf("second Classify: %v", err)
	}
	if next.ItemsConsidered != 40-behind {
		t.Errorf("re-run considered %d items, want the %d not yet committed", next.ItemsConsidered, 40-behind)
	}
	if next.ItemsPending != 0 {
		t.Errorf("re-run left %d pending, want 0", next.ItemsPending)
	}
}

// The watermark used to be fetched_at alone. A topic failing part-way through a
// same-second group moved it to that second, and `fetched_at > mark` then skipped
// the rest of the group forever.
func TestClassifyResumesInsideASameSecondGroup(t *testing.T) {
	db := testDB(t)
	seedTopic(t, db, "Agents", "Agent harnesses.")
	broken := seedTopic(t, db, "Broken", "A definition the model chokes on.")
	ids := seedBacklog(t, db, 6)

	failOn := fmt.Sprintf("item_id: %d", ids[2])
	failing := true
	model := newFakeModel(t, func(prompt string) string {
		if failing && strings.Contains(prompt, "TOPIC: Broken") && strings.Contains(prompt, failOn) {
			return "I cannot do that."
		}
		return scoreAllHigh(prompt)
	})
	cfg := testConfig()
	cfg.ClassifyBatchSize = 2

	first, err := Classify(context.Background(), db, clientFor(t, db, model), cfg, testRunDate, quietLogger())
	if err != nil {
		t.Fatalf("Classify: %v", err)
	}
	// Broken's cursor stops at the end of range 0 -- inside the same-second group.
	got, _ := db.Topic(broken)
	if _, last := store.ParseWatermark(got.ClassifiedThrough); last != ids[1] {
		t.Fatalf("Broken's cursor = %q, want it at item %d: range 1 failed", got.ClassifiedThrough, ids[1])
	}
	if first.ItemsPending != 4 {
		t.Errorf("pending = %d, want the 4 Broken has not judged", first.ItemsPending)
	}

	cfg2, err := configFromDB(db)
	if err != nil {
		t.Fatal(err)
	}
	cfg2.ClassifyBatchSize = 2
	failing = false
	second, err := Classify(context.Background(), db, clientFor(t, db, model), cfg2, testRunDate, quietLogger())
	if err != nil {
		t.Fatalf("second Classify: %v", err)
	}
	if second.ItemsConsidered != 4 {
		t.Errorf("second pass considered %d items, want the 4 after the committed range", second.ItemsConsidered)
	}
	if n := countRows(t, db, "SELECT count(*) FROM matches WHERE topic_id = ?", broken); n != 6 {
		t.Errorf("Broken has %d matches, want all 6 items", n)
	}
}

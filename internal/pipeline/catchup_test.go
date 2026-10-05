package pipeline

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/tammai/interstellar/internal/store"
)

// catchupRig is a runner over a database with two topics and a classifier backlog,
// pointed at a fake model, with no sources -- so a run spends its calls on the
// backlog and nothing else.
type catchupRig struct {
	db     *store.DB
	model  *fakeModel
	runner *Runner
}

func newCatchupRig(t *testing.T, items int, settings map[string]string, reply func(string) string) *catchupRig {
	t.Helper()
	isolateFromRealKeys(t)
	db := testDB(t)
	src := seedSource(t, db, "Blog")
	if _, err := db.Exec(`UPDATE sources SET active = 0`); err != nil {
		t.Fatal(err)
	}
	seedTopic(t, db, "Agents", "Agent harnesses.")
	seedTopic(t, db, "Weights", "Open-weight releases.")
	for i := range items {
		seedItem(t, db, src, fmt.Sprintf("https://x.test/%d", i), fmt.Sprintf("Post %d", i))
	}

	model := newFakeModel(t, reply)
	all := map[string]string{
		store.KeyProviders:         fmt.Sprintf(`[{"name":"fake","base_url":%q,"api_key_env":""}]`, model.URL),
		store.KeyClassifierModel:   "fake/classifier",
		store.KeySynthesizerModel:  "fake/synth",
		store.KeyChatModel:         "fake/chat",
		store.KeyClassifyBatchSize: "2",
		store.KeyMaxCallsPerDay:    "0",
		store.KeyTranscriptsPerRun: "0",
	}
	for k, v := range settings {
		all[k] = v
	}
	if err := db.SetSettings(all); err != nil {
		t.Fatal(err)
	}

	runner := NewRunner(db, testEnv(), quietLogger())
	runner.catchupDelay = 10 * time.Millisecond
	t.Cleanup(runner.Stop)
	return &catchupRig{db: db, model: model, runner: runner}
}

// settle waits until the runner is idle with no catch-up waiting -- the chain has
// ended, one way or another.
func (c *catchupRig) settle(t *testing.T) State {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		st := c.runner.State()
		if !st.Running() && (st.Catchup == nil || st.Catchup.NextAt == nil) {
			return st
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("the catch-up chain never settled: %+v", c.runner.State())
	return State{}
}

func classifierCalls(m *fakeModel) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for _, p := range m.prompts {
		// Compose prompts name the topic too; they are the ones listing matches.
		if strings.Contains(p, "TOPIC:") && !strings.Contains(p, "MATCHED ITEMS") {
			n++
		}
	}
	return n
}

// The point of the feature: one click, and the backlog drains by itself without
// any batch judged twice. 20 items in batches of 2 across two topics is 20 batches;
// a cap of 8 leaves 8 - 2*2 = 4 per run after compose's share, so five runs.
func TestCatchupRunsDrainTheBacklog(t *testing.T) {
	rig := newCatchupRig(t, 20, map[string]string{store.KeyMaxCallsPerRun: "8"}, routeByRole)

	if err := rig.runner.RunNow(context.Background(), "test"); err != nil {
		t.Fatalf("RunNow: %v", err)
	}
	final := rig.settle(t)

	if final.Classify == nil || final.Classify.ItemsPending != 0 {
		t.Fatalf("chain ended with a backlog: %+v", final.Classify)
	}
	if final.Catchup == nil || final.Catchup.Round < 4 {
		t.Errorf("catch-up = %+v, want at least four catch-up rounds after the first run", final.Catchup)
	}
	if final.Catchup != nil && final.Catchup.Ended != "" {
		t.Errorf("a chain that caught up reported ending early: %q", final.Catchup.Ended)
	}
	if got := classifierCalls(rig.model); got != 20 {
		t.Errorf("classifier made %d calls, want exactly 20: no batch judged twice", got)
	}
	if final.Ingest != nil {
		t.Error("a catch-up run ingested; it should reuse the run before it")
	}
}

func TestCatchupRespectsTheSwitch(t *testing.T) {
	rig := newCatchupRig(t, 20, map[string]string{
		store.KeyMaxCallsPerRun: "8",
		store.KeyCatchupEnabled: "0",
	}, routeByRole)

	if err := rig.runner.RunNow(context.Background(), "test"); err != nil {
		t.Fatalf("RunNow: %v", err)
	}
	time.Sleep(100 * time.Millisecond) // ten delays' worth
	st := rig.runner.State()
	if st.Catchup != nil {
		t.Errorf("catch-up = %+v with the switch off", st.Catchup)
	}
	if got := classifierCalls(rig.model); got != 4 {
		t.Errorf("classifier made %d calls, want the 4 one run affords", got)
	}
}

// The daily cap bounds the whole chain: once what is left cannot classify and
// still compose, it pauses and says so, rather than spending the remainder on a
// run that produces nothing.
func TestCatchupPausesWhenTheDayIsNearlySpent(t *testing.T) {
	rig := newCatchupRig(t, 20, map[string]string{
		store.KeyMaxCallsPerRun: "8",
		store.KeyMaxCallsPerDay: "14",
	}, routeByRole)

	if err := rig.runner.RunNow(context.Background(), "test"); err != nil {
		t.Fatalf("RunNow: %v", err)
	}
	final := rig.settle(t)

	if final.Catchup == nil || !strings.Contains(final.Catchup.Ended, "daily cap") {
		t.Fatalf("catch-up = %+v, want it paused on the daily cap", final.Catchup)
	}
	if final.Classify == nil || final.Classify.ItemsPending == 0 {
		t.Error("the backlog emptied; the test is not exercising the cap")
	}
}

// A run that finishes no batch would be repeated forever by a naive chain -- a
// topic the model cannot answer, the same items every minute until the day's
// budget is gone.
func TestCatchupStopsWhenARunMakesNoProgress(t *testing.T) {
	rig := newCatchupRig(t, 20, map[string]string{store.KeyMaxCallsPerRun: "40"},
		func(string) string { return "I cannot help with that." })

	if err := rig.runner.RunNow(context.Background(), "test"); err != nil {
		t.Fatalf("RunNow: %v", err)
	}
	final := rig.settle(t)

	if final.Catchup == nil || !strings.Contains(final.Catchup.Ended, "could not finish any batch") {
		t.Fatalf("catch-up = %+v, want it stopped for lack of progress", final.Catchup)
	}
	if final.Catchup.Round != 0 {
		t.Errorf("round = %d; the chain should never have started", final.Catchup.Round)
	}
}

// A per-run cap landing mid-classify is a pause once a catch-up is on its way: the
// chain carries on, and the banner telling the owner to raise the cap is cleared,
// kept only as the reason the last run paused.
func TestCatchupCarriesOnAfterARunCapStop(t *testing.T) {
	// Prose first, valid JSON on the repair: every batch costs two calls against an
	// estimate of one, so each run overruns its classify budget and hits the cap.
	reply := func(prompt string) string {
		if strings.Contains(prompt, "MATCHED ITEMS") {
			return oneStory(prompt)
		}
		if !strings.Contains(prompt, "That was not valid JSON") {
			return "Sure, here you go."
		}
		return scoreAllHigh(prompt)
	}
	rig := newCatchupRig(t, 20, map[string]string{store.KeyMaxCallsPerRun: "12"}, reply)

	if err := rig.runner.RunNow(context.Background(), "test"); err != nil {
		t.Fatalf("RunNow: %v", err)
	}
	final := rig.settle(t)

	if final.Classify == nil || final.Classify.ItemsPending != 0 {
		t.Fatalf("chain ended with a backlog: classify=%+v catchup=%+v banner=%q",
			final.Classify, final.Catchup, final.Banner)
	}
	if final.Catchup == nil || final.Catchup.Round == 0 {
		t.Errorf("catch-up = %+v; a run-cap stop should have chained", final.Catchup)
	}
}

// Stop cancels a catch-up that is waiting, as well as one that is running: a
// shutdown must not be followed a minute later by a run against a closed database.
func TestStopCancelsAWaitingCatchup(t *testing.T) {
	rig := newCatchupRig(t, 20, map[string]string{store.KeyMaxCallsPerRun: "8"}, routeByRole)
	rig.runner.catchupDelay = 150 * time.Millisecond

	if err := rig.runner.RunNow(context.Background(), "test"); err != nil {
		t.Fatalf("RunNow: %v", err)
	}
	if st := rig.runner.State(); st.Catchup == nil || st.Catchup.NextAt == nil {
		t.Fatalf("no catch-up was scheduled: %+v", st.Catchup)
	}
	rig.runner.Stop()
	time.Sleep(400 * time.Millisecond)

	if got := classifierCalls(rig.model); got != 4 {
		t.Errorf("classifier made %d calls after Stop, want only the first run's 4", got)
	}
}

package pipeline

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/tammai/interstellar/internal/config"
	"github.com/tammai/interstellar/internal/store"
)

// isolateFromRealKeys clears provider credentials so a developer with a key
// exported in their shell cannot make these tests reach -- or bill -- a real
// provider. Only cmd/verify-classifier is allowed to make live calls.
func isolateFromRealKeys(t *testing.T) {
	t.Helper()
	for _, name := range []string{
		"DEEPSEEK_API_KEY", "ANTHROPIC_API_KEY", "GOOGLE_API_KEY",
		"OPENROUTER_API_KEY", "LITELLM_API_KEY",
	} {
		t.Setenv(name, "")
	}
}

func testEnv() config.Env {
	return config.Env{Port: "8422", Location: time.UTC}
}

func TestRunnerReportsStagesAndFinishesIdle(t *testing.T) {
	isolateFromRealKeys(t)
	db := testDB(t)
	if err := db.Bootstrap(); err != nil {
		t.Fatal(err)
	}
	// No network: point every seeded source somewhere that fails fast.
	if _, err := db.Exec(`UPDATE sources SET config = '{"url":"http://127.0.0.1:1/x"}', kind = 'rss'`); err != nil {
		t.Fatal(err)
	}

	runner := NewRunner(db, testEnv(), quietLogger())
	if runner.State().Stage != StageIdle {
		t.Fatalf("initial stage = %q, want idle", runner.State().Stage)
	}

	if err := runner.RunNow(context.Background(), "test"); err != nil {
		t.Fatalf("RunNow: %v", err)
	}

	final := runner.State()
	if final.Stage != StageIdle {
		t.Errorf("final stage = %q, want idle", final.Stage)
	}
	if final.StartedAt == nil || final.EndedAt == nil {
		t.Error("run did not record start and end times")
	}
	if final.Ingest == nil {
		t.Error("no ingest result recorded")
	}
}

// With no key configured the run must stop at a readiness banner that names the
// missing variable -- not fail obscurely, and not attempt a call.
func TestRunnerStopsWithReadinessBanner(t *testing.T) {
	isolateFromRealKeys(t)
	db := testDB(t)
	if err := db.Bootstrap(); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE sources SET active = 0`); err != nil {
		t.Fatal(err)
	}

	runner := NewRunner(db, testEnv(), quietLogger())
	if err := runner.RunNow(context.Background(), "test"); err != nil {
		t.Fatalf("RunNow: %v", err)
	}

	banner := runner.State().Banner
	if banner == "" {
		t.Fatal("no banner when the classifier cannot run")
	}
	if !strings.Contains(banner, "OPENROUTER_API_KEY") {
		t.Errorf("banner = %q, should name the missing variable", banner)
	}
	// Ingest still ran: items accumulating with no model configured is a more
	// useful first-run state than nothing at all.
	if runner.State().Ingest == nil {
		t.Error("ingest was skipped; it does not need a model")
	}
}

func TestRunnerRefusesConcurrentRuns(t *testing.T) {
	isolateFromRealKeys(t)
	db := testDB(t)
	if err := db.Bootstrap(); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE sources SET active = 0`); err != nil {
		t.Fatal(err)
	}

	runner := NewRunner(db, testEnv(), quietLogger())

	// Hold the runner in a run by starting one and immediately checking.
	blocked := make(chan struct{})
	go func() {
		runner.RunNow(context.Background(), "first")
		close(blocked)
	}()

	// Either we catch it running and get a refusal, or it already finished --
	// both are correct, so only assert when we actually observe overlap.
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if runner.State().Running() {
			if err := runner.RunNow(context.Background(), "second"); err != ErrAlreadyRunning {
				t.Errorf("concurrent run err = %v, want ErrAlreadyRunning", err)
			}
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	<-blocked
}

// §10: a run cancelled mid-flight must leave the database usable and the next run
// clean. Every stage is idempotent, so this is about state, not recovery logic.
func TestRunnerCancellationLeavesCleanState(t *testing.T) {
	isolateFromRealKeys(t)
	db := testDB(t)
	if err := db.Bootstrap(); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE sources SET config = '{"url":"http://127.0.0.1:1/x"}', kind = 'rss'`); err != nil {
		t.Fatal(err)
	}

	runner := NewRunner(db, testEnv(), quietLogger())

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancelled before it starts: the harshest timing
	if err := runner.RunNow(ctx, "cancelled"); err != nil {
		t.Fatalf("RunNow: %v", err)
	}

	if runner.State().Stage != StageIdle {
		t.Errorf("stage = %q after cancellation, want idle", runner.State().Stage)
	}

	// The database is still readable and the next run is unblocked.
	if _, err := db.RecentSignals(7); err != nil {
		t.Errorf("database unusable after cancellation: %v", err)
	}
	if err := runner.RunNow(context.Background(), "next"); err != nil {
		t.Errorf("next run refused: %v", err)
	}
}

func TestRunnerSurfacesCapBannerWithGuidance(t *testing.T) {
	isolateFromRealKeys(t)
	db := testDB(t)
	if err := db.Bootstrap(); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE sources SET active = 0`); err != nil {
		t.Fatal(err)
	}
	// A key so readiness passes, and a fake provider so nothing real is called.
	t.Setenv("FAKE_KEY", "sk-fake")
	if err := db.SetSettings(map[string]string{
		store.KeyProviders:        `[{"name":"fake","base_url":"http://127.0.0.1:1/v1","api_key_env":"FAKE_KEY"}]`,
		store.KeyClassifierModel:  "fake/m",
		store.KeySynthesizerModel: "fake/m",
		store.KeyMaxCallsPerDay:   "0",
		store.KeyMaxCallsPerRun:   "1",
	}); err != nil {
		t.Fatal(err)
	}
	// Something to classify, so the cap is actually reachable.
	src := seedSource(t, db, "Blog")
	seedItem(t, db, src, "https://x.test/a", "A post")
	seedTopic(t, db, "T", "A definition.")
	seedTopic(t, db, "U", "Another definition.")

	runner := NewRunner(db, testEnv(), quietLogger())
	if err := runner.RunNow(context.Background(), "cap test"); err != nil {
		t.Fatalf("RunNow: %v", err)
	}

	banner := runner.State().Banner
	if banner == "" {
		t.Fatal("no banner after the run cap was hit")
	}
	// The banner has to tell the owner what to do, not just that it stopped.
	if !strings.Contains(banner, "cap") || !strings.Contains(banner, "Settings") {
		t.Errorf("banner = %q; should name the cap and where to change it", banner)
	}
}

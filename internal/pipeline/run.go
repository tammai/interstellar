package pipeline

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/tammai/interstellar/internal/config"
	"github.com/tammai/interstellar/internal/llm"
	"github.com/tammai/interstellar/internal/store"
)

// Stage is where a run currently is. The UI polls this to show progress (§10).
type Stage string

const (
	StageIdle        Stage = "idle"
	StageIngesting   Stage = "ingesting"
	StageClassifying Stage = "classifying"
	StageComposing   Stage = "composing"
)

// State is a snapshot of the runner, safe to serialize straight to the API.
type State struct {
	Stage     Stage      `json:"stage"`
	StartedAt *time.Time `json:"started_at,omitempty"`
	EndedAt   *time.Time `json:"ended_at,omitempty"`

	// Banner is the reason a run stopped abnormally -- a breached call cap, a
	// provider that never came back. Empty when the last run was clean. §10
	// requires the cap to "surface a UI banner", and this is it.
	Banner string `json:"banner,omitempty"`

	Ingest     *IngestResult     `json:"ingest,omitempty"`
	Classify   *ClassifyResult   `json:"classify,omitempty"`
	Synthesize *SynthesizeResult `json:"synthesize,omitempty"`
	LLMCalls   int               `json:"llm_calls"`
}

// Running reports whether a run is in flight.
func (s State) Running() bool { return s.Stage != StageIdle }

// ErrAlreadyRunning is returned when a refresh is requested mid-run. One run at
// a time: two concurrent classify passes would double the bill for the same work.
var ErrAlreadyRunning = errors.New("a run is already in progress")

// Runner owns run state and serializes runs.
//
// State lives in memory on purpose. A run is a process-lifetime concept, so a
// restart legitimately means "no run in progress" -- which is exactly the §10
// behavior "kill container mid-run -> restart -> next run clean". Persisting it
// would require reconciling a run that no longer exists.
type Runner struct {
	db  *store.DB
	env config.Env
	log *slog.Logger

	mu    sync.Mutex
	state State
	// cancel stops an in-flight run on shutdown.
	cancel context.CancelFunc
}

// NewRunner builds a runner in the idle state.
func NewRunner(db *store.DB, env config.Env, log *slog.Logger) *Runner {
	return &Runner{db: db, env: env, log: log, state: State{Stage: StageIdle}}
}

// State returns a snapshot.
func (r *Runner) State() State {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.state
}

// Start launches a run in the background, refusing if one is already going.
func (r *Runner) Start(reason string) error {
	r.mu.Lock()
	if r.state.Running() {
		r.mu.Unlock()
		return ErrAlreadyRunning
	}

	now := time.Now()
	ctx, cancel := context.WithCancel(context.Background())
	r.cancel = cancel
	r.state = State{Stage: StageIngesting, StartedAt: &now}
	r.mu.Unlock()

	r.log.Info("run started", "reason", reason)

	go func() {
		defer cancel()
		r.execute(ctx)
	}()
	return nil
}

// Stop cancels an in-flight run and waits briefly for it to notice.
func (r *Runner) Stop() {
	r.mu.Lock()
	cancel := r.cancel
	r.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// RunNow executes a run synchronously. Used by the scheduler, which already has
// its own goroutine, and by tests.
func (r *Runner) RunNow(ctx context.Context, reason string) error {
	r.mu.Lock()
	if r.state.Running() {
		r.mu.Unlock()
		return ErrAlreadyRunning
	}
	now := time.Now()
	runCtx, cancel := context.WithCancel(ctx)
	r.cancel = cancel
	r.state = State{Stage: StageIngesting, StartedAt: &now}
	r.mu.Unlock()

	defer cancel()
	r.log.Info("run started", "reason", reason)
	r.execute(runCtx)
	return nil
}

// execute is the pipeline: ingest, classify, compose.
//
// Ingest runs even with no model configured -- items accumulate and the sky shows
// empty constellations, which is a more useful first-run state than nothing at
// all. Only the LLM stages need readiness.
func (r *Runner) execute(ctx context.Context) {
	defer func() {
		r.mu.Lock()
		ended := time.Now()
		r.state.Stage = StageIdle
		r.state.EndedAt = &ended
		r.mu.Unlock()
	}()

	cfg, err := config.Load(r.db)
	if err != nil {
		r.fail("could not read settings", err)
		return
	}

	// --- ingest ---
	r.setStage(StageIngesting)
	ingest, err := Ingest(ctx, r.db, cfg, r.log)
	r.mu.Lock()
	r.state.Ingest = &ingest
	r.mu.Unlock()
	if err != nil {
		r.fail("ingest stopped", err)
		return
	}

	// --- the LLM stages ---
	settings, err := r.db.Settings()
	if err != nil {
		r.fail("could not read settings", err)
		return
	}
	llmCfg, err := llm.LoadConfig(settings)
	if err != nil {
		// A malformed provider registry or model ref. Surfaced as a banner
		// rather than a log line, because only the owner can fix it.
		r.fail("model configuration problem", err)
		return
	}
	if !llmCfg.Ready() {
		r.finishWithBanner(readinessBanner(llmCfg))
		return
	}

	runDate := store.RunDate(time.Now().In(r.env.Location))
	client := llm.NewClient(llmCfg, r.db, runDate, r.log)

	r.setStage(StageClassifying)
	classify, err := Classify(ctx, r.db, client, cfg, r.log)
	r.mu.Lock()
	r.state.Classify = &classify
	r.state.LLMCalls = client.Guard().UsedThisRun()
	r.mu.Unlock()
	if err != nil {
		r.stopReason(client, "classification stopped", err)
		return
	}

	r.setStage(StageComposing)
	synth, err := Synthesize(ctx, r.db, client, cfg, runDate, r.log)
	r.mu.Lock()
	r.state.Synthesize = &synth
	r.state.LLMCalls = client.Guard().UsedThisRun()
	r.mu.Unlock()
	if err != nil {
		r.stopReason(client, "composition stopped", err)
		return
	}

	r.log.Info("run complete",
		"items_new", ingest.ItemsNew,
		"matches", classify.MatchesKept,
		"signals", synth.SignalsWritten,
		"llm_calls", client.Guard().UsedThisRun(),
	)
}

func (r *Runner) setStage(s Stage) {
	r.mu.Lock()
	r.state.Stage = s
	r.mu.Unlock()
}

// stopReason turns a stage failure into a banner, preferring the spend guard's
// own words when it was the guard that stopped things.
func (r *Runner) stopReason(client *llm.Client, what string, err error) {
	if errors.Is(err, llm.ErrCapReached) {
		if reason := client.Guard().StoppedFor(); reason != "" {
			r.finishWithBanner("Run stopped: " + reason + ". Raise the cap in Settings to continue.")
			return
		}
	}
	if errors.Is(err, context.Canceled) {
		r.log.Info("run cancelled", "stage", what)
		return
	}
	r.fail(what, err)
}

func (r *Runner) fail(what string, err error) {
	r.log.Error(what, "err", err)
	r.finishWithBanner(what + ": " + err.Error())
}

func (r *Runner) finishWithBanner(banner string) {
	r.mu.Lock()
	r.state.Banner = banner
	r.mu.Unlock()
}

// TestResult is one canned call through a role, for the Settings test button.
type TestResult struct {
	Reply     string
	Provider  string
	Model     string
	TokensIn  int
	TokensOut int
	LatencyMS int64
}

// TestCall runs a trivial prompt through a role's configured chain.
//
// This is §3.1's "any swap is verifiable in 10 seconds" promise. It uses the
// same client and the same chain a run would, so a green result means the run
// will work -- a test that took a different code path would be worth nothing.
//
// It is logged under the test role: visible in the cost log, exempt from the
// per-run cap (verifying a swap must not be blocked by a run's budget) but still
// subject to the daily one.
func (r *Runner) TestCall(ctx context.Context, cfg llm.Config, role llm.Role) (TestResult, error) {
	runDate := store.RunDate(time.Now().In(r.env.Location))
	client := llm.NewClient(cfg, r.db, runDate, r.log)

	// Ask for something with an unmistakable right answer, so a provider that
	// returns 200 with junk is still visibly wrong.
	req := llm.Request{
		Messages: []llm.Message{
			{Role: "user", Content: `Reply with exactly this JSON and nothing else: {"ok":true}`},
		},
		MaxTokens: 64,
	}

	start := time.Now()
	resp, err := client.CompleteAs(ctx, role, llm.RoleTest, req)
	elapsed := time.Since(start).Milliseconds()
	if err != nil {
		return TestResult{LatencyMS: elapsed}, err
	}

	return TestResult{
		Reply:     resp.Text,
		Provider:  resp.Provider,
		Model:     resp.Model,
		TokensIn:  resp.Usage.TokensIn,
		TokensOut: resp.Usage.TokensOut,
		LatencyMS: elapsed,
	}, nil
}

// readinessBanner explains what setup is missing, naming the specific thing.
func readinessBanner(cfg llm.Config) string {
	for _, st := range cfg.Readiness() {
		if !st.Ready {
			return "Cannot run the " + st.Role + ": " + st.Problem + "."
		}
	}
	return ""
}

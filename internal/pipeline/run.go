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
	StageIdle         Stage = "idle"
	StageIngesting    Stage = "ingesting"
	StageTranscribing Stage = "transcribing"
	StageClassifying  Stage = "classifying"
	StageComposing    Stage = "composing"
	StageExtracting   Stage = "extracting"
	StagePushing      Stage = "pushing"
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
	// StoppedAt is the stage the banner was raised in, so the Schedule screen can
	// mark that stage rather than guess from which results are missing.
	StoppedAt Stage `json:"stopped_at,omitempty"`

	Ingest     *IngestResult     `json:"ingest,omitempty"`
	Transcribe *TranscribeResult `json:"transcribe,omitempty"`
	Classify   *ClassifyResult   `json:"classify,omitempty"`
	Synthesize *SynthesizeResult `json:"synthesize,omitempty"`
	Extract    *ExtractResult    `json:"extract,omitempty"`
	Push       *PushResult       `json:"push,omitempty"`
	LLMCalls   int               `json:"llm_calls"`

	// Progress is the current stage's count, live while it runs, and
	// StageStartedAt when that stage began. Both exist for the Schedule screen,
	// which draws a run as it happens rather than only once it is over: a first
	// run over many sources takes minutes, and "classifying…" for minutes with no
	// number beside it reads as hung.
	Progress       *Progress  `json:"progress,omitempty"`
	StageStartedAt *time.Time `json:"stage_started_at,omitempty"`
	// CallCapRun is the per-run cap this run is spending against, fixed when the
	// run's client is built.
	CallCapRun int `json:"call_cap_run,omitempty"`
	// StoppedBy is which cap stopped the run, when one did ("run_cap", "day_cap").
	StoppedBy llm.StopKind `json:"stopped_by,omitempty"`

	// Catchup is this run's place in a chain of catch-up runs: nil for a run
	// nothing chained to or from. See Runner.afterRun.
	Catchup *Catchup `json:"catchup,omitempty"`
}

// Catchup describes the catch-up chain a run belongs to.
type Catchup struct {
	// Round is 0 for the scheduled or manual run that started the chain, and n for
	// the nth catch-up run after it.
	Round int `json:"round"`
	// NextAt is when the next catch-up run starts, while one is waiting.
	NextAt *time.Time `json:"next_at,omitempty"`
	// Ended says why the chain stopped short of caught up -- the daily cap, a run
	// that made no progress. Empty when it simply finished, or never needed to run.
	Ended string `json:"ended,omitempty"`
	// LastStop is the cap the previous run stopped at, kept because a chain that
	// carries on clears the banner: the run did not fail, it paused.
	LastStop string `json:"last_stop,omitempty"`
}

// defaultCatchupDelay is the pause between catch-up runs: long enough for a
// provider's rate limit to recover, short enough that a first-run backlog drains
// in minutes rather than across the next several mornings.
const defaultCatchupDelay = time.Minute

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
	// guard is the running client's spend guard, so a progress report can carry
	// the live call count without every stage reporting it. Nil outside a run.
	guard *llm.Guard
	// cancel stops an in-flight run on shutdown.
	cancel context.CancelFunc
	// timer is the pending catch-up run, if one is waiting.
	timer *time.Timer
	// catchupDelay is the pause before a catch-up run; a field so tests need not
	// wait a minute.
	catchupDelay time.Duration
}

// NewRunner builds a runner in the idle state.
func NewRunner(db *store.DB, env config.Env, log *slog.Logger) *Runner {
	return &Runner{db: db, env: env, log: log, state: State{Stage: StageIdle},
		catchupDelay: defaultCatchupDelay}
}

// State returns a snapshot.
func (r *Runner) State() State {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.state
}

// Start launches a run in the background, refusing if one is already going.
func (r *Runner) Start(reason string) error {
	ctx, cancel, err := r.begin(context.Background(), 0)
	if err != nil {
		return err
	}
	r.log.Info("run started", "reason", reason)

	go func() {
		defer cancel()
		r.execute(ctx, false)
		r.afterRun(ctx, 0)
	}()
	return nil
}

// Stop cancels an in-flight run and any catch-up run waiting to start. It does
// not disable the runner: the next scheduled or manual run starts as usual.
func (r *Runner) Stop() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.stopTimerLocked()
	if r.state.Catchup != nil {
		r.state.Catchup.NextAt = nil // no countdown to a run that will not happen
	}
	// Under the lock, so a run finishing at this moment sees its context
	// cancelled in afterRun and does not schedule a catch-up behind our back.
	if r.cancel != nil {
		r.cancel()
	}
}

// RunNow executes a run synchronously. Used by the scheduler, which already has
// its own goroutine, and by tests.
func (r *Runner) RunNow(ctx context.Context, reason string) error {
	runCtx, cancel, err := r.begin(ctx, 0)
	if err != nil {
		return err
	}
	defer cancel()
	r.log.Info("run started", "reason", reason)
	r.execute(runCtx, false)
	r.afterRun(runCtx, 0)
	return nil
}

// begin claims the runner for a run. round > 0 marks a catch-up run, which skips
// ingest; any catch-up still waiting is cancelled, because the run starting now
// does its work and decides afresh whether another is needed.
func (r *Runner) begin(parent context.Context, round int) (context.Context, context.CancelFunc, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.state.Running() {
		return nil, nil, ErrAlreadyRunning
	}
	r.stopTimerLocked()

	now := time.Now()
	ctx, cancel := context.WithCancel(parent)
	r.cancel = cancel
	stage := StageIngesting
	if round > 0 {
		stage = StageClassifying
	}
	r.state = State{Stage: stage, StartedAt: &now, StageStartedAt: &now}
	if round > 0 {
		r.state.Catchup = &Catchup{Round: round}
	}
	return ctx, cancel, nil
}

func (r *Runner) stopTimerLocked() {
	if r.timer != nil {
		r.timer.Stop()
		r.timer = nil
	}
}

// startCatchup runs catch-up round n. A run already in progress -- a scheduled
// one, a click -- wins, and decides for itself whether another is needed.
func (r *Runner) startCatchup(round int) {
	ctx, cancel, err := r.begin(context.Background(), round)
	if err != nil {
		r.log.Info("catch-up run skipped", "round", round, "err", err)
		return
	}
	defer cancel()
	r.log.Info("catch-up run started", "round", round)
	r.execute(ctx, true)
	r.afterRun(ctx, round)
}

// afterRun decides whether the run just finished should be followed by a
// catch-up run, and schedules it.
//
// A run whose classify pass was cut short by its call budget leaves a backlog
// that would otherwise wait for the next scheduled run -- twice a day, so a first
// run over many sources took days to reach the sky. This carries on instead, a
// minute at a time, until the backlog is empty or the day's budget is spent.
//
// It stops on its own whenever another run could not help: the daily cap is
// nearly spent, the last run made no progress (a topic failing on every batch
// would otherwise loop on the same items), or the run stopped on something other
// than its per-run cap -- a dead provider is not fixed by asking again. The daily
// cap is what bounds the spend, and it already survives restarts.
func (r *Runner) afterRun(ctx context.Context, round int) {
	carryOn, ended := r.shouldCatchUp()

	r.mu.Lock()
	defer r.mu.Unlock()
	if ctx.Err() != nil {
		return // stopped or shutting down; never chain behind a Stop
	}
	if !carryOn {
		if ended != "" {
			if r.state.Catchup == nil {
				r.state.Catchup = &Catchup{Round: round}
			}
			r.state.Catchup.Ended = ended
		}
		return
	}

	next := time.Now().Add(r.catchupDelay)
	if r.state.Catchup == nil {
		r.state.Catchup = &Catchup{Round: round}
	}
	r.state.Catchup.NextAt = &next
	// A per-run cap that stopped this run is a pause, not a failure, once a
	// catch-up is on its way: the banner would tell the owner to raise the cap
	// for a problem that resolves itself in a minute.
	if r.state.StoppedBy == llm.StopRunCap {
		r.state.Catchup.LastStop = r.state.Banner
		r.state.Banner = ""
	}
	r.log.Info("catch-up run scheduled", "round", round+1, "at", next.Format(time.TimeOnly))
	r.timer = time.AfterFunc(r.catchupDelay, func() { r.startCatchup(round + 1) })
}

// shouldCatchUp reads the finished run and the day's spend. ended is the reason
// to show when the chain stops short of caught up; empty when there is nothing
// worth saying -- caught up, switched off, or a banner already explaining why.
func (r *Runner) shouldCatchUp() (carryOn bool, ended string) {
	st := r.State()
	c := st.Classify
	if c == nil || c.ItemsPending == 0 {
		return false, ""
	}
	cfg, err := config.Load(r.db)
	if err != nil || !cfg.CatchupEnabled {
		return false, ""
	}

	switch {
	case st.StoppedBy == llm.StopDayCap:
		return false, "Paused: the daily cap is spent. The next scheduled run carries on."
	case st.Banner != "" && st.StoppedBy != llm.StopRunCap:
		return false, "" // a real failure; its banner says what to fix
	case c.RangesCommitted == 0:
		return false, "Stopped: the last run could not finish any batch, so another would repeat it. See Classify's problems."
	}

	if cfg.MaxCallsPerDay > 0 {
		used, err := r.db.LLMCallsOn(store.RunDate(time.Now().In(r.env.Location)))
		if err != nil {
			return false, ""
		}
		// Not enough left to classify anything and still compose: another run
		// would spend the remainder producing nothing on the sky.
		if cfg.MaxCallsPerDay-used <= composeCallsPerTopic*c.TopicsRun {
			return false, "Paused: the daily cap is nearly spent. The next scheduled run carries on."
		}
	}
	return true, ""
}

// execute is the pipeline: ingest, classify, compose.
//
// Ingest runs even with no model configured -- items accumulate and the sky shows
// empty constellations, which is a more useful first-run state than nothing at
// all. Only the LLM stages need readiness.
func (r *Runner) execute(ctx context.Context, catchup bool) {
	defer func() {
		r.mu.Lock()
		ended := time.Now()
		r.state.Stage = StageIdle
		r.state.EndedAt = &ended
		r.state.Progress = nil
		r.state.StageStartedAt = nil
		r.guard = nil
		r.mu.Unlock()
	}()

	ctx = withProgress(ctx, func(p Progress) {
		r.mu.Lock()
		defer r.mu.Unlock()
		r.state.Progress = &p
		if r.guard != nil {
			r.state.LLMCalls = r.guard.UsedThisRun()
		}
	})

	cfg, err := config.Load(r.db)
	if err != nil {
		r.fail("could not read settings", err)
		return
	}

	// --- ingest ---
	//
	// A catch-up run skips it: the run before it fetched every feed a minute ago,
	// and the work left is the classifier backlog, not new items.
	var ingest IngestResult
	if !catchup {
		r.setStage(StageIngesting)
		ingest, err = Ingest(ctx, r.db, cfg, r.log)
		r.mu.Lock()
		r.state.Ingest = &ingest
		r.mu.Unlock()
		if err != nil {
			r.fail("ingest stopped", err)
			return
		}
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
		// Recorded against the first model stage, which is the one that could not
		// start: ingest itself finished.
		r.setStage(StageTranscribing)
		r.finishWithBanner(readinessBanner(llmCfg))
		return
	}

	runDate := store.RunDate(time.Now().In(r.env.Location))
	client := llm.NewClient(llmCfg, r.db, runDate, r.log)
	r.mu.Lock()
	r.guard = client.Guard()
	r.state.CallCapRun = llmCfg.MaxCallsPerRun
	r.mu.Unlock()

	// --- transcripts ---
	//
	// Before classify, not after: the summary is written into items.summary, and the
	// classifier reads that. A run late is a run wasted.
	//
	// A failure here is never fatal. The caption path is undocumented and will break
	// eventually; when it does, this degrades to contributing nothing, the same way a
	// dead feed does in ingest.
	//
	// A catch-up run skips it too: transcripts belong to newly ingested videos,
	// and a catch-up ingested nothing.
	var transcribe TranscribeResult
	var transcribeErr error
	if !catchup {
		r.setStage(StageTranscribing)
		transcribe, transcribeErr = Transcribe(ctx, r.db, client, cfg, r.log)
		r.mu.Lock()
		r.state.Transcribe = &transcribe
		r.state.LLMCalls = client.Guard().UsedThisRun()
		r.mu.Unlock()
	}
	if transcribeErr != nil {
		if errors.Is(transcribeErr, llm.ErrCapReached) || errors.Is(transcribeErr, context.Canceled) {
			r.stopReason(client, "transcripts stopped", transcribeErr)
			return
		}
		// Anything else: log it and carry on to the part that matters.
		r.log.Warn("transcripts incomplete", "err", transcribeErr)
	}

	r.setStage(StageClassifying)
	classify, err := Classify(ctx, r.db, client, cfg, runDate, r.log)
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

	// --- extract claims ---
	//
	// LAST, after the sky is composed, and that ordering is deliberate. Extraction
	// and the star map now read the same items on every run and share one per-run
	// call cap, so whichever goes first can starve the other. The sky is the
	// product and the world model is the new thing, so the new thing yields: a cap
	// breach here costs a day of claims, and a cap breach the other way round would
	// be an empty sky caused by a stage nothing on screen reads yet.
	//
	// Nothing downstream depends on claims yet, so position is otherwise free. When
	// synthesis eventually reads them, this has to move back above it -- and at that
	// point the budget question above has to be answered properly rather than by
	// ordering.
	//
	// A failure is never fatal to the run, for the same reason transcripts are not:
	// the watermark simply does not advance and the next run sees the same items.
	r.setStage(StageExtracting)
	extract, extractErr := Extract(ctx, r.db, client, cfg, r.log)
	r.mu.Lock()
	r.state.Extract = &extract
	r.state.LLMCalls = client.Guard().UsedThisRun()
	r.mu.Unlock()
	if extractErr != nil {
		if errors.Is(extractErr, llm.ErrCapReached) || errors.Is(extractErr, context.Canceled) {
			r.stopReason(client, "claim extraction stopped", extractErr)
			return
		}
		r.log.Warn("claim extraction incomplete", "err", extractErr)
	}

	// --- push the deltas ---
	//
	// After extraction, because it sends what extraction just found, and outside
	// the LLM budget entirely: a push is HTTP to one webhook and costs no tokens.
	//
	// A failure here is logged, never fatal. The claims are written either way, and
	// MarkDeltasPushed stamps only what was actually delivered -- so a Slack outage
	// costs a day's notification, not a day's changes, and tomorrow's push picks
	// them up.
	r.setStage(StagePushing)
	push, pushErr := Push(ctx, r.db, cfg, r.log)
	r.mu.Lock()
	r.state.Push = &push
	r.mu.Unlock()
	if pushErr != nil {
		r.log.Warn("push failed", "err", pushErr)
	}

	r.log.Info("run complete",
		"items_new", ingest.ItemsNew,
		"transcripts", transcribe.Summarized,
		"matches", classify.MatchesKept,
		"signals", synth.SignalsWritten,
		"claims", extract.ClaimsWritten,
		"pushed", push.Deltas,
		"llm_calls", client.Guard().UsedThisRun(),
	)
}

func (r *Runner) setStage(s Stage) {
	now := time.Now()
	r.mu.Lock()
	r.state.Stage = s
	r.state.StageStartedAt = &now
	r.state.Progress = nil
	r.mu.Unlock()
}

// stopReason turns a stage failure into a banner, preferring the spend guard's
// own words when it was the guard that stopped things.
func (r *Runner) stopReason(client *llm.Client, what string, err error) {
	if errors.Is(err, llm.ErrCapReached) {
		if reason := client.Guard().StoppedFor(); reason != "" {
			r.finishWithBanner("Run stopped: " + reason + ". Raise the cap in Settings to continue.")
			r.mu.Lock()
			r.state.StoppedBy = client.Guard().StoppedBy()
			r.mu.Unlock()
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
	r.state.StoppedAt = r.state.Stage
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

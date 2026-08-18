package httpapi

import (
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/tammai/interstellar/internal/config"
	"github.com/tammai/interstellar/internal/llm"
	"github.com/tammai/interstellar/internal/pipeline"
	"github.com/tammai/interstellar/internal/sources"
	"github.com/tammai/interstellar/internal/store"
)

// statusResponse is what the UI polls while a run is in flight, and what the
// setup hint reads on first load.
type statusResponse struct {
	Run pipeline.State `json:"run"`

	// Readiness is per-role, and each entry names the specific missing thing so
	// the UI can say "DEEPSEEK_API_KEY is not set" instead of "not ready".
	Readiness []llm.RoleStatus `json:"readiness"`
	Ready     bool             `json:"ready"`
	// SetupHint is non-empty while the instance still needs the owner's
	// attention -- unwritten topic definitions or an unusable model config (§7).
	SetupHint string `json:"setup_hint,omitempty"`

	// Banner is the run banner, or a live cap warning. Derived partly from
	// llm_calls rather than memory, so a cap breached before a restart is still
	// visible afterwards.
	Banner string `json:"banner,omitempty"`

	// Counts for the settings popover, which lists Topics and Sources and wants
	// to say how many of each without opening either screen. Not omitempty: zero
	// is the honest answer on a fresh instance, and an absent field would render
	// as an em dash that looks like a failed read.
	TopicsCount  int `json:"topics_count"`
	SourcesCount int `json:"sources_count"`

	CallsToday int              `json:"calls_today"`
	CallCapDay int              `json:"call_cap_day"`
	Usage      []store.LLMUsage `json:"usage,omitempty"`

	// Transcripts by status, so a caption path that has quietly stopped working is
	// visible rather than showing up only as videos that never match anything.
	Transcripts map[string]int `json:"transcripts,omitempty"`

	// Failures are roles whose most recent day of calls produced nothing but
	// errors. Separate from Readiness because they answer different questions:
	// readiness is whether the configuration COULD work, this is whether it DID.
	// A keyless provider always passes the first and can fail every call of the
	// second.
	Failures []store.RoleFailure `json:"failures,omitempty"`

	NextRuns []time.Time `json:"next_runs,omitempty"`
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	settings, err := s.DB.Settings()
	if err != nil {
		s.error(w, http.StatusInternalServerError, "could not read settings", err)
		return
	}
	cfg, err := config.Load(s.DB)
	if err != nil {
		s.error(w, http.StatusInternalServerError, "could not read settings", err)
		return
	}

	out := statusResponse{
		Run:        s.Runner.State(),
		CallCapDay: cfg.MaxCallsPerDay,
	}

	// A malformed registry is itself a readiness problem, not a 500: the owner
	// broke their own settings and needs to be told which part.
	llmCfg, cfgErr := llm.LoadConfig(settings)
	if cfgErr != nil {
		out.Ready = false
		out.SetupHint = "Model configuration problem: " + cfgErr.Error()
	} else {
		out.Readiness = llmCfg.Readiness()
		out.Ready = llmCfg.Ready()
	}

	// Read the topics once and reuse them: the counts below and the setup hint
	// both need them, and the pool is a single connection.
	topics, topicsErr := s.DB.AllTopics()
	if topicsErr == nil {
		out.TopicsCount = len(topics)
	}
	if srcs, err := s.DB.AllSources(); err == nil {
		out.SourcesCount = len(srcs)
	}

	runDate := store.RunDate(time.Now().In(s.Env.Location))
	if n, err := s.DB.LLMCallsOn(runDate); err == nil {
		out.CallsToday = n
	}
	if usage, err := s.DB.LLMUsageOn(runDate); err == nil {
		out.Usage = usage
	}
	if counts, err := s.DB.TranscriptCounts(); err == nil && len(counts) > 0 {
		out.Transcripts = counts
	}
	// A week back: long enough that a Friday failure is still reported on Monday,
	// short enough that a provider abandoned a month ago stops nagging.
	since := store.RunDate(time.Now().In(s.Env.Location).AddDate(0, 0, -failureWindowDays))
	if failures, err := s.DB.FailingRoles(since); err == nil {
		out.Failures = failures
	}

	out.Banner = s.banner(out, cfg)

	if out.SetupHint == "" {
		out.SetupHint = s.setupHint(out, topics, topicsErr)
	}

	if s.Scheduler != nil {
		out.NextRuns = s.Scheduler.NextRuns()
	}

	s.json(w, http.StatusOK, out)
}

// failureWindowDays bounds how far back a wholly-failing role is still reported.
const failureWindowDays = 7

// pipelineRoles are the roles whose failure means the product is broken, and so
// the ones that earn the star map's banner.
//
// Chat is deliberately absent, for the same reason it is absent from Ready(): a
// dead Ask screen must not dress itself up as a broken morning run. Its failure
// still travels in status.Failures, and the Settings header renders it there.
var pipelineRoles = map[string]bool{"classifier": true, "synthesizer": true, "weekly": true}

// banner prefers the run's own reason, then a wholly-failing role, then a cap
// state derived from the database. The last two are read from llm_calls rather
// than memory, which is what makes both survive a restart (§10 wants the guard's
// effect visible, not just logged).
func (s *Server) banner(st statusResponse, cfg config.Runtime) string {
	if st.Run.Banner != "" {
		return st.Run.Banner
	}

	// Ahead of the cap, and deliberately so. When a dead provider burns the daily
	// budget on retries both are true at once, and only one of them is the cause:
	// raising the cap on a model that answers 403 just buys more 403s.
	for _, f := range st.Failures {
		if !pipelineRoles[f.Role] {
			continue
		}
		return fmt.Sprintf(
			"Every %s call failed on %s (%d of them) and none succeeded — %s: %s. Change the model in Settings.",
			f.Role, f.RunDate, f.Failed, f.Ref, sources.Truncate(f.Error, 160))
	}

	if cfg.MaxCallsPerDay > 0 && st.CallsToday >= cfg.MaxCallsPerDay {
		return fmt.Sprintf(
			"Daily cap of %d LLM calls reached; runs are paused until tomorrow. Raise it in Settings to continue.",
			cfg.MaxCallsPerDay)
	}
	return ""
}

// setupHint tells a fresh instance what to do next, one thing at a time.
func (s *Server) setupHint(st statusResponse, topics []store.Topic, err error) string {
	for _, role := range st.Readiness {
		if !role.Ready {
			return fmt.Sprintf("Finish setup: the %s cannot run (%s).", role.Role, role.Problem)
		}
	}

	if err != nil {
		return ""
	}
	unset := 0
	for _, t := range topics {
		if t.NeedsSetup() {
			unset++
		}
	}
	switch {
	case len(topics) == 0:
		return "Add a topic to start seeing signals."
	case unset == len(topics):
		return "Write a definition for each topic so the classifier knows what you care about."
	case unset > 0:
		return fmt.Sprintf("%d topic(s) still need a definition before they can produce signals.", unset)
	}
	return ""
}

// handleRefresh starts an on-demand run (§3, §10: under ~3 minutes with visible
// progress).
func (s *Server) handleRefresh(w http.ResponseWriter, r *http.Request) {
	err := s.Runner.Start("manual refresh")
	if errors.Is(err, pipeline.ErrAlreadyRunning) {
		// 409, not 400: the request was well-formed, the state was wrong. The UI
		// uses this to keep showing progress rather than reporting an error.
		s.error(w, http.StatusConflict, "a run is already in progress", nil)
		return
	}
	if err != nil {
		s.error(w, http.StatusInternalServerError, "could not start a run", err)
		return
	}
	s.json(w, http.StatusAccepted, map[string]string{"status": "started"})
}

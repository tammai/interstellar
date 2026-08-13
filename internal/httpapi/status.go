package httpapi

import (
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/tammai/interstellar/internal/config"
	"github.com/tammai/interstellar/internal/llm"
	"github.com/tammai/interstellar/internal/pipeline"
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

	out.Banner = s.banner(out, cfg)

	if out.SetupHint == "" {
		out.SetupHint = s.setupHint(out, topics, topicsErr)
	}

	if s.Scheduler != nil {
		out.NextRuns = s.Scheduler.NextRuns()
	}

	s.json(w, http.StatusOK, out)
}

// banner prefers the run's own reason, then falls back to a cap state derived
// from the database. That second path is what makes a cap breach survive a
// restart (§10 wants the guard's effect visible, not just logged).
func (s *Server) banner(st statusResponse, cfg config.Runtime) string {
	if st.Run.Banner != "" {
		return st.Run.Banner
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

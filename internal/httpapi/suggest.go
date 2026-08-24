package httpapi

import (
	"errors"
	"net/http"
	"time"

	"github.com/tammai/interstellar/internal/llm"
	"github.com/tammai/interstellar/internal/pipeline"
	"github.com/tammai/interstellar/internal/store"
)

// handleSuggestSources proposes sources for one topic.
//
// A POST rather than a GET, and not because it writes rows -- though it does. It
// spends a model call and makes up to eight outbound requests, and a GET that does
// that is a GET a browser or a crawler is entitled to repeat.
//
// Readiness is checked on the SYNTHESIZER, which is the chain this runs on. Not
// Ready(): the classifier having no key has nothing to do with whether a
// suggestion pass can work, and refusing on it would be the same mistake as
// folding chat into Ready().
func (s *Server) handleSuggestSources(w http.ResponseWriter, r *http.Request) {
	id, ok := s.pathID(w, r)
	if !ok {
		return
	}

	topic, err := s.DB.Topic(id)
	if errors.Is(err, store.ErrNotFound) {
		s.error(w, http.StatusNotFound, "no such topic", nil)
		return
	}
	if err != nil {
		s.error(w, http.StatusInternalServerError, "could not read the topic", err)
		return
	}
	// A topic with no definition is the first-run state, and the model would be
	// guessing from a title alone -- which is exactly how you get eight plausible
	// feeds about the wrong subject. Say what to do instead.
	if topic.NeedsSetup() {
		s.error(w, http.StatusBadRequest,
			"write a definition for this topic first — suggestions come from what you mean by it, not from its name", nil)
		return
	}

	settings, err := s.DB.Settings()
	if err != nil {
		s.error(w, http.StatusInternalServerError, "could not read settings", err)
		return
	}
	llmCfg, err := llm.LoadConfig(settings)
	if err != nil {
		s.error(w, http.StatusBadRequest, "model configuration problem: "+err.Error(), nil)
		return
	}
	if st := llmCfg.RoleReadiness(llm.RoleSynthesizer); !st.Ready {
		s.error(w, http.StatusServiceUnavailable,
			"the synthesizer model cannot run: "+st.Problem, nil)
		return
	}

	runDate := store.RunDate(time.Now().In(s.Env.Location))
	client := llm.NewClient(llmCfg, s.DB, runDate, s.Log)

	result, err := pipeline.SuggestSources(r.Context(), s.DB, client, topic, s.Log)

	// A breached cap is the guard working, not a failure, and the screen says so
	// in its own words -- same treatment as a question that runs out of budget.
	if errors.Is(err, llm.ErrCapReached) {
		reason := client.Guard().StoppedFor()
		if reason == "" {
			reason = "the daily call cap was reached"
		}
		s.error(w, http.StatusTooManyRequests,
			"Stopped: "+reason+". Raise it in Settings, or try again tomorrow.", nil)
		return
	}
	if r.Context().Err() != nil {
		return // the reader navigated away; nothing to report to nobody
	}
	if err != nil {
		s.error(w, http.StatusBadGateway, "could not suggest sources: "+err.Error(), err)
		return
	}

	s.json(w, http.StatusOK, result)
}

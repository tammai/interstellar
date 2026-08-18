package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/tammai/interstellar/internal/llm"
	"github.com/tammai/interstellar/internal/pipeline"
	"github.com/tammai/interstellar/internal/store"
)

type askRequest struct {
	Question string `json:"question"`
}

type askResponse struct {
	pipeline.AskResult
	// TurnID is the stored row, so the page can link to a question later.
	TurnID int64 `json:"turn_id,omitempty"`
	// ChatCallsToday and ChatCapDay let the screen show what is left before it
	// runs out, rather than discovering the cap by hitting it.
	ChatCallsToday int `json:"chat_calls_today"`
	ChatCapDay     int `json:"chat_cap_day"`
}

// handleAsk answers a question over the owner's own signals and items.
//
// Config is resolved per request, never cached: §3.1's no-restart promise applies
// here exactly as it does to a run, so reassigning the chat role in Settings takes
// effect on the next question.
func (s *Server) handleAsk(w http.ResponseWriter, r *http.Request) {
	var req askRequest
	if err := decodeJSON(w, r, &req); err != nil {
		s.error(w, http.StatusBadRequest, "invalid JSON body", nil)
		return
	}
	if req.Question == "" {
		s.error(w, http.StatusBadRequest, "question is required", nil)
		return
	}

	settings, err := s.DB.Settings()
	if err != nil {
		s.error(w, http.StatusInternalServerError, "could not read settings", err)
		return
	}
	llmCfg, err := llm.LoadConfig(settings)
	if err != nil {
		// The owner broke their own settings; tell them which part.
		s.error(w, http.StatusBadRequest, "model configuration problem: "+err.Error(), nil)
		return
	}

	// Chat readiness is checked on its own, not via Ready(): the pipeline's roles
	// have nothing to do with whether a question can be answered, and vice versa.
	if st := llmCfg.RoleReadiness(llm.RoleChat); !st.Ready {
		s.error(w, http.StatusServiceUnavailable,
			"the chat model cannot run: "+st.Problem, nil)
		return
	}

	runDate := store.RunDate(time.Now().In(s.Env.Location))
	client := llm.NewClient(llmCfg, s.DB, runDate, s.Log)

	asker := pipeline.NewAsker(s.DB, s.Log, func() time.Time {
		return time.Now().In(s.Env.Location)
	})

	result, askErr := asker.Ask(r.Context(), client, req.Question)

	// A breached cap is not a server error -- it is the guard doing its job, and
	// the screen needs to say so specifically rather than show a red failure.
	if errors.Is(askErr, llm.ErrCapReached) {
		reason := client.Guard().StoppedFor()
		if reason == "" {
			reason = "the daily call cap was reached"
		}
		s.error(w, http.StatusTooManyRequests,
			"Stopped: "+reason+". Raise it in Settings, or ask again tomorrow.", nil)
		return
	}
	if errors.Is(askErr, r.Context().Err()) && r.Context().Err() != nil {
		// The reader navigated away or hit stop. Nothing to report to nobody.
		return
	}
	if askErr != nil {
		s.error(w, http.StatusBadGateway, "could not answer that: "+askErr.Error(), askErr)
		return
	}

	out := askResponse{AskResult: result, ChatCapDay: llmCfg.MaxChatCallsPerDay}

	// Persist before responding. A turn that reached the reader but not the
	// database would be missing from the one record of what was actually asked.
	if id, err := s.DB.SaveChatTurn(turnFrom(runDate, result)); err != nil {
		s.Log.Warn("could not save chat turn", "err", err)
	} else {
		out.TurnID = id
	}

	if n, err := s.DB.LLMCallsByRoleOn(runDate, string(llm.RoleChat)); err == nil {
		out.ChatCallsToday = n
	}

	s.json(w, http.StatusOK, out)
}

// turnFrom flattens a result into its stored shape.
func turnFrom(runDate string, r pipeline.AskResult) store.ChatTurn {
	refs := make([]store.HitRef, 0, len(r.Hits))
	for _, h := range r.Hits {
		refs = append(refs, store.HitRef{Kind: h.Kind, ID: h.ID})
	}

	turn := store.ChatTurn{
		RunDate:   runDate,
		Question:  r.Question,
		Answer:    r.Answer,
		Planned:   r.Planned,
		HitRefs:   refs,
		HitCount:  len(r.Hits),
		Calls:     r.Calls,
		TokensIn:  r.TokensIn,
		TokensOut: r.TokensOut,
		ElapsedMS: r.ElapsedMS,
	}
	// Best-effort: losing the plan must not lose the turn.
	if encoded, err := json.Marshal(r.Query); err == nil {
		turn.Query = encoded
	}
	return turn
}

type chatHistoryResponse struct {
	Turns []store.ChatTurn `json:"turns"`

	// Ready and Problem describe the chat role, so the page can show a setup hint
	// instead of failing on the first question.
	Ready   bool   `json:"ready"`
	Problem string `json:"problem,omitempty"`

	ChatCallsToday int `json:"chat_calls_today"`
	ChatCapDay     int `json:"chat_cap_day"`
}

// handleClearChat empties the transcript.
//
// A real delete rather than a screen the page clears locally: history is reloaded on
// every open, so clearing only the DOM would put every question back on the next
// visit. It does not touch llm_calls, so the day's chat budget is unchanged -- the
// calls happened, and a clear button that refunded them would be a way to spend the
// cap twice.
func (s *Server) handleClearChat(w http.ResponseWriter, r *http.Request) {
	n, err := s.DB.DeleteChatTurns()
	if err != nil {
		s.error(w, http.StatusInternalServerError, "could not clear the chat history", err)
		return
	}
	s.json(w, http.StatusOK, map[string]int64{"cleared": n})
}

// handleChatHistory is what the page loads on open: past turns plus whether
// asking will work at all.
func (s *Server) handleChatHistory(w http.ResponseWriter, r *http.Request) {
	limit := 0
	if raw := r.URL.Query().Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 {
			s.error(w, http.StatusBadRequest, "limit must be a positive whole number", nil)
			return
		}
		limit = n
	}

	turns, err := s.DB.ChatTurns(limit)
	if err != nil {
		s.error(w, http.StatusInternalServerError, "could not read chat history", err)
		return
	}

	out := chatHistoryResponse{Turns: turns}

	settings, err := s.DB.Settings()
	if err != nil {
		s.error(w, http.StatusInternalServerError, "could not read settings", err)
		return
	}
	if llmCfg, err := llm.LoadConfig(settings); err != nil {
		out.Problem = "model configuration problem: " + err.Error()
	} else {
		st := llmCfg.RoleReadiness(llm.RoleChat)
		out.Ready = st.Ready
		out.Problem = st.Problem
		out.ChatCapDay = llmCfg.MaxChatCallsPerDay
	}

	runDate := store.RunDate(time.Now().In(s.Env.Location))
	if n, err := s.DB.LLMCallsByRoleOn(runDate, string(llm.RoleChat)); err == nil {
		out.ChatCallsToday = n
	}

	s.json(w, http.StatusOK, out)
}

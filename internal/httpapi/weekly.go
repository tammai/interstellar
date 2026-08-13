package httpapi

import (
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/tammai/interstellar/internal/config"
	"github.com/tammai/interstellar/internal/notify"
	"github.com/tammai/interstellar/internal/pipeline"
	"github.com/tammai/interstellar/internal/store"
)

type weeklyResponse struct {
	Latest  *store.WeeklyDigest  `json:"latest,omitempty"`
	History []store.WeeklyDigest `json:"history,omitempty"`

	// Delivery describes what would happen if a round-up were written now, so the
	// screen can say "email is off" rather than leaving the owner to find out by a
	// digest never arriving.
	Delivery deliveryStatus `json:"delivery"`
}

type deliveryStatus struct {
	EmailEnabled bool   `json:"email_enabled"`
	EmailTo      string `json:"email_to,omitempty"`
	EmailProblem string `json:"email_problem,omitempty"`
	PingEnabled  bool   `json:"ping_enabled"`
	PingProblem  string `json:"ping_problem,omitempty"`
	// Schedule is when the next round-up is due, in the owner's words.
	Schedule string `json:"schedule,omitempty"`
}

func (s *Server) handleWeekly(w http.ResponseWriter, r *http.Request) {
	limit := 0
	if raw := r.URL.Query().Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 {
			s.error(w, http.StatusBadRequest, "limit must be a positive whole number", nil)
			return
		}
		limit = n
	}

	history, err := s.DB.WeeklyDigests(limit)
	if err != nil {
		s.error(w, http.StatusInternalServerError, "could not read the round-ups", err)
		return
	}

	out := weeklyResponse{History: history}
	if len(history) > 0 {
		out.Latest = &history[0]
	}

	cfg, err := config.Load(s.DB)
	if err != nil {
		s.error(w, http.StatusInternalServerError, "could not read settings", err)
		return
	}
	out.Delivery = describeDelivery(cfg)

	s.json(w, http.StatusOK, out)
}

// describeDelivery reports what is switched on and what is missing.
//
// Never the password itself, and never the env var's value -- only whether it
// resolves, which is the same contract /api/settings honors for provider keys.
func describeDelivery(cfg config.Runtime) deliveryStatus {
	out := deliveryStatus{
		EmailEnabled: cfg.DigestEnabled,
		PingEnabled:  cfg.PingEnabled,
	}
	if cfg.ScheduleWeeklyDay != "" {
		out.Schedule = cfg.ScheduleWeeklyDay + " " + cfg.ScheduleWeeklyTime
	}

	if cfg.DigestEnabled {
		out.EmailTo = cfg.DigestTo
		out.EmailProblem = notify.SMTPConfig{
			Host: cfg.SMTPHost, Port: cfg.SMTPPort,
			Username: cfg.SMTPUsername, PasswordEnv: cfg.SMTPPasswordEnv,
			From: cfg.DigestFrom, To: cfg.DigestTo,
		}.Problem()
	}
	if cfg.PingEnabled && cfg.PingURL == "" {
		out.PingProblem = "no webhook URL set"
	}
	return out
}

type weeklyRunRequest struct {
	// Week is an optional YYYY-MM-DD inside the week to write. Defaults to the week
	// that just finished, which is what the schedule writes.
	Week string `json:"week"`
	// Deliver sends it as well as writing it. Off by default: pressing "write it
	// now" to look at the prose should not mail anyone.
	Deliver bool `json:"deliver"`
}

// handleWeeklyRun writes a round-up on demand.
//
// Synchronous, unlike POST /api/refresh. It is one model call rather than a
// pipeline, the owner is waiting to read the result, and returning 202 would mean
// building a second progress channel for something that takes a few seconds.
func (s *Server) handleWeeklyRun(w http.ResponseWriter, r *http.Request) {
	var req weeklyRunRequest
	if r.ContentLength > 0 {
		if err := decodeJSON(w, r, &req); err != nil {
			s.error(w, http.StatusBadRequest, "invalid JSON body", nil)
			return
		}
	}

	when := time.Now().In(s.Env.Location).AddDate(0, 0, -7)
	if req.Week != "" {
		parsed, err := time.ParseInLocation("2006-01-02", req.Week, s.Env.Location)
		if err != nil {
			s.error(w, http.StatusBadRequest, "week must be YYYY-MM-DD", nil)
			return
		}
		when = parsed
	}

	runner := pipeline.NewWeeklyRunner(s.DB, s.Env, s.Log, func() time.Time {
		return time.Now().In(s.Env.Location)
	})

	result, err := runner.Run(r.Context(), when, req.Deliver)
	if err != nil {
		if errors.Is(err, r.Context().Err()) && r.Context().Err() != nil {
			return
		}
		s.error(w, http.StatusBadGateway, "could not write the round-up: "+err.Error(), err)
		return
	}

	s.json(w, http.StatusOK, result)
}

// handleDeliveryTest sends a test message, so a relay can be verified without
// waiting a week to find out it was wrong.
//
// The same shape as the per-role model test button, and for the same reason: a
// setting you cannot verify is a setting you do not trust.
func (s *Server) handleDeliveryTest(w http.ResponseWriter, r *http.Request) {
	cfg, err := config.Load(s.DB)
	if err != nil {
		s.error(w, http.StatusInternalServerError, "could not read settings", err)
		return
	}

	type result struct {
		Email      bool   `json:"email"`
		EmailError string `json:"email_error,omitempty"`
		Ping       bool   `json:"ping"`
		PingError  string `json:"ping_error,omitempty"`
	}
	var out result

	// Tested whether or not they are enabled: the point is to check the credentials
	// before switching delivery on, not after.
	smtpCfg := notify.SMTPConfig{
		Host: cfg.SMTPHost, Port: cfg.SMTPPort,
		Username: cfg.SMTPUsername, PasswordEnv: cfg.SMTPPasswordEnv,
		From: cfg.DigestFrom, To: cfg.DigestTo,
	}
	if smtpCfg.Configured() {
		err := notify.SendEmail(r.Context(), smtpCfg,
			"Interstellar test message",
			"This is a test from your Interstellar instance.\n\n"+
				"If it arrived, the weekly round-up will too.\n")
		if err != nil {
			out.EmailError = err.Error()
		} else {
			out.Email = true
		}
	} else {
		out.EmailError = smtpCfg.Problem()
	}

	if cfg.PingURL != "" {
		err := notify.SendPing(r.Context(), cfg.PingURL, notify.PingPayload{
			Event: "test", Summary: "Test ping from Interstellar.",
		})
		if err != nil {
			out.PingError = err.Error()
		} else {
			out.Ping = true
		}
	} else {
		out.PingError = "no webhook URL set"
	}

	s.json(w, http.StatusOK, out)
}

package httpapi

import (
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/tammai/interstellar/internal/config"
	"github.com/tammai/interstellar/internal/store"
)

// skyTopic is a constellation as the canvas needs it.
type skyTopic struct {
	ID    int64   `json:"id"`
	Title string  `json:"title"`
	PosX  float64 `json:"pos_x"`
	PosY  float64 `json:"pos_y"`
	// NeedsSetup drives the first-run hint (§7): a topic with no definition
	// cannot produce signals, and the UI should say so rather than look broken.
	NeedsSetup bool `json:"needs_setup"`
}

// skyResponse is everything the front door renders in one request. One round
// trip on purpose: the sky is the front door, and it should not stagger in.
type skyResponse struct {
	ObservationDate string         `json:"observation_date"`
	Days            int            `json:"days"`
	HotThreshold    int            `json:"hot_threshold"`
	Topics          []skyTopic     `json:"topics"`
	Signals         []store.Signal `json:"signals"`
}

func (s *Server) handleSky(w http.ResponseWriter, r *http.Request) {
	cfg, err := config.Load(s.DB)
	if err != nil {
		s.error(w, http.StatusInternalServerError, "could not read settings", err)
		return
	}

	days := cfg.SkyDays
	if v := r.URL.Query().Get("days"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 90 {
			days = n
		}
	}

	topics, err := s.DB.ActiveTopics()
	if err != nil {
		s.error(w, http.StatusInternalServerError, "could not read topics", err)
		return
	}
	signals, err := s.DB.RecentSignals(days)
	if err != nil {
		s.error(w, http.StatusInternalServerError, "could not read signals", err)
		return
	}

	out := skyResponse{
		ObservationDate: time.Now().In(s.Env.Location).Format("2006-01-02"),
		Days:            days,
		HotThreshold:    cfg.HotThreshold,
		Topics:          make([]skyTopic, 0, len(topics)),
		Signals:         signals,
	}
	for _, t := range topics {
		out.Topics = append(out.Topics, skyTopic{
			ID: t.ID, Title: t.Title, PosX: t.PosX, PosY: t.PosY,
			NeedsSetup: t.NeedsSetup(),
		})
	}
	if out.Signals == nil {
		out.Signals = []store.Signal{}
	}

	s.json(w, http.StatusOK, out)
}

func (s *Server) handleSignal(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		s.error(w, http.StatusBadRequest, "signal id must be a number", nil)
		return
	}

	detail, err := s.DB.SignalByID(id)
	if errors.Is(err, store.ErrNotFound) {
		s.error(w, http.StatusNotFound, "no such signal", nil)
		return
	}
	if err != nil {
		s.error(w, http.StatusInternalServerError, "could not read signal", err)
		return
	}
	s.json(w, http.StatusOK, detail)
}

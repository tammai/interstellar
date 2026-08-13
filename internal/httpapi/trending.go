package httpapi

import (
	"net/http"
	"strconv"

	"github.com/tammai/interstellar/internal/store"
)

const (
	trendWindowDefault = 30
	trendWindowMax     = 365
)

type trendResponse struct {
	Days int `json:"days"`

	Topics    []topicTrendDTO        `json:"topics"`
	Daily     []store.DailyVolume    `json:"daily"`
	Recurring []store.RecurringStory `json:"recurring"`

	TotalSignals int `json:"total_signals"`
	TotalHot     int `json:"total_hot"`
	// DaysWithSignals is what makes every other number readable. Six signals over
	// two days and over three weeks are different situations, and a total alone
	// cannot tell them apart -- especially on an instance this young.
	DaysWithSignals int `json:"days_with_signals"`
}

// topicTrendDTO adds the derived direction, so the UI does not re-derive it and
// the two cannot disagree.
type topicTrendDTO struct {
	store.TopicTrend
	Direction string `json:"direction"`
}

// handleTrending is the constellation history: what each topic has been doing over
// a window, rather than what it did today.
//
// Read-only over signals, with no model involved. Worth saying because it means
// this endpoint is free to poll and cannot be affected by a spend cap.
func (s *Server) handleTrending(w http.ResponseWriter, r *http.Request) {
	days := trendWindowDefault
	if raw := r.URL.Query().Get("days"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 2 || n > trendWindowMax {
			s.error(w, http.StatusBadRequest,
				"days must be a whole number between 2 and 365", nil)
			return
		}
		days = n
	}

	trends, err := s.DB.TopicTrends(days)
	if err != nil {
		s.error(w, http.StatusInternalServerError, "could not read topic history", err)
		return
	}
	daily, err := s.DB.DailyVolumes(days)
	if err != nil {
		s.error(w, http.StatusInternalServerError, "could not read daily volumes", err)
		return
	}
	// A story has to appear on at least three days in a long window to be worth
	// calling recurring; in a short one, two is all there is room for.
	minDays := 2
	if days >= 14 {
		minDays = 3
	}
	recurring, err := s.DB.RecurringStories(days, minDays, 10)
	if err != nil {
		s.error(w, http.StatusInternalServerError, "could not read recurring stories", err)
		return
	}

	out := trendResponse{Days: days, Daily: daily, Recurring: recurring}
	out.Topics = make([]topicTrendDTO, 0, len(trends))
	for _, t := range trends {
		out.Topics = append(out.Topics, topicTrendDTO{TopicTrend: t, Direction: t.Direction()})
		out.TotalSignals += t.Signals
		out.TotalHot += t.Hot
	}
	out.DaysWithSignals = len(daily)

	s.json(w, http.StatusOK, out)
}

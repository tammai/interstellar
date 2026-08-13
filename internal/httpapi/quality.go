package httpapi

import (
	"net/http"
	"strconv"

	"github.com/tammai/interstellar/internal/store"
)

// qualityWindowDays bounds the ?days= window. The floor is 1 because "today" is a
// legitimate question after a manual refresh; the ceiling is a year because the
// table is one row per topic per day and there is no reason to let a query walk
// further than the data could plausibly go.
const (
	qualityWindowDefault = 7
	qualityWindowMax     = 365
)

// topicStatsResponse is the per-topic hit-rate view.
//
// Signals-per-topic already appears on the Topics screen; this endpoint exists for
// the denominator, which lives nowhere else. Hit rate is the number that tells the
// owner whether a definition is doing its job: a topic matching 2% of what it sees
// is drawn too tight, one matching 60% is drawn too loose, and neither is visible
// from a signal count alone.
type topicStatsResponse struct {
	Days  int                  `json:"days"`
	Rows  []store.TopicHitRate `json:"rows"`
	Ready bool                 `json:"ready"`

	// Totals across the window, so the screen can lead with the instance-wide
	// number before breaking it down.
	ItemsConsidered int     `json:"items_considered"`
	MatchesKept     int     `json:"matches_kept"`
	Batches         int     `json:"batches"`
	HitRate         float64 `json:"hit_rate"`
}

func (s *Server) handleTopicStats(w http.ResponseWriter, r *http.Request) {
	days := qualityWindowDefault
	if raw := r.URL.Query().Get("days"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > qualityWindowMax {
			s.error(w, http.StatusBadRequest, "days must be a whole number between 1 and 365", nil)
			return
		}
		days = n
	}

	rows, err := s.DB.TopicHitRates(days)
	if err != nil {
		s.error(w, http.StatusInternalServerError, "could not read topic statistics", err)
		return
	}

	out := topicStatsResponse{Days: days, Rows: rows, HitRate: -1}
	for _, row := range rows {
		out.ItemsConsidered += row.ItemsConsidered
		out.MatchesKept += row.MatchesKept
		out.Batches += row.Batches
	}
	if out.ItemsConsidered > 0 {
		out.HitRate = float64(out.MatchesKept) / float64(out.ItemsConsidered)
		out.Ready = true
	}

	s.json(w, http.StatusOK, out)
}

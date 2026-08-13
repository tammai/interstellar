package httpapi

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/tammai/interstellar/internal/store"
)

// searchResponse is what the chat page and any future archive view read.
type searchResponse struct {
	Query string            `json:"query"`
	Kind  string            `json:"kind"`
	Hits  []store.SearchHit `json:"hits"`
	Count int               `json:"count"`
}

// handleSearch is full-text search with no model involved.
//
// Worth stating plainly because it is the reason this endpoint exists separately
// from /api/ask: retrieval is local and takes milliseconds. Everything expensive
// about answering a question is the model call, so the page can show what it
// found -- with sources -- long before any prose arrives.
func (s *Server) handleSearch(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()

	kind := q.Get("kind")
	switch kind {
	case "", "signals", "items":
	default:
		s.error(w, http.StatusBadRequest, `kind must be "signals", "items", or omitted`, nil)
		return
	}

	limit := 0
	if raw := q.Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 {
			s.error(w, http.StatusBadRequest, "limit must be a positive whole number", nil)
			return
		}
		limit = n // Search clamps the ceiling itself
	}

	var topicIDs []int64
	if raw := q.Get("topic_id"); raw != "" {
		for _, part := range strings.Split(raw, ",") {
			id, err := strconv.ParseInt(strings.TrimSpace(part), 10, 64)
			if err != nil {
				s.error(w, http.StatusBadRequest, "topic_id must be a comma-separated list of ids", nil)
				return
			}
			topicIDs = append(topicIDs, id)
		}
	}

	query := store.SearchQuery{
		Text:     q.Get("q"),
		Kind:     kind,
		TopicIDs: topicIDs,
		From:     q.Get("from"),
		To:       q.Get("to"),
		Limit:    limit,
	}
	if !validDate(query.From) || !validDate(query.To) {
		s.error(w, http.StatusBadRequest, "from and to must be YYYY-MM-DD", nil)
		return
	}

	hits, err := s.DB.Search(query)
	if err != nil {
		s.error(w, http.StatusInternalServerError, "search failed", err)
		return
	}

	s.json(w, http.StatusOK, searchResponse{
		Query: query.Text,
		Kind:  kind,
		Hits:  hits,
		Count: len(hits),
	})
}

// validDate accepts an empty string or a real YYYY-MM-DD.
//
// time.Parse rather than a digit check: the digit version accepted 2026-99-99, which
// is not a date but is a perfectly good string bound -- so it reached the query and
// silently matched nothing instead of being refused. The chat planner's own validator
// always parsed; these two disagreeing about what a date is was the actual defect.
func validDate(s string) bool {
	if s == "" {
		return true
	}
	_, err := time.Parse("2006-01-02", s)
	return err == nil
}

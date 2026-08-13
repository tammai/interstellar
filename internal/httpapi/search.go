package httpapi

import (
	"net/http"
	"strconv"
	"strings"

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

// validDate accepts an empty string or a strict YYYY-MM-DD. Loose parsing here
// would push a malformed bound into a string comparison against run_date, where
// it silently matches nothing rather than erroring.
func validDate(s string) bool {
	if s == "" {
		return true
	}
	if len(s) != 10 || s[4] != '-' || s[7] != '-' {
		return false
	}
	for i, r := range s {
		if i == 4 || i == 7 {
			continue
		}
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

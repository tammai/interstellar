package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/tammai/interstellar/internal/store"
)

// Candidate review defaults. Three distinct sources inside fourteen days is the
// brief's threshold; both are query parameters so the owner can look wider when
// the list is empty and tighter when it is noisy, without a settings key for
// something adjusted while reading a screen.
const (
	candidateWindowDefault  = 14
	candidateSourcesDefault = 3
	candidateSamples        = 3
	candidateWindowMax      = 365
)

type entitiesResponse struct {
	Entities []store.Entity `json:"entities"`
}

func (s *Server) handleListEntities(w http.ResponseWriter, r *http.Request) {
	entities, err := s.DB.Entities()
	if err != nil {
		s.error(w, http.StatusInternalServerError, "could not read the watchlist", err)
		return
	}
	s.json(w, http.StatusOK, entitiesResponse{Entities: entities})
}

type candidatesResponse struct {
	Days       int               `json:"days"`
	MinSources int               `json:"min_sources"`
	Candidates []store.Candidate `json:"candidates"`
}

// handleListCandidates is the review queue.
//
// This endpoint is the reason the candidate tables are worth having. Promotion is
// deliberately never automatic, which means it depends entirely on something the
// owner can actually look at -- a review queue reachable only from Go is a feature
// that does not exist, the same way chat_model had a settings key, a chain and a
// test branch but no section on the Model screen, and could only be changed by
// hand-crafting JSON at the API.
func (s *Server) handleListCandidates(w http.ResponseWriter, r *http.Request) {
	days, ok := s.intParam(w, r, "days", candidateWindowDefault, 1, candidateWindowMax)
	if !ok {
		return
	}
	minSources, ok := s.intParam(w, r, "min_sources", candidateSourcesDefault, 1, 50)
	if !ok {
		return
	}

	rows, err := s.DB.PromotableCandidates(days, minSources, candidateSamples)
	if err != nil {
		s.error(w, http.StatusInternalServerError, "could not read candidates", err)
		return
	}
	s.json(w, http.StatusOK, candidatesResponse{
		Days: days, MinSources: minSources, Candidates: rows,
	})
}

type promoteRequest struct {
	Layer string `json:"layer"`
	Kind  string `json:"kind"`
}

// handlePromoteCandidate turns a candidate into a watchlist entity.
//
// The layer is required and comes from the owner, never guessed. "Which layer
// does this belong to" is the judgment the review exists to collect: a candidate
// that fits no existing layer is itself the signal that a new layer exists, which
// is the most interesting thing this queue can surface and precisely what an
// auto-assigned default would bury.
func (s *Server) handlePromoteCandidate(w http.ResponseWriter, r *http.Request) {
	id, ok := s.pathID(w, r)
	if !ok {
		return
	}
	var req promoteRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&req); err != nil {
		s.error(w, http.StatusBadRequest, "invalid JSON body", nil)
		return
	}
	layer := strings.TrimSpace(req.Layer)
	if layer == "" {
		s.error(w, http.StatusBadRequest, "layer is required", nil)
		return
	}

	entityID, err := s.DB.PromoteCandidate(id, layer, strings.TrimSpace(req.Kind))
	if errors.Is(err, store.ErrNotFound) {
		s.error(w, http.StatusNotFound, "no such candidate, or it is already promoted", nil)
		return
	}
	if err != nil {
		// A name that already resolves to an entity is the owner's mistake to see,
		// not a server fault: 409, with the sentence naming what it collided with.
		s.error(w, http.StatusConflict, err.Error(), nil)
		return
	}

	entity, err := s.DB.EntityByID(entityID)
	if err != nil {
		s.error(w, http.StatusInternalServerError, "promoted, but could not read it back", err)
		return
	}
	s.json(w, http.StatusOK, entity)
}

func (s *Server) handleDismissCandidate(w http.ResponseWriter, r *http.Request) {
	id, ok := s.pathID(w, r)
	if !ok {
		return
	}
	err := s.DB.DismissCandidate(id)
	if errors.Is(err, store.ErrNotFound) {
		s.error(w, http.StatusNotFound, "no such candidate", nil)
		return
	}
	if err != nil {
		s.error(w, http.StatusInternalServerError, "could not dismiss the candidate", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type claimsResponse struct {
	Days   int           `json:"days"`
	Claims []store.Claim `json:"claims"`
}

// handleListClaims exposes the world model's contents.
//
// Read-only and unglamorous, and the most useful endpoint here for now: until the
// sky or chat read claims, this is the only way to answer "is extraction actually
// producing anything sensible", which is the open question the whole staging plan
// rests on.
func (s *Server) handleListClaims(w http.ResponseWriter, r *http.Request) {
	days, ok := s.intParam(w, r, "days", 7, 1, candidateWindowMax)
	if !ok {
		return
	}
	limit, ok := s.intParam(w, r, "limit", 200, 1, 1000)
	if !ok {
		return
	}

	since := store.DaysAgo(days)
	claims, err := s.DB.ClaimsSince(since, limit)
	if err != nil {
		s.error(w, http.StatusInternalServerError, "could not read claims", err)
		return
	}
	s.json(w, http.StatusOK, claimsResponse{Days: days, Claims: claims})
}

type deltasResponse struct {
	Deltas []store.Delta `json:"deltas"`
}

// handleListDeltas shows what the next push would say, without sending it.
//
// A dry run matters more here than on most endpoints: the push is the one part of
// this system that reaches other people, and "what would you post" should be
// answerable without posting.
func (s *Server) handleListDeltas(w http.ResponseWriter, r *http.Request) {
	days, ok := s.intParam(w, r, "days", 14, 0, candidateWindowMax)
	if !ok {
		return
	}
	deltas, err := s.DB.PendingDeltas(days, 200)
	if err != nil {
		s.error(w, http.StatusInternalServerError, "could not read pending changes", err)
		return
	}
	s.json(w, http.StatusOK, deltasResponse{Deltas: deltas})
}

// intParam reads a bounded whole-number query parameter.
func (s *Server) intParam(w http.ResponseWriter, r *http.Request, name string, def, lo, hi int) (int, bool) {
	raw := r.URL.Query().Get(name)
	if raw == "" {
		return def, true
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < lo || n > hi {
		s.error(w, http.StatusBadRequest,
			name+" must be a whole number between "+strconv.Itoa(lo)+" and "+strconv.Itoa(hi), nil)
		return 0, false
	}
	return n, true
}

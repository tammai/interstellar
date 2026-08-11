package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/tammai/interstellar/internal/sources"
	"github.com/tammai/interstellar/internal/store"
)

// --- topics ---

type topicDTO struct {
	ID         int64   `json:"id"`
	Title      string  `json:"title"`
	Definition string  `json:"definition"`
	Priority   int     `json:"priority"`
	Active     bool    `json:"active"`
	PosX       float64 `json:"pos_x"`
	PosY       float64 `json:"pos_y"`
	NeedsSetup bool    `json:"needs_setup"`
	Signals7d  int     `json:"signals_7d"`
}

func (s *Server) handleListTopics(w http.ResponseWriter, r *http.Request) {
	topics, err := s.DB.AllTopics()
	if err != nil {
		s.error(w, http.StatusInternalServerError, "could not read topics", err)
		return
	}

	// Recent signal counts give the settings screen something honest to show:
	// a topic that has produced nothing in a week is either badly defined or
	// genuinely dry, and either way the owner should see it.
	counts, err := s.DB.SignalCountsByTopic(7)
	if err != nil {
		s.error(w, http.StatusInternalServerError, "could not read signal counts", err)
		return
	}

	out := make([]topicDTO, 0, len(topics))
	for _, t := range topics {
		out = append(out, topicDTO{
			ID: t.ID, Title: t.Title, Definition: t.Definition,
			Priority: t.Priority, Active: t.Active,
			PosX: t.PosX, PosY: t.PosY,
			NeedsSetup: t.NeedsSetup(),
			Signals7d:  counts[t.ID],
		})
	}
	s.json(w, http.StatusOK, out)
}

type topicRequest struct {
	Title      *string `json:"title"`
	Definition *string `json:"definition"`
	Priority   *int    `json:"priority"`
	Active     *bool   `json:"active"`
}

func (s *Server) handleCreateTopic(w http.ResponseWriter, r *http.Request) {
	var req topicRequest
	if err := decodeJSON(w, r, &req); err != nil {
		s.error(w, http.StatusBadRequest, "invalid JSON body", nil)
		return
	}
	if req.Title == nil || strings.TrimSpace(*req.Title) == "" {
		s.error(w, http.StatusBadRequest, "title is required", nil)
		return
	}

	t := store.Topic{Title: strings.TrimSpace(*req.Title), Active: true}
	if req.Definition != nil {
		t.Definition = strings.TrimSpace(*req.Definition)
	}
	if req.Priority != nil {
		t.Priority = *req.Priority
	}
	if req.Active != nil {
		t.Active = *req.Active
	}

	id, err := s.DB.CreateTopic(t)
	if err != nil {
		s.error(w, http.StatusInternalServerError, "could not create topic", err)
		return
	}
	created, err := s.DB.Topic(id)
	if err != nil {
		s.error(w, http.StatusInternalServerError, "could not read new topic", err)
		return
	}
	s.json(w, http.StatusCreated, topicDTO{
		ID: created.ID, Title: created.Title, Definition: created.Definition,
		Priority: created.Priority, Active: created.Active,
		PosX: created.PosX, PosY: created.PosY, NeedsSetup: created.NeedsSetup(),
	})
}

func (s *Server) handleUpdateTopic(w http.ResponseWriter, r *http.Request) {
	id, ok := s.pathID(w, r)
	if !ok {
		return
	}

	existing, err := s.DB.Topic(id)
	if errors.Is(err, store.ErrNotFound) {
		s.error(w, http.StatusNotFound, "no such topic", nil)
		return
	}
	if err != nil {
		s.error(w, http.StatusInternalServerError, "could not read topic", err)
		return
	}

	var req topicRequest
	if err := decodeJSON(w, r, &req); err != nil {
		s.error(w, http.StatusBadRequest, "invalid JSON body", nil)
		return
	}
	if req.Title != nil {
		if strings.TrimSpace(*req.Title) == "" {
			s.error(w, http.StatusBadRequest, "title cannot be empty", nil)
			return
		}
		existing.Title = strings.TrimSpace(*req.Title)
	}
	if req.Definition != nil {
		existing.Definition = strings.TrimSpace(*req.Definition)
	}
	if req.Priority != nil {
		existing.Priority = *req.Priority
	}
	if req.Active != nil {
		existing.Active = *req.Active
	}

	if err := s.DB.UpdateTopic(existing); err != nil {
		s.error(w, http.StatusInternalServerError, "could not update topic", err)
		return
	}
	s.json(w, http.StatusOK, topicDTO{
		ID: existing.ID, Title: existing.Title, Definition: existing.Definition,
		Priority: existing.Priority, Active: existing.Active,
		PosX: existing.PosX, PosY: existing.PosY, NeedsSetup: existing.NeedsSetup(),
	})
}

func (s *Server) handleDeleteTopic(w http.ResponseWriter, r *http.Request) {
	id, ok := s.pathID(w, r)
	if !ok {
		return
	}
	err := s.DB.DeleteTopic(id)
	if errors.Is(err, store.ErrNotFound) {
		s.error(w, http.StatusNotFound, "no such topic", nil)
		return
	}
	if err != nil {
		s.error(w, http.StatusInternalServerError, "could not delete topic", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// --- sources ---

type sourceDTO struct {
	ID        int64           `json:"id"`
	Kind      string          `json:"kind"`
	Name      string          `json:"name"`
	Config    json.RawMessage `json:"config"`
	Active    bool            `json:"active"`
	LastRunAt string          `json:"last_run_at,omitempty"`
	// ItemCount is how many items this source has contributed. The honest
	// measure of whether a feed is pulling its weight.
	ItemCount int `json:"item_count"`
}

func (s *Server) handleListSources(w http.ResponseWriter, r *http.Request) {
	srcs, err := s.DB.AllSources()
	if err != nil {
		s.error(w, http.StatusInternalServerError, "could not read sources", err)
		return
	}
	counts, err := s.DB.ItemCountsBySource()
	if err != nil {
		s.error(w, http.StatusInternalServerError, "could not read item counts", err)
		return
	}

	out := make([]sourceDTO, 0, len(srcs))
	for _, src := range srcs {
		d := sourceDTO{
			ID: src.ID, Kind: src.Kind, Name: src.Name,
			Config: src.Config, Active: src.Active,
			ItemCount: counts[src.ID],
		}
		if src.LastRunAt.Valid {
			d.LastRunAt = src.LastRunAt.String
		}
		out = append(out, d)
	}
	s.json(w, http.StatusOK, out)
}

type sourceRequest struct {
	Kind   *string          `json:"kind"`
	Name   *string          `json:"name"`
	Config *json.RawMessage `json:"config"`
	Active *bool            `json:"active"`
}

func (s *Server) handleCreateSource(w http.ResponseWriter, r *http.Request) {
	var req sourceRequest
	if err := decodeJSON(w, r, &req); err != nil {
		s.error(w, http.StatusBadRequest, "invalid JSON body", nil)
		return
	}
	if req.Kind == nil || req.Name == nil ||
		strings.TrimSpace(*req.Kind) == "" || strings.TrimSpace(*req.Name) == "" {
		s.error(w, http.StatusBadRequest, "kind and name are required", nil)
		return
	}
	kind := strings.TrimSpace(*req.Kind)
	if _, err := sources.For(kind); err != nil {
		s.error(w, http.StatusBadRequest, err.Error(), nil)
		return
	}

	src := store.Source{Kind: kind, Name: strings.TrimSpace(*req.Name), Active: true}
	if req.Config != nil {
		src.Config = *req.Config
	}
	if req.Active != nil {
		src.Active = *req.Active
	}

	id, err := s.DB.CreateSource(src)
	if err != nil {
		s.error(w, http.StatusInternalServerError, "could not create source", err)
		return
	}
	created, err := s.DB.SourceByID(id)
	if err != nil {
		s.error(w, http.StatusInternalServerError, "could not read new source", err)
		return
	}
	s.json(w, http.StatusCreated, sourceDTO{
		ID: created.ID, Kind: created.Kind, Name: created.Name,
		Config: created.Config, Active: created.Active,
	})
}

func (s *Server) handleUpdateSource(w http.ResponseWriter, r *http.Request) {
	id, ok := s.pathID(w, r)
	if !ok {
		return
	}
	existing, err := s.DB.SourceByID(id)
	if errors.Is(err, store.ErrNotFound) {
		s.error(w, http.StatusNotFound, "no such source", nil)
		return
	}
	if err != nil {
		s.error(w, http.StatusInternalServerError, "could not read source", err)
		return
	}

	var req sourceRequest
	if err := decodeJSON(w, r, &req); err != nil {
		s.error(w, http.StatusBadRequest, "invalid JSON body", nil)
		return
	}
	if req.Kind != nil {
		kind := strings.TrimSpace(*req.Kind)
		if _, err := sources.For(kind); err != nil {
			s.error(w, http.StatusBadRequest, err.Error(), nil)
			return
		}
		existing.Kind = kind
	}
	if req.Name != nil {
		if strings.TrimSpace(*req.Name) == "" {
			s.error(w, http.StatusBadRequest, "name cannot be empty", nil)
			return
		}
		existing.Name = strings.TrimSpace(*req.Name)
	}
	if req.Config != nil {
		existing.Config = *req.Config
	}
	if req.Active != nil {
		existing.Active = *req.Active
	}

	if err := s.DB.UpdateSource(existing); err != nil {
		s.error(w, http.StatusInternalServerError, "could not update source", err)
		return
	}
	s.json(w, http.StatusOK, sourceDTO{
		ID: existing.ID, Kind: existing.Kind, Name: existing.Name,
		Config: existing.Config, Active: existing.Active,
	})
}

func (s *Server) handleDeleteSource(w http.ResponseWriter, r *http.Request) {
	id, ok := s.pathID(w, r)
	if !ok {
		return
	}
	err := s.DB.DeleteSource(id)
	if errors.Is(err, store.ErrNotFound) {
		s.error(w, http.StatusNotFound, "no such source", nil)
		return
	}
	if err != nil {
		s.error(w, http.StatusInternalServerError, "could not delete source", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// --- helpers ---

func (s *Server) pathID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		s.error(w, http.StatusBadRequest, "id must be a number", nil)
		return 0, false
	}
	return id, true
}

// decodeJSON reads a request body with a size limit and rejects unknown fields,
// so a typo'd key is an error rather than a silently ignored edit.
func decodeJSON(w http.ResponseWriter, r *http.Request, v any) error {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 256<<10))
	dec.DisallowUnknownFields()
	return dec.Decode(v)
}

package httpapi

import (
	"context"
	"net/http"
	"strings"

	"github.com/tammai/interstellar/internal/sources"
	"github.com/tammai/interstellar/internal/store"
)

type inboxRequest struct {
	URL string `json:"url"`
}

type inboxResponse struct {
	Title   string `json:"title"`
	URL     string `json:"url"`
	Summary string `json:"summary,omitempty"`
	// New is false when this link was already in the database. Reported rather than
	// treated as an error: pasting something twice is a normal thing to do, and the
	// honest answer is "already saved", not a failure.
	New bool `json:"new"`
	// Note tells the reader what happens next, which is not "it appears in the sky"
	// -- it appears after the next classify pass.
	Note string `json:"note"`
}

// handleInbox saves a pasted link as an item, so it reaches the classifier.
//
// This is the sanctioned path for the sources the spec puts permanently out of
// reach: LinkedIn scraping is on the "never" list on ToS grounds, and the X API on
// pricing. Pasting a URL you are already looking at breaks neither, and it is the
// only way something from those places can enter the pipeline.
//
// The link is fetched here, synchronously, on purpose. The owner is watching, and
// "saved" that turns out to mean "saved a dead URL with no title" is worse than a
// two-second wait -- the classifier judges on title and summary, so an item with
// neither can never match anything.
func (s *Server) handleInbox(w http.ResponseWriter, r *http.Request) {
	var req inboxRequest
	if err := decodeJSON(w, r, &req); err != nil {
		s.error(w, http.StatusBadRequest, "invalid JSON body", nil)
		return
	}

	raw := strings.TrimSpace(req.URL)
	if raw == "" {
		s.error(w, http.StatusBadRequest, "a URL is required", nil)
		return
	}
	// A bare domain pasted from a browser bar is a normal thing to hand over.
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}

	canonical, err := sources.Canonical(raw)
	if err != nil {
		s.error(w, http.StatusBadRequest, "that does not look like a link: "+err.Error(), nil)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), sources.FetchTimeout)
	defer cancel()

	page, err := sources.FetchPage(ctx, canonical)
	if err != nil {
		// 502, not 400: the request was fine, the page was not.
		s.error(w, http.StatusBadGateway, "could not read that page: "+err.Error(), nil)
		return
	}

	// Canonicalize again. FetchPage reports where redirects landed, and a shortener
	// or a tracking wrapper is exactly the case where the final URL is the one that
	// must be deduplicated against.
	finalURL, err := sources.Canonical(page.URL)
	if err != nil {
		finalURL = canonical
	}

	sourceID, err := s.DB.ManualSourceID()
	if err != nil {
		s.error(w, http.StatusInternalServerError, "could not prepare the inbox", err)
		return
	}

	_, isNew, err := s.DB.UpsertItem(store.NewItem{
		SourceID:     sourceID,
		CanonicalURL: finalURL,
		Title:        page.Title,
		Summary:      page.Summary,
		Detail:       "pasted",
	})
	if err != nil {
		s.error(w, http.StatusInternalServerError, "could not save that link", err)
		return
	}

	note := "Saved. It will be classified on the next run — press refresh on the star map to do that now."
	if !isNew {
		note = "Already saved; this link is in your items."
	}

	s.json(w, http.StatusOK, inboxResponse{
		Title: page.Title, URL: finalURL, Summary: page.Summary,
		New: isNew, Note: note,
	})
}

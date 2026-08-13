package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"

	"github.com/tammai/interstellar/internal/config"
	"github.com/tammai/interstellar/internal/sources"
	"github.com/tammai/interstellar/internal/store"
)

// importVerifyWorkers bounds concurrent verification fetches. A bundle can carry
// sixty feeds and each is a network round trip, so this is the difference between
// an import that answers and one that times out. Kept modest: these are sixty
// different hosts, but the local process still has to hold the sockets.
const importVerifyWorkers = 8

// maxImportSources bounds a bundle. Well above any real source list, low enough
// that a malformed or hostile file cannot make the server fetch forever.
const maxImportSources = 500

// handleExportSources returns the source list as a portable bundle.
//
// Content-Disposition so a browser saves it rather than rendering it: this is a
// file the owner keeps, and the point of it is to be moved to another instance.
func (s *Server) handleExportSources(w http.ResponseWriter, r *http.Request) {
	bundle, err := s.DB.ExportSources()
	if err != nil {
		s.error(w, http.StatusInternalServerError, "could not read sources", err)
		return
	}

	w.Header().Set("Content-Disposition", `attachment; filename="interstellar-sources.json"`)
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)

	// Indented: the whole value of an export is that a person can read it, diff it
	// against another instance's, and hand-edit a line before importing.
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if err := enc.Encode(bundle); err != nil {
		s.Log.Error("encode source export", "err", err)
	}
}

type importRequest struct {
	store.SourceBundle
	// Verify runs each new source through the real fetcher before accepting it.
	// Defaults to true -- see handleImportSources for why that default matters.
	Verify *bool `json:"verify"`
}

type importResponse struct {
	Added    int                   `json:"added"`
	Skipped  int                   `json:"skipped"`
	Rejected int                   `json:"rejected"`
	Verified bool                  `json:"verified"`
	Results  []store.ImportOutcome `json:"results"`
}

// handleImportSources merges a bundle into this instance's source list.
//
// Verification is on by default, and that is the load-bearing decision here.
// CLAUDE.md's rule -- never accept a source URL you have not run through the real
// fetcher -- exists because a feed can return 200 in a browser and yield zero
// items, and because a YouTube channel id scraped from page HTML can serve someone
// else's channel entirely. An import is precisely that risk arriving in bulk, so
// each candidate is fetched with the app's own parser and its item count reported.
//
// A source that fails verification is added INACTIVE rather than dropped. Dropping
// it loses part of the owner's list with only a line of JSON output to say so;
// inactive-with-a-reason shows up in Settings, where it can be looked at.
func (s *Server) handleImportSources(w http.ResponseWriter, r *http.Request) {
	var req importRequest
	if err := decodeJSON(w, r, &req); err != nil {
		s.error(w, http.StatusBadRequest, "invalid JSON body", nil)
		return
	}

	if req.Version != store.SourceBundleVersion {
		s.error(w, http.StatusBadRequest, fmt.Sprintf(
			"this file is version %d; this instance reads version %d",
			req.Version, store.SourceBundleVersion), nil)
		return
	}
	if len(req.Sources) == 0 {
		s.error(w, http.StatusBadRequest, "the file lists no sources", nil)
		return
	}
	if len(req.Sources) > maxImportSources {
		s.error(w, http.StatusBadRequest, fmt.Sprintf(
			"the file lists %d sources; the limit is %d", len(req.Sources), maxImportSources), nil)
		return
	}

	verify := true
	if req.Verify != nil {
		verify = *req.Verify
	}

	existing, err := s.DB.SourceIdentities()
	if err != nil {
		s.error(w, http.StatusInternalServerError, "could not read sources", err)
		return
	}

	cfg, err := config.Load(s.DB)
	if err != nil {
		s.error(w, http.StatusInternalServerError, "could not read settings", err)
		return
	}

	out := importResponse{Verified: verify}

	// Sort candidates into what needs doing, before touching the database or the
	// network. Shape errors are cheap and are caught here.
	type candidate struct {
		src   store.SourceExport
		index int
	}
	var toVerify []candidate
	results := make([]store.ImportOutcome, len(req.Sources))
	seen := map[string]bool{}

	for i, src := range req.Sources {
		outcome := store.ImportOutcome{Kind: src.Kind, Name: strings.TrimSpace(src.Name)}

		switch {
		case outcome.Name == "":
			outcome.Status, outcome.Reason = "rejected", "no name"
		case sources.Registry[src.Kind] == nil:
			outcome.Status, outcome.Reason = "rejected", "unknown kind "+src.Kind
		case len(src.Config) == 0 || !json.Valid(src.Config):
			outcome.Status, outcome.Reason = "rejected", "config is not valid JSON"
		case seen[src.Identity()]:
			outcome.Status, outcome.Reason = "skipped", "listed twice in this file"
		case existing[src.Identity()] != 0:
			outcome.Status, outcome.Reason = "skipped", "already configured here"
		default:
			seen[src.Identity()] = true
			toVerify = append(toVerify, candidate{src: src, index: i})
			outcome.Status = "added"
		}
		results[i] = outcome
	}

	// Verify concurrently. Read-only and off the database entirely, so this cannot
	// contend with the single connection.
	if verify && len(toVerify) > 0 {
		var wg sync.WaitGroup
		sem := make(chan struct{}, importVerifyWorkers)

		for _, c := range toVerify {
			wg.Add(1)
			go func(c candidate) {
				defer wg.Done()
				sem <- struct{}{}
				defer func() { <-sem }()

				n, err := verifySource(r.Context(), c.src, cfg)
				switch {
				case err != nil:
					results[c.index].Status = "added_inactive"
					results[c.index].Reason = "did not fetch: " + err.Error()
				case n == 0:
					// The exact case the convention warns about: it answered, and
					// carried nothing.
					results[c.index].Status = "added_inactive"
					results[c.index].Reason = "fetched but returned no items"
				default:
					results[c.index].Items = n
				}
			}(c)
		}
		wg.Wait()
	}

	// Now write, one goroutine, in file order so the result reads top to bottom.
	for _, c := range toVerify {
		src := c.src
		if results[c.index].Status == "added_inactive" {
			src.Active = false
		}
		if _, err := s.DB.CreateSource(store.Source{
			Kind: src.Kind, Name: strings.TrimSpace(src.Name),
			Config: src.Config, Active: src.Active,
		}); err != nil {
			results[c.index].Status = "rejected"
			results[c.index].Reason = "could not save"
			s.Log.Error("import source", "name", src.Name, "err", err)
		}
	}

	for _, res := range results {
		switch res.Status {
		case "added", "added_inactive":
			out.Added++
		case "skipped":
			out.Skipped++
		case "rejected":
			out.Rejected++
		}
	}
	out.Results = results

	s.json(w, http.StatusOK, out)
}

// verifySource fetches a candidate with the app's own parser and reports how many
// items it actually yielded.
func verifySource(ctx context.Context, src store.SourceExport, cfg config.Runtime) (int, error) {
	fetcher, err := sources.For(src.Kind)
	if err != nil {
		return 0, err
	}

	ctx, cancel := context.WithTimeout(ctx, sources.FetchTimeout)
	defer cancel()

	items, err := fetcher.Fetch(ctx, src.Config, sources.Options{HNMinPoints: cfg.HNMinPoints})
	if err != nil {
		return 0, err
	}
	return len(items), nil
}

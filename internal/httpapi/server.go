// Package httpapi serves the embedded UI and the JSON API on a single port.
package httpapi

import (
	"encoding/json"
	"io/fs"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/tammai/interstellar/internal/config"
	"github.com/tammai/interstellar/internal/pipeline"
	"github.com/tammai/interstellar/internal/store"
)

// Server holds the dependencies handlers need. It deliberately does not hold a
// resolved config snapshot: handlers and runs read settings fresh, so a provider
// swap takes effect without a restart (§3.1).
type Server struct {
	DB  *store.DB
	Env config.Env
	Web fs.FS
	// WebFromDisk reports that Web is a directory on disk rather than the
	// embedded copy, which changes how the assets may be cached. See withNoStore.
	WebFromDisk bool
	Log         *slog.Logger
	Runner      *pipeline.Runner
	Scheduler   *pipeline.Scheduler
	boot        time.Time
}

// New builds a Server. web is the embedded UI filesystem, rooted at the
// directory containing index.html.
func New(
	db *store.DB,
	env config.Env,
	web fs.FS,
	runner *pipeline.Runner,
	scheduler *pipeline.Scheduler,
	log *slog.Logger,
) *Server {
	return &Server{
		DB: db, Env: env, Web: web,
		Runner: runner, Scheduler: scheduler,
		Log: log, boot: time.Now(),
	}
}

// Routes returns the full handler tree.
func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /api/health", s.handleHealth)

	// The sky and its detail panel: the front door.
	mux.HandleFunc("GET /api/sky", s.handleSky)
	mux.HandleFunc("GET /api/signals/{id}", s.handleSignal)

	// Search: no model, no run. Local and fast enough to type into.
	mux.HandleFunc("GET /api/search", s.handleSearch)

	// The manual-forward inbox: the sanctioned path for sources the spec puts out
	// of reach (LinkedIn on ToS grounds, X on pricing).
	mux.HandleFunc("POST /api/inbox", s.handleInbox)

	// Chat. Its own screen at /chat/, served statically like the rest of the UI.
	mux.HandleFunc("POST /api/ask", s.handleAsk)
	mux.HandleFunc("GET /api/chat/history", s.handleChatHistory)
	// Clears the transcript only. The chat budget lives in llm_calls and is not
	// refunded -- see handleClearChat.
	mux.HandleFunc("DELETE /api/chat/history", s.handleClearChat)

	// The weekly round-up, and delivery.
	mux.HandleFunc("GET /api/weekly", s.handleWeekly)
	mux.HandleFunc("POST /api/weekly/run", s.handleWeeklyRun)
	// Sends a round-up that is already written. Distinct from weekly/run, which
	// composes one -- see handleWeeklySend.
	mux.HandleFunc("POST /api/weekly/send", s.handleWeeklySend)
	mux.HandleFunc("POST /api/delivery/test", s.handleDeliveryTest)

	// Run control and progress.
	mux.HandleFunc("POST /api/refresh", s.handleRefresh)
	mux.HandleFunc("GET /api/status", s.handleStatus)

	// Settings.
	mux.HandleFunc("GET /api/settings", s.handleGetSettings)
	mux.HandleFunc("PUT /api/settings", s.handlePutSettings)
	mux.HandleFunc("POST /api/model/test", s.handleModelTest)
	// One provider at a time, so switching a vendor off does not mean rewriting
	// the whole registry from a stale copy of it.
	mux.HandleFunc("PATCH /api/providers/{name}", s.handleUpdateProvider)

	// Ahead of GET /api/topics only for readability; the patterns are distinct
	// literals, so ServeMux does not have to choose between them.
	mux.HandleFunc("GET /api/topics/stats", s.handleTopicStats)
	mux.HandleFunc("GET /api/models/stats", s.handleModelStats)

	// Constellation history: read-only over signals, no model, free to poll.
	mux.HandleFunc("GET /api/trending", s.handleTrending)

	// The world model. Read-only apart from the candidate review, which is the
	// one place the owner's judgment enters it.
	mux.HandleFunc("GET /api/entities", s.handleListEntities)
	mux.HandleFunc("GET /api/claims", s.handleListClaims)
	mux.HandleFunc("GET /api/deltas", s.handleListDeltas)
	mux.HandleFunc("GET /api/candidates", s.handleListCandidates)
	mux.HandleFunc("POST /api/candidates/{id}/promote", s.handlePromoteCandidate)
	mux.HandleFunc("DELETE /api/candidates/{id}", s.handleDismissCandidate)

	mux.HandleFunc("GET /api/topics", s.handleListTopics)
	mux.HandleFunc("POST /api/topics", s.handleCreateTopic)
	mux.HandleFunc("POST /api/topics/{id}/sources/suggest", s.handleSuggestSources)
	mux.HandleFunc("PATCH /api/topics/{id}", s.handleUpdateTopic)
	mux.HandleFunc("DELETE /api/topics/{id}", s.handleDeleteTopic)

	// Ahead of the {id} routes for readability; distinct literals, so no ambiguity.
	mux.HandleFunc("GET /api/sources/export", s.handleExportSources)
	mux.HandleFunc("POST /api/sources/import", s.handleImportSources)

	mux.HandleFunc("GET /api/sources", s.handleListSources)
	mux.HandleFunc("POST /api/sources", s.handleCreateSource)
	mux.HandleFunc("PATCH /api/sources/{id}", s.handleUpdateSource)
	mux.HandleFunc("DELETE /api/sources/{id}", s.handleDeleteSource)

	// The UI is embedded, so this is the last route: everything not under /api
	// is a static asset or the entry point.
	mux.Handle("GET /", http.FileServerFS(s.Web))

	return s.withLogging(s.withNoStore(mux))
}

// withNoStore stops responses being cached that must not be.
//
// API responses, always: the sky changes when a run finishes, and a browser
// serving yesterday's stars from cache would look exactly like a broken
// pipeline.
//
// Static assets, only when they are being served from disk. Embedded assets are
// left cacheable because they cannot change without the binary changing, which
// is a new container. But http.FileServerFS sends Last-Modified and no
// Cache-Control, and that is enough for a browser to cache heuristically -- so
// in development an edit that is on disk, and that the server is serving, can
// still not reach the page. That is the same lost hour DevDirEnv exists to
// prevent, one step further along: there, the binary had never seen the change;
// here, the browser refuses to ask for it. Both present as "my edit did
// nothing", which is why the fix belongs next to the mechanism that causes it.
func (s *Server) withNoStore(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/") || s.WebFromDisk {
			w.Header().Set("Cache-Control", "no-store")
		}
		next.ServeHTTP(w, r)
	})
}

type healthResponse struct {
	Status        string `json:"status"`
	SchemaVersion int    `json:"schema_version"`
	UptimeSeconds int    `json:"uptime_seconds"`
	Timezone      string `json:"timezone"`
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	version, err := s.DB.Version()
	if err != nil {
		s.error(w, http.StatusInternalServerError, "schema version unavailable", err)
		return
	}
	s.json(w, http.StatusOK, healthResponse{
		Status:        "ok",
		SchemaVersion: version,
		UptimeSeconds: int(time.Since(s.boot).Seconds()),
		Timezone:      s.Env.Location.String(),
	})
}

func (s *Server) json(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(body); err != nil {
		s.Log.Error("encode response", "err", err)
	}
}

// error logs the underlying cause and returns a message safe to show the owner.
// The wrapped error is never included in the response body -- it can carry a
// provider URL or a query, and there is no reason to surface either.
func (s *Server) error(w http.ResponseWriter, status int, msg string, err error) {
	if err != nil {
		s.Log.Error(msg, "err", err, "status", status)
	}
	s.json(w, status, map[string]string{"error": msg})
}

func (s *Server) withLogging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		// Health is polled; logging it at info would bury everything else.
		level := slog.LevelInfo
		if r.URL.Path == "/api/health" {
			level = slog.LevelDebug
		}
		s.Log.Log(r.Context(), level, "request",
			"method", r.Method,
			"path", r.URL.Path,
			"status", rec.status,
			"ms", time.Since(start).Milliseconds(),
		)
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

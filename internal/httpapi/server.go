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
	DB        *store.DB
	Env       config.Env
	Web       fs.FS
	Log       *slog.Logger
	Runner    *pipeline.Runner
	Scheduler *pipeline.Scheduler
	boot      time.Time
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

	// Run control and progress.
	mux.HandleFunc("POST /api/refresh", s.handleRefresh)
	mux.HandleFunc("GET /api/status", s.handleStatus)

	// Settings.
	mux.HandleFunc("GET /api/settings", s.handleGetSettings)
	mux.HandleFunc("PUT /api/settings", s.handlePutSettings)
	mux.HandleFunc("POST /api/model/test", s.handleModelTest)

	mux.HandleFunc("GET /api/topics", s.handleListTopics)
	mux.HandleFunc("POST /api/topics", s.handleCreateTopic)
	mux.HandleFunc("PATCH /api/topics/{id}", s.handleUpdateTopic)
	mux.HandleFunc("DELETE /api/topics/{id}", s.handleDeleteTopic)

	mux.HandleFunc("GET /api/sources", s.handleListSources)
	mux.HandleFunc("POST /api/sources", s.handleCreateSource)
	mux.HandleFunc("PATCH /api/sources/{id}", s.handleUpdateSource)
	mux.HandleFunc("DELETE /api/sources/{id}", s.handleDeleteSource)

	// The UI is embedded, so this is the last route: everything not under /api
	// is a static asset or the entry point.
	mux.Handle("GET /", http.FileServerFS(s.Web))

	return s.withLogging(s.withNoStore(mux))
}

// withNoStore stops API responses being cached.
//
// The sky changes when a run finishes, and a browser serving yesterday's stars
// from cache would look exactly like a broken pipeline. Static assets are left
// cacheable.
func (s *Server) withNoStore(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/") {
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

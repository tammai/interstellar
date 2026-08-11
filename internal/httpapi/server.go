// Package httpapi serves the embedded UI and the JSON API on a single port.
package httpapi

import (
	"encoding/json"
	"io/fs"
	"log/slog"
	"net/http"
	"time"

	"github.com/tammai/interstellar/internal/config"
	"github.com/tammai/interstellar/internal/store"
)

// Server holds the dependencies handlers need. It deliberately does not hold a
// resolved config snapshot: handlers and runs read settings fresh, so a provider
// swap takes effect without a restart (§3.1).
type Server struct {
	DB   *store.DB
	Env  config.Env
	Web  fs.FS
	Log  *slog.Logger
	boot time.Time
}

// New builds a Server. web is the embedded UI filesystem, rooted at the
// directory containing index.html.
func New(db *store.DB, env config.Env, web fs.FS, log *slog.Logger) *Server {
	return &Server{DB: db, Env: env, Web: web, Log: log, boot: time.Now()}
}

// Routes returns the full handler tree.
func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /api/health", s.handleHealth)

	// The UI is embedded, so this is the last route: everything not under /api
	// is a static asset or the SPA entry point.
	mux.Handle("GET /", http.FileServerFS(s.Web))

	return s.withLogging(mux)
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

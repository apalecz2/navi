// Package httpapi builds the HTTP server: the mux, the middleware chain, and
// the handlers.
//
// Routing is stdlib net/http.ServeMux. Since Go 1.22 it handles method matching
// and path wildcards, which covers every route in docs/07-api-spec.md, and
// middleware is ordinary func(http.Handler) http.Handler.
package httpapi

import (
	"log/slog"
	"net/http"
	"time"

	"github.com/aidenpaleczny/navi/internal/config"
	"github.com/aidenpaleczny/navi/internal/health"
	"github.com/aidenpaleczny/navi/internal/metrics"
)

// Server holds what the handlers read. Later sessions add the repository here;
// nothing in this struct is written after New returns.
type Server struct {
	log     *slog.Logger
	health  *health.Registry
	metrics *metrics.Metrics
}

// New builds the http.Server. It takes the HTTP config group rather than the
// whole configuration, so a handler cannot reach a credential it has no
// business with.
func New(cfg config.HTTP, log *slog.Logger, h *health.Registry, m *metrics.Metrics) *http.Server {
	s := &Server{log: log, health: h, metrics: m}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.handleHealthz)

	// /metrics is served on the same listener but is deliberately absent from
	// the tunnel ingress table: it carries no secrets, but it describes usage
	// patterns in detail and has no reason to leave the host.
	mux.Handle("GET /metrics", m.Handler())

	return &http.Server{
		Addr:    cfg.Addr,
		Handler: recoverPanic(log)(logRequests(log)(mux)),

		// A read header timeout is the one timeout that is always right to set:
		// without it a stalled client holds a connection open indefinitely.
		ReadHeaderTimeout: 10 * time.Second,
	}
}

// logRequests logs one line per request at debug, and elevates 5xx to error.
func logRequests(log *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}

			next.ServeHTTP(rec, r)

			level := slog.LevelDebug
			if rec.status >= http.StatusInternalServerError {
				level = slog.LevelError
			}
			log.LogAttrs(r.Context(), level, "request",
				slog.String("method", r.Method),
				slog.String("path", r.URL.Path),
				slog.Int("status", rec.status),
				slog.Duration("took", time.Since(start)),
			)
		})
	}
}

// recoverPanic keeps one bad handler from taking the process down. It is the
// only recover outside the supervisor's per-tick one.
func recoverPanic(log *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				if rec := recover(); rec != nil {
					log.Error("panic in handler",
						"method", r.Method, "path", r.URL.Path, "err", rec)
					w.WriteHeader(http.StatusInternalServerError)
				}
			}()
			next.ServeHTTP(w, r)
		})
	}
}

type statusRecorder struct {
	http.ResponseWriter
	status  int
	written bool
}

func (r *statusRecorder) WriteHeader(status int) {
	if r.written {
		return
	}
	r.written = true
	r.status = status
	r.ResponseWriter.WriteHeader(status)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	r.written = true
	return r.ResponseWriter.Write(b)
}

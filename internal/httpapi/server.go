// Package httpapi builds the HTTP server: the mux, the middleware chain, and
// the handlers.
//
// Routing is stdlib net/http.ServeMux. Since Go 1.22 it handles method matching
// and path wildcards, which covers every route in docs/07-api-spec.md, and
// middleware is ordinary func(http.Handler) http.Handler.
package httpapi

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/aidenpaleczny/navi/internal/config"
	"github.com/aidenpaleczny/navi/internal/domain"
	"github.com/aidenpaleczny/navi/internal/health"
	"github.com/aidenpaleczny/navi/internal/metrics"
	"github.com/aidenpaleczny/navi/internal/store"
)

// Store is the slice of the repository the handlers in this package use. It is
// declared here, by the consumer, rather than exported by internal/store: this
// package depends on three methods, and saying so is what keeps a handler from
// reaching a query it has no business with as the repository grows.
type Store interface {
	Ping(ctx context.Context) error
	PendingOverdue(ctx context.Context, floor time.Time) (int, error)
	Horizon(ctx context.Context) (int, bool, error)

	// ResolveOccurrence carries the whole of the resolution decision, so the
	// handler maps an outcome onto a status code and judges nothing itself.
	ResolveOccurrence(ctx context.Context, id string, to domain.Status, note *string,
		source domain.ResolutionSource, now time.Time) (store.Resolution, error)
}

// Server holds what the handlers read. Nothing in this struct is written after
// New returns.
type Server struct {
	log     *slog.Logger
	health  *health.Registry
	metrics *metrics.Metrics
	store   Store

	// claimFloor is the scheduler's oldest firable start time. It is a value
	// rather than a callback because the floor is fixed at process start and
	// never moves; passing it in is what makes /healthz count exactly what the
	// scheduler would claim.
	claimFloor time.Time
}

// New builds the http.Server. It takes the HTTP config group rather than the
// whole configuration, so a handler cannot reach a credential it has no
// business with.
//
// chatWebhook is the conversational transport's own inbound handler —
// *telegram.Inbound today, mounted the same way m.Handler() is: the package
// that owns a wire format hands this one a ready http.Handler rather than
// this package importing telegram's internals. nil means CHAT_TRANSPORT
// names nothing (still the default outside P1 testing), in which case the
// route is never registered and a POST to it 404s from the mux itself rather
// than reaching a handler with nothing configured to verify against.
func New(cfg config.HTTP, log *slog.Logger, h *health.Registry, m *metrics.Metrics, st Store, claimFloor time.Time, chatWebhook http.Handler) *http.Server {
	s := &Server{log: log, health: h, metrics: m, store: st, claimFloor: claimFloor}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.handleHealthz)

	// The first /api route. Everything under /api is authenticated by
	// Cloudflare Access at the tunnel edge, one-time PIN
	// (docs/07-api-spec.md#authentication, ops/cloudflared-ingress.md) — there
	// is no in-process check here because there is no session for one to read,
	// and D-014's "auth is resolved before the handler runs" is that decision.
	mux.HandleFunc("POST /api/occurrences/{id}/resolve", s.handleResolveOccurrence)

	// /metrics is served on the same listener but is deliberately absent from
	// the tunnel ingress table: it carries no secrets, but it describes usage
	// patterns in detail and has no reason to leave the host.
	mux.Handle("GET /metrics", m.Handler())

	if chatWebhook != nil {
		// Authenticated by its own shared secret (D8), not by Cloudflare
		// Access — Telegram has no session to put behind it.
		mux.Handle("POST /webhook/telegram", chatWebhook)
	}

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

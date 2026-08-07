// Command navi is the whole service: an HTTP API and five background loops in
// one process, in one container (D-019).
//
// This file is wiring and nothing else. It loads configuration, builds the
// logger and the two registries, registers the loops, starts the server, and
// shuts everything down in the order D12 requires. Anything that makes a
// decision belongs in a package.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"syscall"
	"time"

	// A scratch image has no /usr/share/zoneinfo, and every schedule in this
	// system resolves against an IANA zone. Embedding the database is what keeps
	// a missing tzdb from looking like correct behaviour until the first DST
	// boundary.
	_ "time/tzdata"

	"github.com/aidenpaleczny/navi/internal/config"
	"github.com/aidenpaleczny/navi/internal/copywriter"
	"github.com/aidenpaleczny/navi/internal/defaults"
	"github.com/aidenpaleczny/navi/internal/domain"
	"github.com/aidenpaleczny/navi/internal/health"
	"github.com/aidenpaleczny/navi/internal/httpapi"
	"github.com/aidenpaleczny/navi/internal/materializer"
	"github.com/aidenpaleczny/navi/internal/metrics"
	"github.com/aidenpaleczny/navi/internal/reconciler"
	"github.com/aidenpaleczny/navi/internal/schedule"
	"github.com/aidenpaleczny/navi/internal/scheduler"
	"github.com/aidenpaleczny/navi/internal/store"
	"github.com/aidenpaleczny/navi/internal/supervisor"
	"github.com/aidenpaleczny/navi/internal/sweeper"
	"github.com/aidenpaleczny/navi/internal/transport/logging"
)

// shutdownTimeout bounds each phase of shutdown: draining HTTP, then joining
// the loops. Both are expected to take milliseconds.
const shutdownTimeout = 5 * time.Second

// scrapeTimeout bounds the query behind navi_pending_overdue. A scrape must not
// be able to hold a connection longer than the interval between scrapes.
const scrapeTimeout = 2 * time.Second

func main() {
	if err := run(); err != nil {
		// Configuration failures happen before the logger exists, and a
		// container that exits during startup leaves nothing behind but this
		// line, so it names the variable and the reason.
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: cfg.Log.Level}))
	slog.SetDefault(log)
	log.Info("starting", "config", cfg)

	// The vocabulary table is read once, here, and handed to whatever needs it
	// (D-016). Reading it before the store means a typo in defaults.yaml is a
	// failed boot with a line naming the offending key, rather than a rejected
	// reminder three days later. Nothing consumes it until P1 wires the
	// validator into the create path and the same value into the system prompt.
	table, err := defaults.Load(cfg.Files.DefaultsPath())
	if err != nil {
		return err
	}
	if err := schedule.CheckTable(table); err != nil {
		return fmt.Errorf("config: %s: %w", cfg.Files.DefaultsPath(), err)
	}
	log.Info("vocabulary defaults loaded", "path", cfg.Files.DefaultsPath(), "table", table.Summary())

	h := health.New()
	m := metrics.New()

	// The store opens before anything that could use it, and a failure here is
	// fatal: this service is the database, and a process that starts without
	// one would report healthy loops that cannot do any work. Opening also
	// applies any pending migrations.
	st, err := store.Open(context.Background(), cfg.Data, log)
	if err != nil {
		return err
	}
	defer func() {
		if err := st.Close(); err != nil {
			log.Error("store close", "err", err)
		}
	}()

	notifier, err := notifyTransport(cfg.Transport.Notify, log)
	if err != nil {
		return err
	}
	if notifier.Name() == config.LoggingTransport {
		// The one misconfiguration that looks exactly like a working system:
		// loops tick, rows are claimed and marked notified, /healthz is green,
		// and no phone ever buzzes. It is the right default for `go run` and
		// wrong in a container, so it says so every boot.
		log.Warn("notifications are going to the log, not to a device",
			"notify_transport", notifier.Name())
	}

	// Loops run under their own context so shutdown can drain HTTP first and
	// cancel them second (D12).
	loopCtx, cancelLoops := context.WithCancel(context.Background())
	defer cancelLoops()

	// The sweeper backfills the horizon by calling the materializer, so it holds
	// the same instance the supervisor drives rather than one of its own.
	mat := materializer.New(log.With("loop", materializer.Name), st, cfg.Schedule.DefaultTZ)

	// The scheduler's recovery window is measured back from now, which is process
	// start (C9, Q-2).
	sched := scheduler.New(log.With("loop", scheduler.Name), st, notifier, m, time.Now())

	// The claim floor is fixed for the life of the process, so the gauge, the
	// health endpoint, and the claim itself are all driven by the one value
	// rather than by three thresholds that can drift.
	claimFloor := sched.ClaimFloor()
	m.RegisterPendingOverdue(func() float64 { return pendingOverdue(st, claimFloor, log) })
	m.RegisterHorizonDays(func() float64 { return horizonDays(st, log) })
	m.RegisterTransition(string(domain.StatusPending), string(domain.StatusNotified), scheduler.Source)

	sup := supervisor.New(log, h, m)
	sup.Register(
		mat.Loop(),
		sched.Loop(),
		copywriter.New(log.With("loop", copywriter.Name)).Loop(),
		reconciler.New(log.With("loop", reconciler.Name)).Loop(),
		sweeper.New(log.With("loop", sweeper.Name), st, mat).Loop(),
	)
	sup.Start(loopCtx)

	srv := httpapi.New(cfg.HTTP, log, h, m, st, claimFloor)
	serveErr := make(chan error, 1)
	go func() {
		log.Info("http listening", "addr", srv.Addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErr <- fmt.Errorf("http: serve: %w", err)
			return
		}
		serveErr <- nil
	}()

	signalCtx, stopSignals := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stopSignals()

	select {
	case err := <-serveErr:
		if err != nil {
			cancelLoops()
			sup.Wait()
			return err
		}
	case <-signalCtx.Done():
		stopSignals() // a second signal from here on kills the process outright
		log.Info("shutdown signal received")
	}

	// Drain in-flight requests first, so nothing is cut off mid-write, and only
	// then cancel the loops (D12).
	drainCtx, cancelDrain := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancelDrain()
	if err := srv.Shutdown(drainCtx); err != nil {
		log.Error("http shutdown", "err", err)
	}

	cancelLoops()
	if !waitFor(sup.Wait, shutdownTimeout) {
		log.Error("loops did not stop within timeout", "timeout", shutdownTimeout)
	}

	// Reported so the goroutine-leak check after a shutdown is a log line rather
	// than a profiling session. A clean exit leaves a handful.
	log.Info("shutdown complete", "goroutines", runtime.NumGoroutine())
	return nil
}

// notifyTransport resolves NOTIFY_TRANSPORT to an adapter.
//
// An unknown name is a boot failure rather than a fallback. A typo that quietly
// degraded to the logging transport would produce a container that ticks, claims
// rows, reports healthy, and never reaches a phone — which is the failure this
// whole switch exists to make impossible.
//
// Telegram joins this switch in session 6, which is the entire change the fire
// path needs to start delivering for real.
func notifyTransport(name string, log *slog.Logger) (scheduler.Notifier, error) {
	switch name {
	case config.LoggingTransport:
		return logging.New(log.With("transport", logging.Name)), nil
	default:
		return nil, fmt.Errorf("config: NOTIFY_TRANSPORT %q is not a known adapter", name)
	}
}

// pendingOverdue answers navi_pending_overdue at scrape time. NaN rather than
// zero when the database cannot be reached, because a gap in the series is what
// "unknown" looks like on a dashboard and zero is what "healthy" looks like.
func pendingOverdue(st *store.Store, claimFloor time.Time, log *slog.Logger) float64 {
	ctx, cancel := context.WithTimeout(context.Background(), scrapeTimeout)
	defer cancel()

	n, err := st.PendingOverdue(ctx, claimFloor)
	if err != nil {
		log.Error("metrics: pending overdue", "err", err)
		return math.NaN()
	}
	return float64(n)
}

// horizonDays answers navi_materializer_horizon_days at scrape time. NaN both
// when the database cannot be reached and when nothing has ever been
// materialized: a database with no horizon has no number to report, and zero is
// the specific reading that means the horizon ran out.
func horizonDays(st *store.Store, log *slog.Logger) float64 {
	ctx, cancel := context.WithTimeout(context.Background(), scrapeTimeout)
	defer cancel()

	days, ok, err := st.Horizon(ctx)
	if err != nil {
		log.Error("metrics: horizon", "err", err)
		return math.NaN()
	}
	if !ok {
		return math.NaN()
	}
	return float64(days)
}

// waitFor runs wait and reports whether it returned before the timeout.
func waitFor(wait func(), timeout time.Duration) bool {
	done := make(chan struct{})
	go func() {
		wait()
		close(done)
	}()
	select {
	case <-done:
		return true
	case <-time.After(timeout):
		return false
	}
}

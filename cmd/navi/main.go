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
	"github.com/aidenpaleczny/navi/internal/health"
	"github.com/aidenpaleczny/navi/internal/httpapi"
	"github.com/aidenpaleczny/navi/internal/materializer"
	"github.com/aidenpaleczny/navi/internal/metrics"
	"github.com/aidenpaleczny/navi/internal/reconciler"
	"github.com/aidenpaleczny/navi/internal/scheduler"
	"github.com/aidenpaleczny/navi/internal/supervisor"
	"github.com/aidenpaleczny/navi/internal/sweeper"
)

// shutdownTimeout bounds each phase of shutdown: draining HTTP, then joining
// the loops. Both are expected to take milliseconds.
const shutdownTimeout = 5 * time.Second

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

	h := health.New()
	m := metrics.New()

	// Loops run under their own context so shutdown can drain HTTP first and
	// cancel them second (D12).
	loopCtx, cancelLoops := context.WithCancel(context.Background())
	defer cancelLoops()

	sup := supervisor.New(log, h, m)
	sup.Register(
		materializer.New(log.With("loop", materializer.Name)).Loop(),
		scheduler.New(log.With("loop", scheduler.Name)).Loop(),
		copywriter.New(log.With("loop", copywriter.Name)).Loop(),
		reconciler.New(log.With("loop", reconciler.Name)).Loop(),
		sweeper.New(log.With("loop", sweeper.Name)).Loop(),
	)
	sup.Start(loopCtx)

	srv := httpapi.New(cfg.HTTP, log, h, m)
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

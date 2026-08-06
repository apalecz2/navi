// Package supervisor runs the background loops.
//
// Five loops with independent intervals live in this one process rather than in
// five containers (D-019), so the tradeoff this package exists to manage is that
// a crash in one loop must not take down the others. Two details carry that:
//
//   - The recover is per tick rather than per loop, so a single malformed row
//     costs one interval instead of the loop.
//   - last_tick is recorded in the deferred function, so a loop that is running
//     but failing still reports as ticking. /healthz then distinguishes
//     "stalled" from "erroring", which are different problems with different
//     fixes.
//
// Cancellation is uniform: every loop takes a context and SIGTERM cancels the
// root (D12).
package supervisor

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/aidenpaleczny/navi/internal/health"
	"github.com/aidenpaleczny/navi/internal/metrics"
)

// restartBackoff is the pause before relaunching a loop goroutine that returned
// while its context was still live. That should not happen — run only exits on
// cancellation — so the backoff exists to keep an impossible condition from
// becoming a hot loop.
const restartBackoff = time.Second

// Loop is one background task: a name, how often it runs, and what it does.
//
// Tick must respect its context and should return promptly when it is
// cancelled. It must not log its own errors — returning them is how they reach
// the log with the loop name attached, exactly once.
type Loop struct {
	Name     string
	Interval time.Duration
	Tick     func(context.Context) error
}

// Supervisor owns the loop goroutines.
type Supervisor struct {
	log     *slog.Logger
	health  *health.Registry
	metrics *metrics.Metrics

	loops []Loop
	wg    sync.WaitGroup
}

// New returns a supervisor with nothing registered.
func New(log *slog.Logger, h *health.Registry, m *metrics.Metrics) *Supervisor {
	return &Supervisor{log: log, health: h, metrics: m}
}

// Register adds a loop and declares it to the health registry and the metrics
// registry. Registering before Start is what makes a loop that never ticks
// visible on /healthz rather than absent from it.
func (s *Supervisor) Register(loops ...Loop) {
	for _, l := range loops {
		s.loops = append(s.loops, l)
		s.health.Register(l.Name, l.Interval)
		s.metrics.RegisterLoop(l.Name)
	}
}

// Start launches every registered loop. It returns immediately; Wait joins the
// goroutines after the context is cancelled.
func (s *Supervisor) Start(ctx context.Context) {
	for _, l := range s.loops {
		s.wg.Add(1)
		go func(l Loop) {
			defer s.wg.Done()
			s.supervise(ctx, l)
		}(l)
	}
	s.log.Info("loops started", "count", len(s.loops))
}

// Wait blocks until every loop goroutine has returned.
func (s *Supervisor) Wait() {
	s.wg.Wait()
}

// supervise restarts a loop that returns for any reason other than
// cancellation. run cannot return early as written, which is the point: D6 asks
// for a supervisor that restarts any loop that exits, and a guarantee that
// depends on nobody ever changing run is not a guarantee.
func (s *Supervisor) supervise(ctx context.Context, l Loop) {
	for ctx.Err() == nil {
		s.run(ctx, l)

		if ctx.Err() != nil {
			break
		}
		s.log.Error("loop exited unexpectedly, restarting", "loop", l.Name, "backoff", restartBackoff)
		select {
		case <-ctx.Done():
		case <-time.After(restartBackoff):
		}
	}
	s.log.Info("loop stopped", "loop", l.Name)
}

// run ticks the loop until its context is cancelled.
func (s *Supervisor) run(ctx context.Context, l Loop) {
	var lastStart time.Time

	for ctx.Err() == nil {
		start := time.Now()
		if !lastStart.IsZero() {
			s.metrics.ObserveTickInterval(l.Name, start.Sub(lastStart).Seconds())
		}
		lastStart = start

		if err := s.tickOnce(ctx, l); err != nil {
			s.metrics.IncLoopError(l.Name)
			s.log.Error("tick failed", "loop", l.Name, "err", err)
		}

		select {
		case <-ctx.Done():
		case <-time.After(l.Interval):
		}
	}
}

// tickOnce recovers panics so one bad row cannot kill the loop, and records
// last_tick for /healthz whether or not the body succeeded.
func (s *Supervisor) tickOnce(ctx context.Context, l Loop) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("panic in %s: %v", l.Name, r)
		}
		s.health.Observe(l.Name, time.Now())
	}()
	return l.Tick(ctx)
}

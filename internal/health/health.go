// Package health holds the in-memory record of when each background loop last
// ticked. The supervisor writes to it, the /healthz handler reads from it, and
// nothing else touches it.
//
// It is deliberately a leaf: stdlib only, no database, no configuration. It has
// to keep answering when the parts of the system that can fail have failed,
// which is the whole point of a liveness endpoint.
package health

import (
	"sync"
	"time"
)

// staleGrace is added to the staleness allowance so a loop is not reported
// unhealthy for a scheduling jitter of a few hundred milliseconds.
const staleGrace = 5 * time.Second

// staleIntervals is how many intervals a loop may miss before it counts as
// stalled. Three is enough to absorb one slow tick and its retry without
// reporting a problem that is not there.
const staleIntervals = 3

// Registry records last-tick times for a fixed set of loops.
//
// Loops are registered up front rather than on first tick, so /healthz reports
// the full set from the first request — a loop that has never ticked is a
// present entry with a null last_tick and healthy false, not a missing one. A
// registry that grew as loops reported in would render a stalled-since-startup
// loop as silence, which is the exact failure it exists to surface.
type Registry struct {
	mu    sync.Mutex
	order []string
	loops map[string]*loop

	// now is swappable so behaviour that depends on the clock stays testable by
	// hand; production always uses time.Now.
	now func() time.Time
}

type loop struct {
	interval time.Duration
	lastTick time.Time // zero means never
	ticks    uint64
}

// New returns an empty registry.
func New() *Registry {
	return &Registry{
		loops: make(map[string]*loop),
		now:   time.Now,
	}
}

// Register declares a loop and the interval it is expected to tick at.
// Registering the same name twice overwrites the interval and keeps the history.
func (r *Registry) Register(name string, interval time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if existing, ok := r.loops[name]; ok {
		existing.interval = interval
		return
	}
	r.loops[name] = &loop{interval: interval}
	r.order = append(r.order, name)
}

// Observe records that a loop ticked at the given time. It is called from the
// supervisor's deferred function whether or not the tick body succeeded: a loop
// that is running but erroring is a different problem from a loop that has
// stopped, and /healthz is where that distinction gets made.
func (r *Registry) Observe(name string, at time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()

	l, ok := r.loops[name]
	if !ok {
		l = &loop{}
		r.loops[name] = l
		r.order = append(r.order, name)
	}
	l.lastTick = at
	l.ticks++
}

// Snapshot is a consistent read of every registered loop, in registration
// order.
type Snapshot struct {
	Loops   []LoopStatus
	Healthy bool
}

// LoopStatus is one loop's state at the moment of the snapshot.
type LoopStatus struct {
	Name     string
	Interval time.Duration
	LastTick time.Time // zero means never ticked
	Ticks    uint64
	Healthy  bool
}

// Snapshot reads the whole registry. Health is computed here rather than in the
// handler because the interval is what makes the answer meaningful, and the
// registry is where the interval lives.
func (r *Registry) Snapshot() Snapshot {
	r.mu.Lock()
	defer r.mu.Unlock()

	now := r.now()
	snap := Snapshot{
		Loops:   make([]LoopStatus, 0, len(r.order)),
		Healthy: true,
	}
	for _, name := range r.order {
		l := r.loops[name]
		healthy := !l.lastTick.IsZero() &&
			now.Sub(l.lastTick) <= staleIntervals*l.interval+staleGrace
		if !healthy {
			snap.Healthy = false
		}
		snap.Loops = append(snap.Loops, LoopStatus{
			Name:     name,
			Interval: l.interval,
			LastTick: l.lastTick,
			Ticks:    l.ticks,
			Healthy:  healthy,
		})
	}
	return snap
}

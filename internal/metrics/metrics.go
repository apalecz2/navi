// Package metrics owns the Prometheus registry and every series this service
// exports. Components take a *Metrics and call a named method; nothing else
// constructs a collector, and nothing uses the default registry.
//
// The series are specified in docs/03-architecture.md#observability. This
// session registers the two loop series, which need no database. The rest —
// delivery latency, pending_overdue, transitions, the model counters — are added
// as fields on this struct by the sessions that produce them.
//
// Why this exists alongside /healthz: almost everything in this system degrades
// silently by design. A plain-title notification looks fine, a stalled
// copywriter looks fine, and /healthz reports whether loops are ticking and
// nothing about whether the work is any good (D-023).
package metrics

import (
	"net/http"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Metrics holds the registry and the collectors registered on it.
type Metrics struct {
	registry *prometheus.Registry

	loopTickInterval *prometheus.HistogramVec
	loopErrors       *prometheus.CounterVec
}

// New builds the registry and registers every series.
func New() *Metrics {
	reg := prometheus.NewRegistry()

	// Go and process collectors are registered because go_goroutines is how a
	// leaked goroutine after shutdown gets noticed, and it costs nothing.
	reg.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)

	m := &Metrics{
		registry: reg,
		loopTickInterval: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name: "navi_loop_tick_interval_seconds",
			Help: "Observed interval between consecutive tick starts, per loop.",
			// 1s to roughly two hours, which covers the 30s scheduler and the
			// hourly sweeper in one set of buckets. A loop that slows before it
			// stalls is the signal /healthz gives too late.
			Buckets: prometheus.ExponentialBuckets(1, 2, 14),
		}, []string{"loop"}),
		loopErrors: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "navi_loop_errors_total",
			Help: "Tick bodies that returned an error or panicked, per loop.",
		}, []string{"loop"}),
	}
	reg.MustRegister(m.loopTickInterval, m.loopErrors)
	return m
}

// RegisterLoop creates the child series for a loop so it exports a zero before
// it has ever errored. Without this, navi_loop_errors_total appears only after
// the first failure, and "no data" and "no errors" render identically on a
// dashboard.
func (m *Metrics) RegisterLoop(name string) {
	m.loopErrors.WithLabelValues(name)
	m.loopTickInterval.WithLabelValues(name)
}

// ObserveTickInterval records the gap between consecutive tick starts.
func (m *Metrics) ObserveTickInterval(loop string, seconds float64) {
	m.loopTickInterval.WithLabelValues(loop).Observe(seconds)
}

// IncLoopError counts one failed tick.
func (m *Metrics) IncLoopError(loop string) {
	m.loopErrors.WithLabelValues(loop).Inc()
}

// Handler serves the exposition format for this registry.
func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.registry, promhttp.HandlerOpts{
		ErrorHandling: promhttp.ContinueOnError,
	})
}

// Package metrics owns the Prometheus registry and every series this service
// exports. Components take a *Metrics and call a named method; nothing else
// constructs a collector, and nothing uses the default registry.
//
// The series are specified in docs/03-architecture.md#observability. Registered
// so far: the two loop series and the two database-backed gauges, the fire
// path's delivery latency, transitions, claim releases, and copywriter
// fallbacks, the inbound-message counters, and, since the model client
// (session 9), llm_calls' live counterpart: calls by task/tier/outcome and
// their latency by task/tier.
//
// Why this exists alongside /healthz: almost everything in this system degrades
// silently by design. A plain-title notification looks fine, a stalled
// copywriter looks fine, and /healthz reports whether loops are ticking and
// nothing about whether the work is any good (D-023).
package metrics

import (
	"net/http"
	"strconv"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Metrics holds the registry and the collectors registered on it.
type Metrics struct {
	registry *prometheus.Registry

	loopTickInterval *prometheus.HistogramVec
	loopErrors       *prometheus.CounterVec

	deliveryLatency     prometheus.Histogram
	transitions         *prometheus.CounterVec
	claimsReleased      prometheus.Counter
	copywriterFallbacks prometheus.Counter

	checkIns              prometheus.Counter
	reconciledOccurrences prometheus.Counter
	checkInFallbacks      prometheus.Counter

	briefingsSent       prometheus.Counter
	briefingFallbacks   prometheus.Counter
	briefingsUnanswered prometheus.Counter

	inboundAccepted *prometheus.CounterVec
	inboundDropped  *prometheus.CounterVec

	llmCalls   *prometheus.CounterVec
	llmLatency *prometheus.HistogramVec
}

// deliveryLatencyBuckets are explicit rather than exponential so that 60 is an
// edge. Q1 allows one minute between the scheduled time and delivery, and a
// bucket boundary exactly there makes "what fraction of reminders met it" a
// division rather than an interpolation across a bucket that straddles the
// number.
//
// The long tail is real and expected: a restart legitimately delivers rows up to
// the recovery window old, so every deploy puts observations near 900s and the
// p95 panel spikes afterwards. That is the histogram being honest, not a
// regression.
var deliveryLatencyBuckets = []float64{1, 5, 10, 15, 30, 45, 60, 90, 120, 300, 900, 1800}

// llmLatencyBuckets run from a fast tier-one reply up past the longest
// configured tier timeout (digest's tier 2, 60s in config/model.yaml), so a
// call that hit its timeout and one that returned promptly land in
// different, readable buckets rather than both landing in a +Inf tail.
var llmLatencyBuckets = []float64{0.1, 0.25, 0.5, 1, 2, 5, 10, 15, 20, 30, 45, 60, 90}

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
		deliveryLatency: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name:    "navi_delivery_latency_seconds",
			Help:    "Seconds from an occurrence's scheduled time to the notification being sent.",
			Buckets: deliveryLatencyBuckets,
		}),
		transitions: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "navi_occurrence_transitions_total",
			Help: "Occurrence status transitions, by edge and by the surface that made it.",
		}, []string{"from", "to", "source"}),
		claimsReleased: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "navi_scheduler_claims_released_total",
			Help: "Claimed occurrences returned to pending because the send did not happen.",
		}),
		copywriterFallbacks: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "navi_copywriter_fallback_total",
			Help: "Notifications sent as the plain item title because no generated text existed.",
		}),
		checkIns: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "navi_reconcile_checkins_total",
			Help: "Daily check-in messages sent (K4). One per pass, never one per item.",
		}),
		reconciledOccurrences: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "navi_reconcile_occurrences_total",
			Help: "Occurrences a check-in asked about, across all passes.",
		}),
		checkInFallbacks: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "navi_reconcile_fallback_total",
			Help: "Check-ins sent as the plain template because no composed text was produced.",
		}),
		briefingsSent: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "navi_briefing_sent_total",
			Help: "Morning briefings sent (O6). One per day.",
		}),
		briefingFallbacks: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "navi_briefing_fallback_total",
			Help: "Briefings sent as the plain template because no composed text was produced.",
		}),
		briefingsUnanswered: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "navi_briefing_unanswered_total",
			Help: "Morning briefings that got no reply before their grace window closed (O8).",
		}),
		inboundAccepted: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "navi_inbound_messages_accepted_total",
			Help: "Inbound messages recorded to conversations, by transport.",
		}, []string{"transport"}),
		inboundDropped: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "navi_inbound_messages_dropped_total",
			Help: "Inbound messages rejected before being recorded, by reason.",
		}, []string{"reason"}),
		llmCalls: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "navi_llm_calls_total",
			Help: "Model client calls, by task, tier and outcome.",
		}, []string{"task", "tier", "outcome"}),
		llmLatency: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "navi_llm_latency_seconds",
			Help:    "Seconds from a model call starting to Complete returning, by task and tier.",
			Buckets: llmLatencyBuckets,
		}, []string{"task", "tier"}),
	}
	reg.MustRegister(
		m.loopTickInterval,
		m.loopErrors,
		m.deliveryLatency,
		m.transitions,
		m.claimsReleased,
		m.copywriterFallbacks,
		m.checkIns,
		m.reconciledOccurrences,
		m.checkInFallbacks,
		m.briefingsSent,
		m.briefingFallbacks,
		m.briefingsUnanswered,
		m.inboundAccepted,
		m.inboundDropped,
		m.llmCalls,
		m.llmLatency,
	)
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

// ObserveDeliveryLatency records how late a notification was, measured from the
// occurrence's scheduled time to the send returning.
//
// Negative observations are clamped to zero. An NTP step backwards produces one,
// Prometheus accepts it without complaint, and it corrupts the _sum for the life
// of the process — which is a quietly wrong dashboard rather than a visible
// failure.
func (m *Metrics) ObserveDeliveryLatency(seconds float64) {
	if seconds < 0 {
		seconds = 0
	}
	m.deliveryLatency.Observe(seconds)
}

// IncTransition counts one occurrence status change.
//
// Only real transitions belong here — edges that appear in the table in
// internal/domain/status.go. The scheduler's release of an unsent claim moves a
// row from notified back to pending, which is not one of them; it is counted by
// IncClaimReleased instead. Putting it in this series would mean any dashboard
// summing by "to" reports more rows reaching notified than ever did, and anyone
// diffing the series against the state machine finds a contradiction.
func (m *Metrics) IncTransition(from, to, source string) {
	m.transitions.WithLabelValues(from, to, source).Inc()
}

// RegisterTransition creates a child series so an edge that has not happened yet
// exports a zero, on the same argument as RegisterLoop.
func (m *Metrics) RegisterTransition(from, to, source string) {
	m.transitions.WithLabelValues(from, to, source)
}

// IncClaimReleased counts one claim rolled back, whatever the cause: a failed
// send, a cancelled tick, or a panic in the send loop.
func (m *Metrics) IncClaimReleased() { m.claimsReleased.Inc() }

// IncCopywriterFallback counts one notification that went out as the plain item
// title.
//
// Counted here rather than in the copywriter because the copywriter cannot
// observe it: it knows what it failed to generate, not what was actually sent,
// and the number worth having is how often a reminder reached the user plain.
func (m *Metrics) IncCopywriterFallback() { m.copywriterFallbacks.Inc() }

// IncCheckIn counts one daily check-in sent. Counted after the send returns,
// not when the pass decides to run: a check-in that never reached the user
// asked nobody anything, and D-008 turns on that distinction.
//
// Both of these are plain counters and neither carries a status label. The
// grace pass's missed assignments go to navi_occurrence_transitions_total like
// every other edge, which is where a status label belongs.
func (m *Metrics) IncCheckIn() { m.checkIns.Inc() }

// IncCheckInFallback counts one check-in sent as the plain template rather than
// as composed text.
//
// It exists for the same reason IncCopywriterFallback does, and the parallel is
// exact: what is worth knowing is how often the user got the boring version,
// which is a fact about what was sent rather than about what failed to
// generate. llm_calls cannot answer it, because the commonest fallback writes
// no llm_calls row at all — a deployment with no chat transport has no model
// client, never calls one, and templates every night.
func (m *Metrics) IncCheckInFallback() { m.checkInFallbacks.Inc() }

// IncBriefingSent counts one morning briefing delivered. Counted after the send
// returns, like IncCheckIn, and for the same reason: a briefing that never
// reached the user asked nobody anything, and O8's grace window turns on that.
func (m *Metrics) IncBriefingSent() { m.briefingsSent.Inc() }

// IncBriefingFallback counts one briefing sent as the plain template rather
// than as composed text — the exact parallel of IncCheckInFallback, and the
// number worth having for the same reason: how often the user got the boring
// version, which llm_calls cannot answer because the commonest fallback (no
// chat transport, so no model client) writes no llm_calls row.
func (m *Metrics) IncBriefingFallback() { m.briefingFallbacks.Inc() }

// IncBriefingUnanswered counts one briefing whose grace window closed with no
// reply (O8). Unlike the reconciler's grace pass there is no occurrence
// transition to fold this into, so it is its own plain counter — the only
// signal "the briefing went unanswered" produces, read later by the day's tone
// strategy.
func (m *Metrics) IncBriefingUnanswered() { m.briefingsUnanswered.Inc() }

// AddReconciledOccurrences counts how many occurrences a check-in asked about.
// Read against navi_reconcile_checkins_total it is the average size of an
// evening's outstanding list, which is the number that says whether
// consolidation (K4, D-009) is earning its keep.
func (m *Metrics) AddReconciledOccurrences(n int) {
	m.reconciledOccurrences.Add(float64(n))
}

// IncInboundAccepted counts one inbound message actually recorded to
// conversations — not one webhook call, so a retried delivery that hit the
// dedup path does not inflate this.
func (m *Metrics) IncInboundAccepted(transport string) {
	m.inboundAccepted.WithLabelValues(transport).Inc()
}

// IncInboundDropped counts one inbound message rejected before it was
// recorded — today only "allowlist" (D8), labeled for the reasons a future
// adapter or check might add.
func (m *Metrics) IncInboundDropped(reason string) {
	m.inboundDropped.WithLabelValues(reason).Inc()
}

// RegisterInboundAccepted and RegisterInboundDropped create their child
// series up front, on the same RegisterLoop/RegisterTransition argument: a
// combination that has not happened yet should export a zero, not be absent.
func (m *Metrics) RegisterInboundAccepted(transport string) {
	m.inboundAccepted.WithLabelValues(transport)
}

func (m *Metrics) RegisterInboundDropped(reason string) {
	m.inboundDropped.WithLabelValues(reason)
}

// IncLLMCall counts one model-client attempt, tagged with its outcome —
// "success" or an ErrorKind's string, e.g. "timeout".
func (m *Metrics) IncLLMCall(task string, tier int, outcome string) {
	m.llmCalls.WithLabelValues(task, strconv.Itoa(tier), outcome).Inc()
}

// ObserveLLMLatency records how long one Complete call took, start to
// return, regardless of whether it succeeded.
func (m *Metrics) ObserveLLMLatency(task string, tier int, seconds float64) {
	m.llmLatency.WithLabelValues(task, strconv.Itoa(tier)).Observe(seconds)
}

// RegisterLLMCall creates the child series for a (task, tier) pair so it
// exports a zero before it has ever been called, on the same
// RegisterTransition argument.
func (m *Metrics) RegisterLLMCall(task string, tier int) {
	m.llmLatency.WithLabelValues(task, strconv.Itoa(tier))
}

// RegisterPendingOverdue publishes navi_pending_overdue, backed by a function
// this package calls at scrape time.
//
// A callback rather than a setter, because this package is a leaf and has to
// stay one: the supervisor writes to it and a handler reads it, so a database
// import here would put a dependency on the store underneath everything. The
// caller supplies a closure over the repository, and the count is a single
// partial-index lookup.
//
// f should return NaN when it cannot answer. Prometheus renders that as a gap
// in the series, which is what an unreachable database should look like — zero
// would look like a healthy scheduler.
func (m *Metrics) RegisterPendingOverdue(f func() float64) {
	m.registry.MustRegister(prometheus.NewGaugeFunc(prometheus.GaugeOpts{
		Name: "navi_pending_overdue",
		Help: "Pending occurrences past their start time that the scheduler has not claimed.",
	}, f))
}

// RegisterHorizonDays publishes navi_materializer_horizon_days, on the same
// callback pattern and for the same reason.
//
// It is the one number that says the materializer is still working. Every other
// symptom of a stalled expansion is invisible for weeks: occurrences already
// exist, reminders keep firing, and nothing goes wrong until the day the last
// materialized row fires and the calendar is empty behind it. This gauge starts
// falling the first night a run is missed.
//
// f should return NaN when nothing has ever been materialized, which is a
// database with no horizon rather than a horizon of zero.
func (m *Metrics) RegisterHorizonDays(f func() float64) {
	m.registry.MustRegister(prometheus.NewGaugeFunc(prometheus.GaugeOpts{
		Name: "navi_materializer_horizon_days",
		Help: "Whole days of occurrences materialized ahead of now.",
	}, f))
}

// Handler serves the exposition format for this registry.
func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.registry, promhttp.HandlerOpts{
		ErrorHandling: promhttp.ContinueOnError,
	})
}

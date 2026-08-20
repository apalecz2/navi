// Package reconciler runs the daily check-in and, after a grace window, assigns
// missed.
//
// The ordering is the point: missed means "asked and got nothing", not
// "midnight passed" (D-008, K6). Nothing in this package may mark an occurrence
// missed on a clock alone, which is also why a transport outage produces no
// false misses — an unsent check-in never asked.
//
// That rule is what decides the one ordering choice in Reconcile, and it points
// the opposite way from the scheduler's. The scheduler commits its claim before
// it sends, accepting at-most-once across a crash, because holding the single
// writer connection open across a network call blocks every other write and
// because reconciliation is the backstop for whatever gets dropped. Here the
// send happens first and reconciled_at is written after it returns, because
// reconciled_at is the instant a future grace window measures from: a crash
// between the two costs one duplicate question, where the other order would
// cost a miss nobody was ever asked about. Do not "fix" this into consistency
// with the scheduler — the two are asymmetric on purpose.
//
// The check-in is composed by a model when one is configured and rendered from
// a template when it is not, or when the call fails (compose.go, template.go).
// The template is not a placeholder: 06-agent-spec gives this component the
// failure mode "fall back to a templated list", and it is the live path for any
// deployment without a chat transport - which is the right answer there, since
// a check-in nobody can reply to does not need to be prettier.
//
// The loop is two passes and they are separate on purpose. Reconcile asks;
// ExpireGrace (grace.go) concludes. Nothing here reads a reply, and that is not
// a gap: a reply arrives as an ordinary inbound message and is resolved by the
// agent's bulk_resolve, which is D-009's "reconciliation and batch completion
// are the same code path" taken literally. What this package contributes to
// that is occurrences.reconciled_at and the conversations row's context_ref;
// internal/conversation renders both into the turn that answers them.
package reconciler

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/aidenpaleczny/navi/internal/domain"
	"github.com/aidenpaleczny/navi/internal/schedule"
	"github.com/aidenpaleczny/navi/internal/store"
	"github.com/aidenpaleczny/navi/internal/supervisor"
	"github.com/aidenpaleczny/navi/internal/transport"
)

// Name is the loop's identity in logs, /healthz, and the metric labels.
const Name = "reconciler"

// Interval is how often the loop wakes. It acts at configured local times, so
// the body is a gate that returns immediately on most ticks.
const Interval = 60 * time.Second

// sendTimeout bounds the one network call a pass makes, matching the
// scheduler's. A check-in that cannot go out inside it is retried on the next
// tick with nothing written.
const sendTimeout = 10 * time.Second

// slotSeparator joins a local date and a local HH:MM into the value stored in
// kv.last_reconcile_date. T rather than a space so the result sorts and reads
// like the ISO instants beside it, and so a stored value is unambiguous when
// someone reads the kv table by hand.
const slotSeparator = "T"

// Store is the narrow view this loop needs. Declared here rather than taking
// *store.Store, the same shape scheduler.Store and sweeper.Store use.
type Store interface {
	GlobalPauseUntil(ctx context.Context) (time.Time, bool, error)
	CurrentTZ(ctx context.Context) (string, bool, error)
	ListActiveItems(ctx context.Context) ([]domain.Item, error)
	LastReconcileSlot(ctx context.Context) (string, bool, error)
	ListUnreconciled(ctx context.Context, from, to time.Time, globalSlot, slot string) ([]store.Unreconciled, error)
	RecordCheckIn(ctx context.Context, ids []string, slot string, conv *domain.NewConversation, now time.Time) error

	// The grace half. ListAwaitingReconciliation is the read the agent's
	// context injection shares; BulkResolve is the same atomic write every
	// other resolution surface reaches, which is what keeps the fourth surface
	// from being a fourth set of transition rules (D-014).
	ListAwaitingReconciliation(ctx context.Context, loc *time.Location) ([]store.Awaiting, error)
	BulkResolve(ctx context.Context, rows []store.BulkResolution, source domain.ResolutionSource, now time.Time) ([]store.Resolution, error)
}

// Notifier is the outbound half of a transport and nothing else, on the same
// argument scheduler.Notifier makes: omitting Receive keeps this loop unable to
// read messages, which is true of it and will stay true when the reply path
// lands in a separate component.
type Notifier interface {
	Name() string
	Capabilities() transport.Capabilities
	Send(ctx context.Context, msg transport.Outbound) (externalID string, err error)
}

// Metrics is the check-in's slice of the registry.
//
// IncTransition is here because the grace pass writes a status, and a status
// change belongs in navi_occurrence_transitions_total wherever it happens -
// the store does not count for anyone, so every surface counts at its own edge.
type Metrics interface {
	IncCheckIn()
	IncCheckInFallback()
	AddReconciledOccurrences(n int)
	IncTransition(from, to, source string)
}

// Reconciler sends the daily check-in and, from next session, applies missed
// after grace.
type Reconciler struct {
	log      *slog.Logger
	store    Store
	notifier Notifier
	metrics  Metrics

	// composer is nil when no model client exists, which is every deployment
	// without a chat transport. That is a supported configuration and not a
	// degraded one — see compose.go.
	composer Composer

	// globalAt is cfg.Schedule.ReconcileAt, a zero-padded local HH:MM.
	// items.reconcile_at overrides it per item (K8).
	globalAt string

	// defaultTZ is the bottom rung of schedule.Zones — the deployment default
	// standing in when kv.current_tz has never been set.
	defaultTZ *time.Location
}

// New returns a reconciler. globalAt is the configured check-in time as a local
// HH:MM; config.Load has already rejected anything that is not one. composer
// may be nil, in which case every check-in is the template.
func New(log *slog.Logger, st Store, n Notifier, m Metrics, c Composer, globalAt string, defaultTZ *time.Location) *Reconciler {
	return &Reconciler{
		log:       log,
		store:     st,
		notifier:  n,
		metrics:   m,
		composer:  c,
		globalAt:  globalAt,
		defaultTZ: defaultTZ,
	}
}

// Loop describes this loop to the supervisor.
func (r *Reconciler) Loop() supervisor.Loop {
	return supervisor.Loop{Name: Name, Interval: Interval, Tick: r.Tick}
}

// Result is what one pass did, for the log line and for naviseed to assert
// against.
type Result struct {
	// Slot is the pass that ran, as a local HH:MM. Empty means no slot was due,
	// which is what almost every tick returns.
	Slot string

	// Occurrences is how many rows the check-in asked about.
	Occurrences int

	// Sent reports whether a message actually went out. False with a non-empty
	// Slot is Q-6's answer to a day with nothing outstanding: the pass ran, it
	// found nothing, and silence is better than a daily "all done".
	Sent bool

	// Paused reports that a global pause suppressed the pass entirely (I6).
	Paused bool
}

// LogValue renders the result as one group rather than four top-level keys.
func (r Result) LogValue() slog.Value {
	return slog.GroupValue(
		slog.String("slot", r.Slot),
		slog.Int("occurrences", r.Occurrences),
		slog.Bool("sent", r.Sent),
	)
}

// Tick is the loop body: ask, then expire, then a line if either did anything.
//
// The two run in that order and both run unconditionally. Order, because a row
// asked about at 21:00 with a five-minute grace should not wait a full tick to
// expire. Unconditionally, because a failed send must not block expiry: the
// send that failed wrote no reconciled_at, so it added nothing to the set the
// grace pass is working on, and holding yesterday's answered-nothing rows
// hostage to today's transport outage would be a second failure caused by the
// first.
func (r *Reconciler) Tick(ctx context.Context) error {
	now := time.Now()

	res, askErr := r.Reconcile(ctx, now)
	if res.Sent {
		r.log.Info("check-in sent", "result", res)
	}

	exp, graceErr := r.ExpireGrace(ctx, now)
	if exp.Missed > 0 {
		r.log.Info("grace expired", "result", exp)
	}

	return errors.Join(askErr, graceErr)
}

// Reconcile runs one pass. It is the synchronous entry point, and it takes its
// own now for the reason schedule.ResolveDelta does: an evening pass is
// otherwise only checkable in the evening, and cmd/naviseed drives all of this
// at whatever hour it happens to run.
//
// The order of the steps is load-bearing and reads top to bottom: global pause
// first because it must suppress the writes and not merely the message, the
// zone second because it decides both which slot has arrived and where the day
// begins, and the send before the record for the reason in the package doc.
func (r *Reconciler) Reconcile(ctx context.Context, now time.Time) (Result, error) {
	// Vacation mode, checked before anything is read or written. A global pause
	// suppresses the check-in entirely (I6): no message, no reconciled_at, and
	// no latch advance, so the ordinary cadence resumes on its own when the
	// window ends rather than starting from a slot that was never really run.
	pausedUntil, paused, err := r.store.GlobalPauseUntil(ctx)
	if err != nil {
		return Result{}, err
	}
	if paused && pausedUntil.After(now) {
		return Result{Paused: true}, nil
	}

	// The device zone, read once before any write, the same discipline
	// schedule.Zones' doc comment describes. Local rather than For: the
	// check-in is one message to one person about their day, so it resolves
	// against where the user is and not against any item's zone.
	zones, err := schedule.LoadZones(ctx, r.store, r.defaultTZ)
	if err != nil {
		return Result{}, fmt.Errorf("reconciler: resolve zones: %w", err)
	}
	local := now.In(zones.Local())

	slot, ok, err := r.dueSlot(ctx, local)
	if err != nil || !ok {
		return Result{}, err
	}

	date := local.Format(domain.DateLayout)
	midnight := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, local.Location())

	outstanding, err := r.store.ListUnreconciled(ctx, midnight, now, r.globalAt, slot)
	if err != nil {
		return Result{}, err
	}

	res := Result{Slot: slot, Occurrences: len(outstanding)}

	// Nothing outstanding: say nothing, but advance the latch. Q-6 prefers
	// silence to a daily "all done", and without the latch this pass would be
	// re-evaluated every sixty seconds until midnight.
	if len(outstanding) == 0 {
		return res, r.store.RecordCheckIn(ctx, nil, date+slotSeparator+slot, nil, now)
	}

	text := r.composeCheckIn(ctx, outstanding)

	sendCtx, cancel := context.WithTimeout(ctx, sendTimeout)
	defer cancel()

	// No Actions and no SubjectID. A check-in is about a set of occurrences
	// rather than one, so there is nothing for SubjectID to name, and the answer
	// to "which of those got done?" is prose routed through bulk_resolve rather
	// than a tap (D-009). An adapter that renders buttons therefore renders none
	// here, without branching on why the message was sent.
	if _, err := r.notifier.Send(sendCtx, transport.Outbound{
		Body: transport.Truncate(text, r.notifier.Capabilities().MaxBodyLength),
	}); err != nil {
		// Nothing is written. The next tick re-asks, which is the failure-mode
		// table's "reconciliation queues its message and sends on recovery" —
		// and, more importantly, an unsent check-in has asked nobody anything,
		// so no row may carry a reconciled_at claiming otherwise.
		return res, fmt.Errorf("reconciler: send check-in for %s: %w", date, err)
	}
	res.Sent = true

	ids := make([]string, len(outstanding))
	for i, o := range outstanding {
		ids[i] = o.ID
	}

	// context_ref is what lets next session recognise a reply as an answer
	// rather than a new request (A9). The row is otherwise shaped exactly like
	// the ones Ladder.persistAssistantProse writes, which is what makes it
	// replayable as cross-turn history; transport and external_id are left
	// unset deliberately, since idx_conv_dedup is unique over that pair and a
	// Telegram message id is drawn from the same sequence in both directions.
	contextRef := "reconcile:" + date
	conv := domain.NewConversation{
		Role:       domain.RoleAssistant,
		Content:    text,
		ContextRef: &contextRef,
	}

	if err := r.store.RecordCheckIn(ctx, ids, date+slotSeparator+slot, &conv, now); err != nil {
		return res, err
	}

	r.metrics.IncCheckIn()
	r.metrics.AddReconciledOccurrences(len(outstanding))
	return res, nil
}

// dueSlot answers which reconciliation pass, if any, is due right now.
//
// Candidates are the configured global time plus every distinct per-item
// override (K8). A candidate is due when it has arrived in local time and has
// not already run today, which is the comparison kv.last_reconcile_date's slot
// format exists to make a string comparison.
//
// It returns the *greatest* due candidate rather than looping over all of them.
// That is what keeps K4 true across an outage: a process down from 13:00 to
// 22:00 comes back with both the 14:00 and the 21:00 passes eligible, and
// running the later one covers the earlier, because ListUnreconciled's own
// predicate is "every item due to be asked at or before this pass". One
// catch-up message, not two.
func (r *Reconciler) dueSlot(ctx context.Context, local time.Time) (string, bool, error) {
	last, _, err := r.store.LastReconcileSlot(ctx)
	if err != nil {
		return "", false, err
	}

	items, err := r.store.ListActiveItems(ctx)
	if err != nil {
		return "", false, err
	}

	candidates := map[string]struct{}{r.globalAt: {}}
	for _, item := range items {
		if item.ReconcileAt != nil && *item.ReconcileAt != "" {
			candidates[*item.ReconcileAt] = struct{}{}
		}
	}

	date := local.Format(domain.DateLayout)
	nowHM := local.Format(LocalTimeLayout)

	var best string
	for slot := range candidates {
		if slot > nowHM {
			continue // has not arrived yet today
		}
		if date+slotSeparator+slot <= last {
			continue // already run, including on an earlier day
		}
		if slot > best {
			best = slot
		}
	}
	if best == "" {
		return "", false, nil
	}
	return best, true, nil
}

// LocalTimeLayout is config.LocalTimeLayout's value, repeated rather than
// imported: internal/config is the process's environment reader and nothing
// outside main depends on it, which is a boundary worth more than one shared
// constant. The two are asserted equal by cmd/naviseed.
const LocalTimeLayout = "15:04"

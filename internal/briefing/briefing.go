// Package briefing runs the morning briefing: a daily proactive message that
// states what today looks like and, unlike every other proactive message in
// this system, expects a reply (O6-O9, docs/11-goals-spec.md#the-morning-briefing).
//
// It is the first new loop since session 1. It appears on /healthz, on the
// loops dashboard, and in the shutdown path like the other six.
//
// Three phases on a 60-second tick, each gated by a kv slot so a restart at any
// point loses nothing and repeats nothing:
//
//   - compose, ~30 minutes ahead of BRIEFING_AT: build the same context blob
//     the agent's system prompt uses, ask the model for prose, fall back to a
//     plain template, and store the result in kv.briefing_pending. Never at
//     send time — a briefing generated when it fires is a briefing that does
//     not arrive when the provider is down (invariant 1, same rule as the fire
//     path and the reconciler's composer).
//   - send, at BRIEFING_AT: read the stored text (or render the template with
//     no model call if none was staged), send it, and in one transaction write
//     the outbound conversation row with context_ref = briefing:{date}, the
//     last_briefing_date latch, and the awaiting-response marker.
//   - evaluate, every tick: the moment any inbound message lands after the
//     briefing went out, clear the marker; if the grace window closes with
//     none, record that it went unanswered. There is no occurrence here, so
//     "unanswered" writes no missed and no transition — it produces only the
//     record that it happened, read later by the day's tone strategy (O8, O9).
//
// The composer is nil when no model client exists, which is every deployment
// without a chat transport. That is a supported configuration, not a degraded
// one, and composeBriefing templates in that case without treating it as a
// failure (compose.go) — the same shape reconciler.Composer has.
package briefing

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
const Name = "briefing"

// Interval is how often the loop wakes. It acts at one local time a day, so the
// body is a gate that returns immediately on almost every tick.
const Interval = 60 * time.Second

// composeLead is how far ahead of BRIEFING_AT the briefing is composed. Long
// enough that a slow tier walk and its fallback both finish before the send,
// short enough that "what today looks like" has not gone stale.
const composeLead = 30 * time.Minute

// sendTimeout bounds the one network call a send makes, matching the
// scheduler's and the reconciler's. A briefing that cannot go out inside it is
// retried on the next tick with nothing written.
const sendTimeout = 10 * time.Second

// LocalTimeLayout is config.LocalTimeLayout's value, repeated rather than
// imported for the same reason reconciler.LocalTimeLayout repeats it:
// internal/config is the process's environment reader and nothing outside main
// depends on it. cmd/naviseed asserts the two are equal.
const LocalTimeLayout = "15:04"

// Store is the narrow view this loop needs. Declared here rather than taking
// *store.Store, the same shape scheduler.Store, reconciler.Store and
// sweeper.Store use.
type Store interface {
	GlobalPauseUntil(ctx context.Context) (time.Time, bool, error)
	CurrentTZ(ctx context.Context) (string, bool, error)

	// The context blob: the same three reads the agent's system prompt
	// assembles, so a goal named in the morning message and one the agent
	// discusses mid-conversation show identical numbers (06-agent-spec).
	ListActiveItems(ctx context.Context) ([]domain.Item, error)
	TodaysOccurrences(ctx context.Context, loc *time.Location) ([]store.TodayOccurrence, error)
	ListGoalProgress(ctx context.Context, filter store.GoalFilter, loc *time.Location) ([]store.GoalProgress, error)

	// The three kv slots and the reply check.
	BriefingPending(ctx context.Context) (date, text string, ok bool, err error)
	SetBriefingPending(ctx context.Context, date, text string) error
	LastBriefingDate(ctx context.Context) (string, bool, error)
	BriefingAwaiting(ctx context.Context) (date string, sentAt time.Time, ok bool, err error)
	RecordBriefing(ctx context.Context, date, text, contextRef string, sentAt time.Time) error
	ClearBriefingAwaiting(ctx context.Context) error
	HasInboundSince(ctx context.Context, t time.Time) (bool, error)
}

// Notifier is the outbound half of a transport and nothing else, on the same
// argument scheduler.Notifier and reconciler.Notifier make.
type Notifier interface {
	Name() string
	Capabilities() transport.Capabilities
	Send(ctx context.Context, msg transport.Outbound) (externalID string, err error)
}

// Metrics is the briefing's slice of the registry: sends and template
// fallbacks, mirroring the reconciler's two counters, plus the one signal the
// evaluate pass produces.
type Metrics interface {
	IncBriefingSent()
	IncBriefingFallback()
	IncBriefingUnanswered()
}

// Briefing composes, sends, and tracks the reply to the morning briefing.
type Briefing struct {
	log      *slog.Logger
	store    Store
	notifier Notifier
	metrics  Metrics

	// composer is nil when no model client exists. composeBriefing treats that
	// as an ordinary template pass rather than a failure — see compose.go.
	composer Composer

	// at is cfg.Schedule.BriefingAt, a zero-padded local HH:MM. config.Load has
	// already rejected anything that is not one.
	at string

	// defaultTZ is the bottom rung of schedule.Zones — the deployment default
	// standing in when kv.current_tz has never been set.
	defaultTZ *time.Location
}

// New returns a briefing loop. at is the configured send time as a local HH:MM.
// composer may be nil, in which case every briefing is the template.
func New(log *slog.Logger, st Store, n Notifier, m Metrics, c Composer, at string, defaultTZ *time.Location) *Briefing {
	return &Briefing{
		log:       log,
		store:     st,
		notifier:  n,
		metrics:   m,
		composer:  c,
		at:        at,
		defaultTZ: defaultTZ,
	}
}

// Loop describes this loop to the supervisor.
func (b *Briefing) Loop() supervisor.Loop {
	return supervisor.Loop{Name: Name, Interval: Interval, Tick: b.Tick}
}

// Phase names what one Run pass did, for the log line and for naviseed.
type Phase string

const (
	PhaseIdle    Phase = ""
	PhaseCompose Phase = "compose"
	PhaseSend    Phase = "send"
)

// Result is what one Run pass did.
type Result struct {
	Phase Phase

	// Sent reports that a briefing actually went out this pass.
	Sent bool

	// Fallback reports that what was composed or sent was the plain template
	// rather than model prose.
	Fallback bool

	// Paused reports that a global pause suppressed the pass entirely (I6).
	Paused bool
}

// LogValue renders the result as one group.
func (r Result) LogValue() slog.Value {
	return slog.GroupValue(
		slog.String("phase", string(r.Phase)),
		slog.Bool("sent", r.Sent),
		slog.Bool("fallback", r.Fallback),
	)
}

// Tick is the loop body: compose-or-send, then evaluate, then a line if either
// did anything. Both run unconditionally, the same as the reconciler's two
// passes: a failed send must not block the evaluation of a briefing sent
// yesterday.
func (b *Briefing) Tick(ctx context.Context) error {
	now := time.Now()

	res, runErr := b.Run(ctx, now)
	if res.Sent {
		b.log.Info("briefing sent", "result", res)
	}

	ev, evalErr := b.EvaluateResponse(ctx, now)
	if ev.Answered || ev.Unanswered {
		b.log.Info("briefing response evaluated", "result", ev)
	}

	return errors.Join(runErr, evalErr)
}

// Run is the compose-and-send half. It takes its own now so an early-morning
// pass is checkable at any hour, the way reconciler.Reconcile and
// schedule.ResolveDelta take theirs.
//
// The order of the steps is load-bearing: global pause first because it must
// suppress the writes and not merely the message, the zone second because it
// decides which phase the wall clock is in and where the day begins.
func (b *Briefing) Run(ctx context.Context, now time.Time) (Result, error) {
	pausedUntil, paused, err := b.store.GlobalPauseUntil(ctx)
	if err != nil {
		return Result{}, err
	}
	if paused && pausedUntil.After(now) {
		return Result{Paused: true}, nil
	}

	zones, err := schedule.LoadZones(ctx, b.store, b.defaultTZ)
	if err != nil {
		return Result{}, fmt.Errorf("briefing: resolve zones: %w", err)
	}
	local := now.In(zones.Local())
	date := local.Format(domain.DateLayout)

	// Already sent today: nothing for Run to do until tomorrow. The evaluate
	// pass, which Tick calls separately, is what still has work.
	last, _, err := b.store.LastBriefingDate(ctx)
	if err != nil {
		return Result{}, err
	}
	if last == date {
		return Result{}, nil
	}

	sendAt, err := b.slotToday(local)
	if err != nil {
		return Result{}, err
	}
	composeAt := sendAt.Add(-composeLead)

	switch {
	case local.Before(composeAt):
		// Too early for anything.
		return Result{}, nil
	case local.Before(sendAt):
		return b.compose(ctx, date, local)
	default:
		return b.send(ctx, date, local, now)
	}
}

// compose builds the context blob, asks for prose (or templates), and stages
// the result. It writes nothing else — no send, no latch.
func (b *Briefing) compose(ctx context.Context, date string, local time.Time) (Result, error) {
	pendingDate, _, ok, err := b.store.BriefingPending(ctx)
	if err != nil {
		return Result{}, err
	}
	if ok && pendingDate == date {
		return Result{Phase: PhaseCompose}, nil // already staged for today
	}

	blob, err := b.buildContext(ctx, local)
	if err != nil {
		return Result{}, err
	}

	text, fellBack := b.composeBriefing(ctx, blob, true)
	if err := b.store.SetBriefingPending(ctx, date, text); err != nil {
		return Result{}, err
	}
	// Counted here, at compose time, because that is where the choice between
	// prose and template is made - and a staged template is what the user will
	// receive at BRIEFING_AT. The guard above means a re-compose on the same day
	// returns before reaching this line, so one briefing is counted at most once.
	if fellBack {
		b.metrics.IncBriefingFallback()
	}
	return Result{Phase: PhaseCompose, Fallback: fellBack}, nil
}

// send reads the staged text (or renders the template with no model call when
// none was staged), delivers it, and records the send in one transaction.
func (b *Briefing) send(ctx context.Context, date string, local, now time.Time) (Result, error) {
	pendingDate, pendingText, ok, err := b.store.BriefingPending(ctx)
	if err != nil {
		return Result{}, err
	}

	text := pendingText
	fellBack := false
	if !ok || pendingDate != date {
		// The compose window was missed - a restart that spanned it, or a first
		// boot after BRIEFING_AT. Render the template synchronously: invariant 1
		// forbids a model call this close to (or past) the send.
		blob, berr := b.buildContext(ctx, local)
		if berr != nil {
			return Result{}, berr
		}
		text = composeTemplate(blob)
		fellBack = true
	}

	sendCtx, cancel := context.WithTimeout(ctx, sendTimeout)
	defer cancel()

	// No Actions and no SubjectID: a briefing is a message to reply to in prose,
	// not a set of buttons.
	if _, err := b.notifier.Send(sendCtx, transport.Outbound{
		Body: transport.Truncate(text, b.notifier.Capabilities().MaxBodyLength),
	}); err != nil {
		// Nothing written. The next tick re-sends, and no marker claims a
		// briefing went out that did not.
		return Result{Phase: PhaseSend}, fmt.Errorf("briefing: send for %s: %w", date, err)
	}

	if err := b.store.RecordBriefing(ctx, date, text, "briefing:"+date, now); err != nil {
		return Result{Phase: PhaseSend, Sent: true}, err
	}

	b.metrics.IncBriefingSent()
	if fellBack {
		b.metrics.IncBriefingFallback()
	}
	return Result{Phase: PhaseSend, Sent: true, Fallback: fellBack}, nil
}

// slotToday resolves the configured HH:MM against local's calendar day.
func (b *Briefing) slotToday(local time.Time) (time.Time, error) {
	hm, err := time.Parse(LocalTimeLayout, b.at)
	if err != nil {
		// config.Load validated this; a failure here means the value was
		// mutated after boot, which cannot happen.
		return time.Time{}, fmt.Errorf("briefing: parse send time %q: %w", b.at, err)
	}
	return time.Date(local.Year(), local.Month(), local.Day(),
		hm.Hour(), hm.Minute(), 0, 0, local.Location()), nil
}

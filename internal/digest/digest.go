// Package digest sends the weekly digest: one message on Sunday evening with the
// week's numbers and one observation about them (docs/06-agent-spec.md, Proactive
// behaviour; Q-9).
//
// It is the eighth loop when it is enabled, and no loop at all when it is not:
// config.Schedule.DigestAt is empty unless DIGEST_AT is set, main builds the loop
// and registers its metric series only in that case, and so an unset DIGEST_AT
// leaves no loop in /healthz, no series on /metrics and no kv key behind. Q-9 says
// this is the easiest piece of P5 to remove, and the way to make that true is for
// removing it to be one line of .env rather than a code change.
//
// Why its own loop and not a phase of the briefing: the shapes match (compose
// ahead, send, latch), and that is the whole of the resemblance. The briefing is
// daily, reads the day's occurrences and goals, expects a reply and evaluates
// whether it got one; the digest is weekly, reads internal/stats, and has nothing
// to evaluate. Folding it in would give the briefing package a stats dependency,
// a Sunday branch in every phase, and a metric set that exists whether or not the
// digest does - and an optional feature living inside a mandatory loop is
// precisely what cannot be switched off cleanly. The machinery it would have
// shared is thirty lines of gate.
//
// Three phases on a 60-second tick, on Sundays only, each gated by a kv slot so a
// restart at any point loses nothing and repeats nothing:
//
//   - compose, ~30 minutes ahead of DIGEST_AT: read the trailing week from
//     stats.Service, choose a form (Settle), word the figures, ask the model for
//     one observation, check every number in it, and stage the result.
//   - send, at DIGEST_AT: send the staged text, or render the template with no
//     model call if none was staged (a restart that spanned the window, a first
//     boot after DIGEST_AT), and in one transaction record the conversations row
//     with context_ref = digest:{date} and the latch.
//   - there is no third. Nothing evaluates a digest: it does not ask for an answer
//     it will later score. A reply is recognised by the clock (store.OpenDigest).
//
// The daily cap does not count the digest, and the quiet-hours window does not
// hold it. Both are the rule session 26 gave every scheduled message: the person
// chose the time, so it is not unprompted (A13 caps unprompted messages), and if
// the briefing and the check-in counted, a cap of three would leave one. The
// digest is the same kind of thing; it adds nothing to kv.proactive_count and
// reads none of it.
package digest

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/aidenpaleczny/navi/internal/domain"
	"github.com/aidenpaleczny/navi/internal/schedule"
	"github.com/aidenpaleczny/navi/internal/stats"
	"github.com/aidenpaleczny/navi/internal/supervisor"
	"github.com/aidenpaleczny/navi/internal/transport"
)

// Name is the loop's identity in logs, /healthz, and the metric labels.
const Name = "digest"

// Interval is how often the loop wakes. It acts once a week, so the body is a
// gate that returns immediately on almost every tick.
const Interval = 60 * time.Second

// composeLead is how far ahead of DIGEST_AT the digest is composed.
const composeLead = 30 * time.Minute

// sendTimeout bounds the one network call a send makes, matching the other loops.
const sendTimeout = 10 * time.Second

// LocalTimeLayout is config.LocalTimeLayout's value, repeated for the reason
// briefing.LocalTimeLayout repeats it.
const LocalTimeLayout = "15:04"

// Weekday is the day the digest goes out. A design fact, like the loop intervals.
const Weekday = time.Sunday

// Store is the narrow view this loop needs.
type Store interface {
	GlobalPauseUntil(ctx context.Context) (time.Time, bool, error)
	CurrentTZ(ctx context.Context) (string, bool, error)

	DigestPending(ctx context.Context) (date, text string, ok bool, err error)
	SetDigestPending(ctx context.Context, date, text string) error
	LastDigestDate(ctx context.Context) (string, bool, error)
	RecordDigest(ctx context.Context, date, text string, sentAt time.Time) error
	SkipDigest(ctx context.Context, date string, now time.Time) error
}

// Summarizer is the statistics the digest reads. *stats.Service satisfies it, the
// same value the page and get_stats hold: there is no other source of a figure in
// this package (V6).
type Summarizer interface {
	Summary(ctx context.Context, q stats.Query) (stats.Summary, error)
}

// Notifier is the outbound half of a transport and nothing else.
type Notifier interface {
	Name() string
	Capabilities() transport.Capabilities
	Send(ctx context.Context, msg transport.Outbound) (externalID string, err error)
}

// Metrics is the digest's slice of the registry.
type Metrics interface {
	IncDigestSent(form string)
	IncDigestFallback()
	IncDigestRejected()
}

// Digest composes and sends the weekly digest.
type Digest struct {
	log      *slog.Logger
	store    Store
	stats    Summarizer
	notifier Notifier
	metrics  Metrics
	composer Composer // nil when there is no model client

	at        string // cfg.Schedule.DigestAt, a zero-padded local HH:MM
	defaultTZ *time.Location

	weekday time.Weekday // Weekday unless WithWeekday says otherwise
}

// New returns the loop. at is the configured send time as a local HH:MM, which
// config.Load has already validated and which is never empty here: a deployment
// without DIGEST_AT does not call New. composer may be nil.
func New(log *slog.Logger, st Store, sum Summarizer, n Notifier, m Metrics, c Composer, at string, defaultTZ *time.Location) *Digest {
	return &Digest{log: log, store: st, stats: sum, notifier: n, metrics: m, composer: c, at: at,
		defaultTZ: defaultTZ, weekday: Weekday}
}

// WithWeekday replaces the day the digest goes out and returns the loop.
// Production never calls it; cmd/naviseed does, to run a pass at the real clock
// whatever day the check happens to be run on (the reply window is measured
// against the real clock, so that pass cannot be at a constructed Sunday).
func (d *Digest) WithWeekday(w time.Weekday) *Digest {
	d.weekday = w
	return d
}

// Loop describes this loop to the supervisor.
func (d *Digest) Loop() supervisor.Loop {
	return supervisor.Loop{Name: Name, Interval: Interval, Tick: d.Tick}
}

// Phase names what one Run pass did.
type Phase string

const (
	PhaseIdle    Phase = ""
	PhaseCompose Phase = "compose"
	PhaseSend    Phase = "send"
)

// Result is what one Run pass did.
type Result struct {
	Phase Phase

	// Form is the form the pass chose or sent.
	Form Form

	// Sent reports that a digest went out. Skipped reports a week with nothing in
	// it, latched without a send.
	Sent    bool
	Skipped bool

	// Fallback reports that a full digest went (or was staged) as the template;
	// Rejected that the number check refused a completion on the way.
	Fallback bool
	Rejected bool

	// Paused reports that a global pause suppressed the pass (I6).
	Paused bool
}

// LogValue renders the result as one group.
func (r Result) LogValue() slog.Value {
	return slog.GroupValue(
		slog.String("phase", string(r.Phase)),
		slog.String("form", string(r.Form)),
		slog.Bool("sent", r.Sent),
		slog.Bool("skipped", r.Skipped),
		slog.Bool("fallback", r.Fallback),
		slog.Bool("rejected", r.Rejected),
	)
}

// Tick is the loop body.
func (d *Digest) Tick(ctx context.Context) error {
	res, err := d.Run(ctx, time.Now())
	if res.Sent || res.Skipped {
		d.log.Info("digest pass", "result", res)
	}
	return err
}

// Run is one pass. It takes its own now so any hour of any day is checkable, the
// way briefing.Run and reconciler.Reconcile take theirs.
//
// Order matters: a global pause first, because it must suppress the writes as
// well as the message; then the zone, which decides the weekday and the date.
func (d *Digest) Run(ctx context.Context, now time.Time) (Result, error) {
	pausedUntil, paused, err := d.store.GlobalPauseUntil(ctx)
	if err != nil {
		return Result{}, err
	}
	if paused && pausedUntil.After(now) {
		return Result{Paused: true}, nil
	}

	zones, err := schedule.LoadZones(ctx, d.store, d.defaultTZ)
	if err != nil {
		return Result{}, fmt.Errorf("digest: resolve zones: %w", err)
	}
	local := now.In(zones.Local())
	if local.Weekday() != d.weekday {
		return Result{}, nil
	}
	date := local.Format(domain.DateLayout)

	last, _, err := d.store.LastDigestDate(ctx)
	if err != nil {
		return Result{}, err
	}
	if last == date {
		return Result{}, nil
	}

	hm, err := time.Parse(LocalTimeLayout, d.at)
	if err != nil {
		// config.Load validated this; a failure means the value was mutated.
		return Result{}, fmt.Errorf("digest: parse send time %q: %w", d.at, err)
	}
	sendAt := time.Date(local.Year(), local.Month(), local.Day(), hm.Hour(), hm.Minute(), 0, 0, local.Location())

	switch {
	case local.Before(sendAt.Add(-composeLead)):
		return Result{}, nil
	case local.Before(sendAt):
		return d.compose(ctx, date)
	default:
		return d.send(ctx, date, now)
	}
}

// week reads the trailing week and chooses a form. It is the only read, and it
// goes through the Service the page and get_stats call.
func (d *Digest) week(ctx context.Context) (Figures, error) {
	q, ve := stats.NewQuery(string(stats.RangeWeek), "", "")
	if ve != nil {
		return Figures{}, fmt.Errorf("digest: query: %w", ve)
	}
	sum, err := d.stats.Summary(ctx, q)
	if err != nil {
		return Figures{}, fmt.Errorf("digest: summary: %w", err)
	}
	return Settle(sum), nil
}

// A staged digest is "{form}\n{text}" in the pending slot, so the send does not
// have to guess what it is sending.
func stage(f Form, text string) string { return string(f) + "\n" + text }

func unstage(s string) (Form, string, bool) {
	form, text, ok := strings.Cut(s, "\n")
	return Form(form), text, ok && (Form(form) == FormFull || Form(form) == FormShort)
}

// compose stages the digest. It writes no send and no latch. A week with nothing
// in it stages nothing; the send phase concludes that for itself.
func (d *Digest) compose(ctx context.Context, date string) (Result, error) {
	if pendingDate, _, ok, err := d.store.DigestPending(ctx); err != nil {
		return Result{}, err
	} else if ok && pendingDate == date {
		return Result{Phase: PhaseCompose}, nil // already staged
	}

	f, err := d.week(ctx)
	if err != nil {
		return Result{}, err
	}
	if f.Form == FormNone {
		return Result{Phase: PhaseCompose, Form: FormNone}, nil
	}

	text, out := composeMessage(ctx, d.composer, f, true, d.log.Warn)
	if err := d.store.SetDigestPending(ctx, date, stage(f.Form, text)); err != nil {
		return Result{}, err
	}
	// Counted at compose, where the choice between prose and template is made,
	// and once: the staged guard above returns before reaching here on a re-run.
	if out.Rejected {
		d.metrics.IncDigestRejected()
	}
	if out.Fallback {
		d.metrics.IncDigestFallback()
	}
	return Result{Phase: PhaseCompose, Form: f.Form, Fallback: out.Fallback, Rejected: out.Rejected}, nil
}

// send delivers the staged text, or the template if none was staged, and latches.
// A week with nothing in it latches without sending.
func (d *Digest) send(ctx context.Context, date string, now time.Time) (Result, error) {
	var (
		form Form
		text string
		out  Outcome
	)

	pendingDate, pending, ok, err := d.store.DigestPending(ctx)
	if err != nil {
		return Result{}, err
	}
	if f, t, staged := unstage(pending); ok && pendingDate == date && staged {
		form, text = f, t
	} else {
		// The compose window was missed. Render the template synchronously:
		// invariant 1 forbids a model call this close to, or past, the send.
		fig, err := d.week(ctx)
		if err != nil {
			return Result{}, err
		}
		form = fig.Form
		if form != FormNone {
			text, out = composeMessage(ctx, nil, fig, false, d.log.Warn)
		}
	}

	if form == FormNone {
		if err := d.store.SkipDigest(ctx, date, now); err != nil {
			return Result{Phase: PhaseSend}, err
		}
		return Result{Phase: PhaseSend, Form: FormNone, Skipped: true}, nil
	}

	sendCtx, cancel := context.WithTimeout(ctx, sendTimeout)
	defer cancel()

	// No Actions and no SubjectID: a digest is prose to reply to, not buttons.
	if _, err := d.notifier.Send(sendCtx, transport.Outbound{
		Body: transport.Truncate(text, d.notifier.Capabilities().MaxBodyLength),
	}); err != nil {
		// Nothing written; the next tick sends again and no latch claims otherwise.
		return Result{Phase: PhaseSend, Form: form}, fmt.Errorf("digest: send for %s: %w", date, err)
	}

	if err := d.store.RecordDigest(ctx, date, text, now); err != nil {
		return Result{Phase: PhaseSend, Form: form, Sent: true},
			errors.Join(err, fmt.Errorf("digest: %s was sent but not recorded and will be sent again", date))
	}

	d.metrics.IncDigestSent(string(form))
	if out.Fallback {
		d.metrics.IncDigestFallback() // the unstaged-template path; staged ones were counted at compose
	}
	return Result{Phase: PhaseSend, Form: form, Sent: true, Fallback: out.Fallback}, nil
}

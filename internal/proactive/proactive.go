// Package proactive is the loop that speaks up unprompted: when an item has been
// missed five times running, when one occurrence has been snoozed three times, when
// an active item has gone dormant. Code decides when; the model decides how it is
// worded; and nothing here applies a change (D-012, D-013, A12, A13).
//
// It is the seventh loop. The sweeper was the obvious home and is the wrong one:
// it is hourly, it has no transport, and its imports are what make "the sweeper
// cannot reach the model" a compile-time fact. The shape this loop wants is the
// briefing's - a notifier, a nil-able composer, a latch in kv - so it is that.
//
// One pass, every five minutes:
//
//   - detect. Read each active reminder's signals and each recently snoozed chain
//     from internal/stats - the thresholds and the predicates are stats', so the
//     loop and the statistics page cannot disagree about what "consecutive" means -
//     and reconcile them against the latches. A condition seen for the first time
//     becomes a latch in state detected, which is what "the trigger fired" means
//     and is counted once. A latch whose condition has cleared is deleted; an
//     asked latch older than Rearm is deleted and, if the condition still holds,
//     re-detected. Detection runs at any hour.
//
//   - send, only inside the quiet-hours window. Take the detected latches in
//     priority order, stop at the daily cap, and send at most one per pass. A
//     trigger the cap or the hour holds back is not dropped: its latch stays
//     detected, and the first pass that is inside the window on a day with budget
//     sends it. Nothing is sent at 03:00 because nothing at 03:00 is allowed to
//     get past the window gate, however long ago the trigger was detected.
//
// What counts against the cap is only what this loop sends. The morning briefing
// and the end-of-day check-in are scheduled - the person set their time - and an
// agent reply to something the person said is not unprompted. If the first two
// counted, a cap of three would leave one.
//
// The composer is nil when no model client exists, an ordinary configuration, and
// composeProposal templates in that case without treating it as a failure.
package proactive

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/aidenpaleczny/navi/internal/domain"
	"github.com/aidenpaleczny/navi/internal/schedule"
	"github.com/aidenpaleczny/navi/internal/stats"
	"github.com/aidenpaleczny/navi/internal/store"
	"github.com/aidenpaleczny/navi/internal/supervisor"
	"github.com/aidenpaleczny/navi/internal/transport"
)

// Name is the loop's identity in logs, /healthz, and the metric labels.
const Name = "proactive"

// Interval is how often the loop wakes. Every pass reads each active reminder's
// history, so it is not a 60-second loop; five minutes is also the minimum spacing
// between two proposals, since a pass sends at most one.
const Interval = 5 * time.Minute

// DailyCap is A13: unprompted messages per local day. It is a design fact like the
// loop intervals, not a deployment knob.
const DailyCap = 3

// The quiet-hours window, local HH:MM in the person's zone. It sits between the
// default morning briefing (07:00) and the default end-of-day check-in (21:00) and
// is not derived from either: BRIEFING_AT and RECONCILE_AT are configurable, and a
// proposal landing in the same minute as one of them is a nuisance, not a
// correctness bug, so this does not read them. Moving them close to the window is
// the operator's trade.
const (
	WindowStart = "09:00"
	WindowEnd   = "20:00"
)

// Rearm is how long an asked episode stays latched with its condition still
// holding before it may be asked about again. A person who answered "leave it" and
// kept missing is not asked tomorrow; a month later it is a fresh question.
const Rearm = 30 * 24 * time.Hour

// sendTimeout bounds the one network call a send makes, matching the other loops.
const sendTimeout = 10 * time.Second

// LocalTimeLayout is the HH:MM layout WindowStart and WindowEnd are written in.
const LocalTimeLayout = "15:04"

// Store is the narrow view this loop needs, declared here like the other loops'.
type Store interface {
	GlobalPauseUntil(ctx context.Context) (time.Time, bool, error)
	CurrentTZ(ctx context.Context) (string, bool, error)
	ListActiveItems(ctx context.Context) ([]domain.Item, error)
	ListOccurrencesForItem(ctx context.Context, itemID string) ([]domain.Occurrence, error)

	Latches(ctx context.Context) (map[string]store.Latch, error)
	UpdateLatches(ctx context.Context, now time.Time, fn func(map[string]store.Latch) bool) error
	ProactiveCount(ctx context.Context, date string) (int, error)
	RecordProactiveSend(ctx context.Context, p domain.Proposal, text, latchKey, date, yesterday string, sentAt time.Time) error
}

// Signals is the statistics the triggers read. *stats.Service satisfies it; there
// is no other source of a count in this package.
type Signals interface {
	ItemSignals(ctx context.Context, itemID string) (stats.ItemSignals, error)
	SnoozedChains(ctx context.Context) ([]store.StatsChain, error)
}

// Notifier is the outbound half of a transport and nothing else.
type Notifier interface {
	Name() string
	Capabilities() transport.Capabilities
	Send(ctx context.Context, msg transport.Outbound) (externalID string, err error)
}

// Metrics is the loop's slice of the registry.
type Metrics interface {
	// IncTrigger counts one episode first seen, by kind. Once per episode: it
	// is counted when the latch is created, not when the message goes out, so a
	// trigger the cap holds back for a day is still one.
	IncTrigger(kind string)
	IncProposalSent(kind string)
	IncProposalFallback()
	IncProposalCapped()
}

// Proactive detects triggers and sends proposals.
type Proactive struct {
	log      *slog.Logger
	store    Store
	signals  Signals
	notifier Notifier
	metrics  Metrics
	composer Composer // nil when there is no model client

	defaultTZ *time.Location

	// from and to are the quiet-hours window, WindowStart and WindowEnd unless
	// WithWindow says otherwise.
	from, to string
}

// New returns the loop. composer may be nil, in which case every proposal is the
// template.
func New(log *slog.Logger, st Store, sig Signals, n Notifier, m Metrics, c Composer, defaultTZ *time.Location) *Proactive {
	return &Proactive{log: log, store: st, signals: sig, notifier: n, metrics: m, composer: c,
		defaultTZ: defaultTZ, from: WindowStart, to: WindowEnd}
}

// WithWindow replaces the quiet-hours window and returns the loop. Production
// never calls it; cmd/naviseed does, to run a pass at the real clock whatever hour
// the check happens to be run at, while the default window is what it asserts
// against at constructed instants.
func (p *Proactive) WithWindow(from, to string) *Proactive {
	p.from, p.to = from, to
	return p
}

// Loop describes this loop to the supervisor.
func (p *Proactive) Loop() supervisor.Loop {
	return supervisor.Loop{Name: Name, Interval: Interval, Tick: p.Tick}
}

// Result is what one pass did.
type Result struct {
	// Detected is episodes first seen this pass.
	Detected int

	// Sent reports that a proposal went out; Trigger and Key say which.
	Sent    bool
	Trigger string
	Key     string

	// Fallback reports that what was sent was the template.
	Fallback bool

	// Capped is triggers newly counted as held back by the daily cap.
	Capped int

	// Quiet reports that the pass was outside the window and so could not send;
	// Paused that a global pause suppressed it entirely (I6).
	Quiet  bool
	Paused bool
}

// LogValue renders the result as one group.
func (r Result) LogValue() slog.Value {
	return slog.GroupValue(
		slog.Int("detected", r.Detected),
		slog.Bool("sent", r.Sent),
		slog.String("trigger", r.Trigger),
		slog.Bool("fallback", r.Fallback),
		slog.Int("capped", r.Capped),
	)
}

// Tick is the loop body.
func (p *Proactive) Tick(ctx context.Context) error {
	res, err := p.Run(ctx, time.Now())
	if res.Detected > 0 || res.Sent || res.Capped > 0 {
		p.log.Info("proactive pass", "result", res)
	}
	return err
}

// priority orders what is owed when more than the cap allows is: a five-miss run
// first, a repeated snooze second, dormancy last.
func priority(trigger string) int {
	switch trigger {
	case domain.TriggerMisses:
		return 0
	case domain.TriggerSnoozes:
		return 1
	}
	return 2
}

// want is one condition currently holding.
type want struct {
	trigger string
	itemID  string
}

// Run is one pass. It takes its own now so every hour of the day is checkable, the
// way the other loops' entry points are.
func (p *Proactive) Run(ctx context.Context, now time.Time) (Result, error) {
	pausedUntil, paused, err := p.store.GlobalPauseUntil(ctx)
	if err != nil {
		return Result{}, err
	}
	if paused && pausedUntil.After(now) {
		return Result{Paused: true}, nil
	}

	zones, err := schedule.LoadZones(ctx, p.store, p.defaultTZ)
	if err != nil {
		return Result{}, fmt.Errorf("proactive: resolve zones: %w", err)
	}
	local := now.In(zones.Local())
	date := local.Format(domain.DateLayout)

	items, err := p.store.ListActiveItems(ctx)
	if err != nil {
		return Result{}, fmt.Errorf("proactive: list active items: %w", err)
	}
	byID := make(map[string]domain.Item, len(items))
	for _, it := range items {
		if it.Kind == domain.KindReminder {
			byID[it.ID] = it
		}
	}

	var res Result
	wants, keep, err := p.detect(ctx, now, byID)
	if err != nil {
		return res, err
	}
	fired, err := p.reconcileLatches(ctx, now, wants, keep)
	if err != nil {
		return res, err
	}
	for _, trigger := range fired {
		p.metrics.IncTrigger(trigger)
	}
	res.Detected = len(fired)

	if !p.inWindow(local) {
		res.Quiet = true
		return res, nil
	}
	return p.send(ctx, now, local, date, zones, byID, res)
}

// detect reads the conditions that hold right now. keep names latches whose
// condition cannot be evaluated this pass and so must be neither created nor
// cleared: an item on a pause is the case. A pause is not the condition clearing
// and it is not an answer, and treating it as either would re-ask the moment it
// lifted.
func (p *Proactive) detect(ctx context.Context, now time.Time, items map[string]domain.Item) (map[string]want, map[string]bool, error) {
	wants := map[string]want{}
	keep := map[string]bool{}

	for _, it := range items {
		key := "item:" + it.ID
		if it.IsPaused(now) {
			keep[key] = true
			continue
		}
		if s, err := schedule.Parse(it.Schedule); err != nil || s.Kind == schedule.KindOneOff {
			// A one-off has no run to be missed repeatedly and nothing to reshape.
			continue
		}
		sig, err := p.signals.ItemSignals(ctx, it.ID)
		if err != nil {
			return nil, nil, fmt.Errorf("proactive: signals for %s: %w", it.ID, err)
		}
		// Mutually exclusive on purpose: five misses is dormant by the copywriter's
		// reckoning too, and one item must be one question.
		switch {
		case sig.MissRunReached():
			wants[key] = want{domain.TriggerMisses, it.ID}
		case sig.Dormant():
			wants[key] = want{domain.TriggerDormant, it.ID}
		}
	}

	chains, err := p.signals.SnoozedChains(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("proactive: snoozed chains: %w", err)
	}
	for _, c := range chains {
		it, ok := items[c.ItemID]
		if !ok || it.IsPaused(now) {
			continue
		}
		if s, err := schedule.Parse(it.Schedule); err != nil || s.Kind == schedule.KindOneOff {
			continue
		}
		if threshold := stats.SnoozeThreshold(it.SnoozeCap); threshold > 0 && c.SnoozeCount >= threshold {
			wants["snooze:"+c.RootID] = want{domain.TriggerSnoozes, it.ID}
		}
	}
	return wants, keep, nil
}

// reconcileLatches brings the stored latches in line with the conditions, and
// returns the triggers of the episodes it created. See the package doc for the
// three things that end an episode: the condition clearing, the item going away,
// and time passing. A reply to the proposal is deliberately not one of them: a
// person who answered "leave it" has answered, and asking again the next hour
// would be the nag this loop exists to not be.
func (p *Proactive) reconcileLatches(ctx context.Context, now time.Time, wants map[string]want, keep map[string]bool) ([]string, error) {
	var fired []string
	err := p.store.UpdateLatches(ctx, now, func(m map[string]store.Latch) bool {
		fired = nil
		changed := false

		for k, l := range m {
			w, held := wants[k]
			switch {
			case keep[k]:
			case !held:
				delete(m, k)
				changed = true
			case l.State == store.LatchAsked && l.AskedAt != nil && now.Sub(*l.AskedAt) >= Rearm:
				delete(m, k)
				changed = true
			case l.State == store.LatchDetected && l.Trigger != w.trigger:
				// Still owed, but the reason moved (dormant became a five-miss run):
				// the message should be about what is true when it is sent.
				l.Trigger = w.trigger
				m[k] = l
				changed = true
			}
		}

		keys := make([]string, 0, len(wants))
		for k := range wants {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			if _, ok := m[k]; ok {
				continue
			}
			w := wants[k]
			m[k] = store.Latch{Key: k, Trigger: w.trigger, ItemID: w.itemID, State: store.LatchDetected, DetectedAt: now}
			fired = append(fired, w.trigger)
			changed = true
		}
		return changed
	})
	return fired, err
}

// send picks what is owed, applies the cap, and sends at most one proposal.
func (p *Proactive) send(ctx context.Context, now, local time.Time, date string, zones schedule.Zones,
	items map[string]domain.Item, res Result) (Result, error) {

	latches, err := p.store.Latches(ctx)
	if err != nil {
		return res, err
	}
	var owed []store.Latch
	for _, l := range latches {
		if l.State == store.LatchDetected {
			if _, ok := items[l.ItemID]; ok {
				owed = append(owed, l)
			}
		}
	}
	if len(owed) == 0 {
		return res, nil
	}
	sort.Slice(owed, func(i, j int) bool {
		a, b := owed[i], owed[j]
		if pa, pb := priority(a.Trigger), priority(b.Trigger); pa != pb {
			return pa < pb
		}
		if !a.DetectedAt.Equal(b.DetectedAt) {
			return a.DetectedAt.Before(b.DetectedAt)
		}
		return a.Key < b.Key
	})

	count, err := p.store.ProactiveCount(ctx, date)
	if err != nil {
		return res, err
	}
	if count >= DailyCap {
		// Held, not dropped: the latches stay detected, and tomorrow's first
		// in-window pass sends the first of them. Counted once per trigger per day.
		capped := 0
		err := p.store.UpdateLatches(ctx, now, func(m map[string]store.Latch) bool {
			capped = 0
			for _, o := range owed {
				if l, ok := m[o.Key]; ok && l.CappedOn != date {
					l.CappedOn = date
					m[o.Key] = l
					capped++
				}
			}
			return capped > 0
		})
		if err != nil {
			return res, err
		}
		for i := 0; i < capped; i++ {
			p.metrics.IncProposalCapped()
		}
		res.Capped = capped
		return res, nil
	}

	l := owed[0]
	item := items[l.ItemID]
	loc, err := zones.For(item)
	if err != nil {
		return res, fmt.Errorf("proactive: zone for %s: %w", item.ID, err)
	}

	var live *time.Time
	if l.Trigger == domain.TriggerSnoozes {
		occs, err := p.store.ListOccurrencesForItem(ctx, item.ID)
		if err != nil {
			return res, fmt.Errorf("proactive: occurrences for %s: %w", item.ID, err)
		}
		live = liveLink(occs, strings.TrimPrefix(l.Key, "snooze:"))
	}

	cand := Candidate{Key: l.Key, Trigger: l.Trigger, Item: item, Alt: Alternate(item, l.Trigger, live, loc, now)}
	text, fellBack, cerr := composeProposal(ctx, p.composer, cand)
	if cerr != nil {
		p.log.Warn("proposal composition fell back to the template", "item", item.ID, "err", cerr)
	}

	sendCtx, cancel := context.WithTimeout(ctx, sendTimeout)
	defer cancel()
	body := transport.Truncate(text, p.notifier.Capabilities().MaxBodyLength)
	if _, err := p.notifier.Send(sendCtx, transport.Outbound{Body: body}); err != nil {
		// Nothing written. The latch is still detected, the count is unspent, and
		// the next pass tries again.
		return res, fmt.Errorf("proactive: send for %s: %w", l.Key, err)
	}

	prop := store.NewProposal(domain.Proposal{
		Trigger: l.Trigger, ItemID: item.ID, ItemTitle: item.Title,
		Tool: cand.Alt.Tool, Changes: cand.Alt.Changes, Summary: cand.Alt.Summary, Mentions: cand.Alt.Mentions,
		Rationale: cand.reason(),
	}, now)
	yesterday := local.AddDate(0, 0, -1).Format(domain.DateLayout)
	if err := p.store.RecordProactiveSend(ctx, prop, body, l.Key, date, yesterday, now); err != nil {
		return res, errors.Join(err, fmt.Errorf("proactive: %s was sent but not recorded and will be sent again", l.Key))
	}

	p.metrics.IncProposalSent(l.Trigger)
	if fellBack {
		p.metrics.IncProposalFallback()
	}
	res.Sent, res.Trigger, res.Key, res.Fallback = true, l.Trigger, l.Key, fellBack
	return res, nil
}

// liveLink is the local time the last link of the chain rooted at rootID sits at,
// or nil if the chain has no children. It walks parent links from the root, so it
// needs no query of its own.
func liveLink(occs []domain.Occurrence, rootID string) *time.Time {
	child := make(map[string]domain.Occurrence, len(occs))
	for _, o := range occs {
		if o.ParentOccurrenceID != nil {
			child[*o.ParentOccurrenceID] = o
		}
	}
	var last *time.Time
	for cur := rootID; ; {
		o, ok := child[cur]
		if !ok {
			return last
		}
		t := o.StartsAt
		last = &t
		cur = o.ID
	}
}

// inWindow reports whether a local time is inside the quiet-hours window. The
// start is inclusive and the end exclusive, so 09:00 sends and 20:00 does not.
func (p *Proactive) inWindow(local time.Time) bool {
	from, errFrom := minutes(p.from)
	to, errTo := minutes(p.to)
	if errFrom != nil || errTo != nil {
		return false // constants; an unparseable one sends nothing rather than everything
	}
	m := local.Hour()*60 + local.Minute()
	return m >= from && m < to
}

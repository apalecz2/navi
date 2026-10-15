// Package copywriter generates the per-occurrence reminder text ahead of time,
// on the two-pass schedule in G3: a safety-net pass roughly 30 minutes out and a
// regeneration pass roughly 4 minutes out if relevant state changed.
//
// Everything here is best-effort, and nothing here is in the firing path. The
// scheduler reads message_text off the row, or the title when there is none, and
// that path is unchanged: invariant 1 holds because this package writes a column
// the scheduler reads and never the other way round, and because it cannot
// reach the scheduler or the transport at all. The one way it could still do
// harm is a race - writing text to a row the scheduler has just claimed - and
// every write goes through store.WriteGeneratedMessage, whose statement carries
// AND status = 'pending', so a late generation is a zero-row update.
//
// Three decisions the spec left open are made here, and each is stated where it
// is enforced:
//
//   - G9 (Q-7): an item with fewer than MinResolvedChains resolved chains gets no
//     model call at all. Evaluated here, per candidate, on every tick, against
//     the item's chains read through internal/stats - not at materialization,
//     where there is no history yet to count, and never in the scheduler.
//   - Pass 2 asks whether anything it would write from has changed, by comparing
//     a fingerprint of the context against the one stored with pass 1's text
//     (occurrences.message_basis). See fingerprint.
//   - A snooze child is generated as soon as it exists. See Run's comment on
//     the pass-1 window.
//
// Failure is bounded by an attempt counter (G4) and leaves message_text as it
// was, which the scheduler reads as "send the plain title". A stalled copywriter
// is a cosmetic outage, not a delivery one.
package copywriter

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/aidenpaleczny/navi/internal/domain"
	"github.com/aidenpaleczny/navi/internal/schedule"
	"github.com/aidenpaleczny/navi/internal/stats"
	"github.com/aidenpaleczny/navi/internal/store"
	"github.com/aidenpaleczny/navi/internal/supervisor"
)

// Name is the loop's identity in logs, /healthz, and the metric labels.
const Name = "copywriter"

// Interval is the poll interval. The tighter of the two passes is four minutes
// out, so a minute of granularity is enough.
const Interval = 60 * time.Second

const (
	// MinResolvedChains is G9's threshold, Q-7's "roughly 10 resolved occurrences
	// per item". It counts chains that reached an outcome (completed, skipped or
	// missed) rather than rows or days: a weekly reminder accumulates history far
	// more slowly than a daily one, and a snooze chain is one chain. Below it the
	// item fires as its title with no model call and no llm_calls row.
	MinResolvedChains = 10

	// MaxAttempts is G4's cap, per pass. A successful write resets the counter,
	// so pass 2 has its own two.
	MaxAttempts = 2

	// Pass1Lead and Pass2Lead are G3's two windows. A row is a pass-1 candidate
	// once it starts within Pass1Lead and has no text, and a pass-2 candidate once
	// it starts within Pass2Lead and has safety-net text.
	Pass1Lead = 30 * time.Minute
	Pass2Lead = 4 * time.Minute

	// MinLead is the shortest lead worth a model call. A row starting sooner is
	// left alone: the call could not return before the scheduler claims it, and
	// the guarded write would discard the result.
	MinLead = 90 * time.Second

	// RecentMessageCount is G6's "last three to five".
	RecentMessageCount = 5

	// maxCandidates bounds the read; maxCalls bounds the model calls one tick may
	// make, and tickBudget the wall time it may spend starting them. Supervisor
	// ticks are serial, so an unbounded pass over a hung provider would stale
	// /healthz for a loop that is working fine.
	maxCandidates = 50
	maxCalls      = 5
	tickBudget    = 50 * time.Second
)

// Store is the slice of the repository the copywriter uses.
type Store interface {
	ListCopyCandidates(ctx context.Context, w store.CopyWindow) ([]store.CopyCandidate, error)
	ClaimGenerationAttempt(ctx context.Context, id string, pass, maxAttempts int) (bool, error)
	WriteGeneratedMessage(ctx context.Context, id string, g store.Generated) (bool, error)
	RecentMessages(ctx context.Context, itemID, excludeID string, n int) ([]string, error)
	GetItem(ctx context.Context, id string) (domain.Item, error)
	GlobalPauseUntil(ctx context.Context) (time.Time, bool, error)
	CurrentTZ(ctx context.Context) (string, bool, error)
}

// Signals is internal/stats' per-item read. Declared here so the numbers in a
// prompt cannot come from anywhere else: this package has no chain to count.
type Signals interface {
	ItemSignals(ctx context.Context, itemID string) (stats.ItemSignals, error)
}

// Metrics is the copywriter's slice of the registry.
type Metrics interface {
	IncCopywriterGeneration(pass, outcome string)
}

// Copywriter generates occurrence message text.
type Copywriter struct {
	log       *slog.Logger
	store     Store
	signals   Signals
	gen       Generator
	metrics   Metrics
	defaultTZ *time.Location
}

// New returns a copywriter. gen is nil when there is no model client, which is an
// ordinary configuration and not a degraded one: every reminder then fires as its
// title and nothing here runs.
func New(log *slog.Logger, st Store, sig Signals, gen Generator, m Metrics, defaultTZ *time.Location) *Copywriter {
	return &Copywriter{log: log, store: st, signals: sig, gen: gen, metrics: m, defaultTZ: defaultTZ}
}

// Loop describes this loop to the supervisor.
func (c *Copywriter) Loop() supervisor.Loop {
	return supervisor.Loop{Name: Name, Interval: Interval, Tick: c.Tick}
}

// Result is what one pass did, for the loop's log line and for naviseed.
type Result struct {
	// Disabled: there is no generator, so nothing ran. Paused: vacation mode.
	Disabled bool
	Paused   bool

	// Candidates is what the read returned.
	Candidates int

	// Gated were skipped by G9 and Unchanged by pass 2's fingerprint. Neither made
	// a model call, claimed an attempt, or wrote a row.
	Gated     int
	Unchanged int

	// Deferred were eligible but left for the next tick by the per-tick bounds.
	Deferred int

	// Pass1 and Pass2 are messages stored; Failed and Late are attempts that made
	// a call and did not produce a stored message.
	Pass1  int
	Pass2  int
	Failed int
	Late   int
}

// LogValue renders the result as one group.
func (r Result) LogValue() slog.Value {
	return slog.GroupValue(
		slog.Int("candidates", r.Candidates),
		slog.Int("gated", r.Gated),
		slog.Int("unchanged", r.Unchanged),
		slog.Int("pass1", r.Pass1),
		slog.Int("pass2", r.Pass2),
		slog.Int("failed", r.Failed),
		slog.Int("late", r.Late),
		slog.Int("deferred", r.Deferred),
	)
}

// Tick is the loop body: one pass, then a line if it did anything.
func (c *Copywriter) Tick(ctx context.Context) error {
	res, err := c.Run(ctx, time.Now())
	if res.Pass1+res.Pass2+res.Failed+res.Late > 0 {
		c.log.Info("messages generated", "result", res)
	}
	return err
}

// Run is the synchronous entry point, so naviseed can drive one pass at a chosen
// instant without a ticker.
//
// The windows are "has started" conditions and not clock slots: pass 1 takes any
// text-less row that starts within thirty minutes, which is what makes a snooze
// child work. A 10-minute snooze is created with its T-30 already behind it, and
// under a slot reading it would never be generated; under this one it is
// generated on the next tick - it is already inside the window, and it gets the
// snooze-child strategy because it is one. The same reading covers a restart, a
// first boot, and a row the materializer topped up late. The cost is that a row
// created with less than MinLead to spare fires as its title, which is correct.
func (c *Copywriter) Run(ctx context.Context, now time.Time) (Result, error) {
	var res Result
	if c.gen == nil {
		res.Disabled = true
		return res, nil
	}

	// Vacation mode, on the scheduler's own reading. Nothing fires while it lasts,
	// so a model call would have no reader.
	pausedUntil, paused, err := c.store.GlobalPauseUntil(ctx)
	if err != nil {
		return res, fmt.Errorf("copywriter: read global pause: %w", err)
	}
	if paused && pausedUntil.After(now) {
		res.Paused = true
		return res, nil
	}

	zones, err := schedule.LoadZones(ctx, c.store, c.defaultTZ)
	if err != nil {
		return res, fmt.Errorf("copywriter: resolve zones: %w", err)
	}

	cands, err := c.store.ListCopyCandidates(ctx, store.CopyWindow{
		From:        now.Add(MinLead),
		Pass1Until:  now.Add(Pass1Lead),
		Pass2Until:  now.Add(Pass2Lead),
		Now:         now,
		MaxAttempts: MaxAttempts,
		Limit:       maxCandidates,
	})
	if err != nil {
		return res, fmt.Errorf("copywriter: list candidates: %w", err)
	}
	res.Candidates = len(cands)

	deadline := time.Now().Add(tickBudget)
	calls := 0
	var errs []error
	for _, cand := range cands {
		if err := ctx.Err(); err != nil {
			errs = append(errs, fmt.Errorf("copywriter: cancelled with candidates unread: %w", err))
			break
		}

		// Decide before spending anything. Everything down to the attempt claim
		// reads; the claim is the first write this candidate can cause.
		item, err := c.store.GetItem(ctx, cand.ItemID)
		if err != nil {
			errs = append(errs, fmt.Errorf("copywriter: item %s: %w", cand.ItemID, err))
			continue
		}
		sig, err := c.signals.ItemSignals(ctx, cand.ItemID)
		if err != nil {
			errs = append(errs, fmt.Errorf("copywriter: signals for %s: %w", cand.ItemID, err))
			continue
		}

		// G9. Counted in chains, evaluated here, every tick. No model call, no
		// attempt, no llm_calls row: the row fires as its title.
		if sig.Resolved < MinResolvedChains {
			res.Gated++
			continue
		}

		recent, err := c.store.RecentMessages(ctx, cand.ItemID, cand.ID, RecentMessageCount)
		if err != nil {
			errs = append(errs, fmt.Errorf("copywriter: recent messages for %s: %w", cand.ItemID, err))
			continue
		}
		loc, err := zones.For(item)
		if err != nil {
			errs = append(errs, fmt.Errorf("copywriter: zone for %s: %w", cand.ItemID, err))
			continue
		}

		blob := buildBlob(item, cand, sig, recent, loc)
		strategy := Choose(blob)
		pass := cand.GenerationPass + 1
		basis := fingerprint(blob, strategy)

		// Pass 2 only when the inputs moved. An unchanged row costs this read and
		// nothing else, which is why it is re-evaluated on every tick of its
		// four-minute window instead of being latched.
		if pass == domain.GenerationPassFresh && cand.MessageBasis != nil && *cand.MessageBasis == basis {
			res.Unchanged++
			continue
		}

		if calls >= maxCalls || time.Now().After(deadline) {
			res.Deferred++
			continue
		}

		granted, err := c.store.ClaimGenerationAttempt(ctx, cand.ID, cand.GenerationPass, MaxAttempts)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if !granted {
			// Claimed by the scheduler, resolved, moved on a pass, or out of
			// attempts since the read. All of them mean "not this one".
			continue
		}
		calls++

		out, err := c.gen.Generate(ctx, Request{OccurrenceID: cand.ID, Blob: blob, Strategy: strategy})
		if err != nil {
			res.Failed++
			c.metrics.IncCopywriterGeneration(passLabel(pass), "failed")
			c.log.Warn("generation failed; the reminder keeps what it has",
				"occurrence", cand.ID, "pass", pass, "err", err)
			continue
		}

		landed, err := c.store.WriteGeneratedMessage(ctx, cand.ID, store.Generated{
			Text: out.Text, Model: out.Model, At: time.Now(), Pass: pass, Basis: basis,
		})
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if !landed {
			res.Late++
			c.metrics.IncCopywriterGeneration(passLabel(pass), "late")
			c.log.Info("generated text discarded: the row had already left pending",
				"occurrence", cand.ID, "pass", pass)
			continue
		}
		if pass == domain.GenerationPassSafety {
			res.Pass1++
		} else {
			res.Pass2++
		}
		c.metrics.IncCopywriterGeneration(passLabel(pass), "generated")
	}
	return res, errors.Join(errs...)
}

func passLabel(pass int) string {
	if pass == domain.GenerationPassFresh {
		return "2"
	}
	return "1"
}

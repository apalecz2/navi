package reconciler

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/aidenpaleczny/navi/internal/domain"
	"github.com/aidenpaleczny/navi/internal/schedule"
	"github.com/aidenpaleczny/navi/internal/store"
)

// Expiry is what one grace pass did, for the log line and for naviseed.
type Expiry struct {
	// Candidates is how many rows were still awaiting an answer, expired or
	// not. Missed is how many of them the pass actually wrote.
	Candidates int
	Missed     int

	// Paused reports that a global pause suppressed the pass (I6). Nothing is
	// read and nothing is written.
	Paused bool
}

// LogValue renders the result as one group rather than three top-level keys.
func (e Expiry) LogValue() slog.Value {
	return slog.GroupValue(
		slog.Int("candidates", e.Candidates),
		slog.Int("missed", e.Missed),
	)
}

// ExpireGrace assigns missed to every occurrence a check-in asked about and got
// no answer for inside its grace window. It is the other half of D-008, and the
// only thing in the tree that assigns missed for the reason K6 gives it.
//
// The ordering that matters here is not inside this function, it is that this
// function exists at all and Reconcile ran first. missed means "asked and got
// nothing": the ask is occurrences.reconciled_at, written by RecordCheckIn, and
// a row that carries none is invisible to the query below no matter how long
// ago it was due. Midnight passing is not an event this pass can observe.
//
// A global pause suppresses it entirely, the same way it suppresses the
// check-in. Those rows keep their reconciled_at and are missed when the pause
// lifts - they were asked, and vacation mode is a reason not to conclude
// anything today rather than a reason to forgive the question. An item-scoped
// pause is deliberately not consulted; see the query's comment in
// queries/occurrences.sql for why the two scopes differ here.
func (r *Reconciler) ExpireGrace(ctx context.Context, now time.Time) (Expiry, error) {
	pausedUntil, paused, err := r.store.GlobalPauseUntil(ctx)
	if err != nil {
		return Expiry{}, err
	}
	if paused && pausedUntil.After(now) {
		return Expiry{Paused: true}, nil
	}

	// The device zone, and the same one the pass that asked resolved its day
	// in. domain.GraceDeadline says why it is not the item's.
	zones, err := schedule.LoadZones(ctx, r.store, r.defaultTZ)
	if err != nil {
		return Expiry{}, fmt.Errorf("reconciler: resolve zones: %w", err)
	}

	awaiting, err := r.store.ListAwaitingReconciliation(ctx, zones.Local())
	if err != nil {
		return Expiry{}, err
	}

	res := Expiry{Candidates: len(awaiting)}

	rows := make([]store.BulkResolution, 0, len(awaiting))
	for _, a := range awaiting {
		if a.Deadline.After(now) {
			continue // still answerable; the agent is still offering it
		}
		// No note. A note is the user's own words about why something did not
		// happen, which is what makes a skip a skip (R2) - and the defining
		// property of a miss is that they said nothing at all.
		rows = append(rows, store.BulkResolution{
			OccurrenceID: a.ID,
			Status:       domain.StatusMissed,
		})
	}
	if len(rows) == 0 {
		return res, nil
	}

	// The same atomic write every other surface reaches, with the reconciler's
	// own source. A concurrent resolution landing between the read above and
	// this write aborts the batch, because terminal -> missed is a rejected
	// transition rather than a no-op. That is correct and it self-heals: the
	// row that beat us is now terminal, so the next tick's read no longer
	// returns it and the batch that was blocked goes through. Sixty seconds,
	// and no row is missed that was answered.
	resolved, err := r.store.BulkResolve(ctx, rows, domain.ResolvedByReconciler, now)
	if err != nil {
		return res, err
	}

	for _, got := range resolved {
		if got.Outcome != domain.OutcomeApplied {
			continue
		}
		res.Missed++
		r.metrics.IncTransition(
			string(got.Previous),
			string(got.Occurrence.Status),
			string(domain.ResolvedByReconciler),
		)
	}
	return res, nil
}

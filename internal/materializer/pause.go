package materializer

import (
	"context"
	"fmt"
	"time"

	"github.com/aidenpaleczny/navi/internal/domain"
	"github.com/aidenpaleczny/navi/internal/schedule"
	"github.com/aidenpaleczny/navi/internal/store"
)

// Pause sets or lifts items.paused_until and re-plans that item, in one
// transaction (I6). A nil until lifts the pause.
//
// It lives here rather than in each caller because the plan closure can only be
// built from this package - PlanFor takes the unexported run - and because both
// pause surfaces need the identical twenty lines. POST /api/items/{id}/pause
// and the agent's pause tool are both a decode and a call to this, which is the
// same arrangement that keeps two snooze surfaces from resolving a delta two
// ways.
//
// The re-plan is what makes a pause do anything to occurrences that already
// exist: the item handed to PlanFor is the one that comes back from the write,
// already carrying the new paused_until, so run.paused excludes every slot
// inside the window and the pending rows sitting there fall into the plan's
// Delete set. Nothing about this knows what a pause is - it is the same
// machinery an archived item takes, and the same SQL guard keeps it away from
// history and from overrides.
//
// Like Item, it does not move the horizon: one item says nothing about how far
// ahead the rest of them reach.
func (m *Materializer) Pause(ctx context.Context, id string, until *time.Time) (domain.Item, store.Applied, error) {
	r, err := m.begin(ctx)
	if err != nil {
		return domain.Item{}, store.Applied{}, err
	}

	current, err := m.store.GetItem(ctx, id)
	if err != nil {
		return domain.Item{}, store.Applied{}, err
	}

	// Parse and not Prepare: this is the read path decoding a stored value, not
	// the write path completing one from the defaults table. An inactive or
	// archived item plans an empty set with no schedule at all, which is why
	// PlanFor ignores sched in that case and why this skips the parse.
	var sched schedule.Schedule
	if current.Active && current.ArchivedAt == nil {
		if sched, err = schedule.Parse(current.Schedule); err != nil {
			return domain.Item{}, store.Applied{}, err
		}
	}

	item, applied, err := m.store.PauseItemAndMaterialize(ctx, id, until, r.Now(),
		func(it domain.Item, existing []domain.Occurrence) (store.Plan, error) {
			return m.PlanFor(it, sched, r, existing)
		})
	if err != nil {
		return domain.Item{}, store.Applied{}, err
	}
	return item, applied, nil
}

// PauseAll sets or lifts kv.global_pause_until and re-plans every active item.
// A nil until lifts the pause.
//
// Two writes rather than one transaction, deliberately. Materializing every
// item inside a single BEGIN IMMEDIATE would hold the only writer connection
// for the length of a full nightly run, which is the shape the whole design
// avoids; instead the kv row goes first so the re-plan that follows reads the
// window it is meant to honour. A crash in between leaves the pause set with
// stale pending rows inside it, which is a state both the nightly run and the
// sweeper's backfill correct on their own - and which never fires a
// notification, because the scheduler consults the same kv row before it claims
// anything.
//
// It runs All, so it does move the horizon. That is correct: it has just
// re-planned every item, which is exactly the claim the horizon makes.
func (m *Materializer) PauseAll(ctx context.Context, until *time.Time) (Result, error) {
	if until == nil {
		if err := m.store.ClearGlobalPause(ctx); err != nil {
			return Result{}, err
		}
	} else if err := m.store.SetGlobalPauseUntil(ctx, *until); err != nil {
		return Result{}, err
	}

	res, err := m.All(ctx)
	if err != nil {
		return res, fmt.Errorf("materializer: re-plan after global pause: %w", err)
	}
	return res, nil
}

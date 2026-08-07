package materializer

import (
	"log/slog"
	"math/rand/v2"
	"time"

	"github.com/aidenpaleczny/navi/internal/domain"
	"github.com/aidenpaleczny/navi/internal/schedule"
	"github.com/aidenpaleczny/navi/internal/store"
)

// request is everything one item's materialization needs, gathered so the
// expansion functions can be methods on it rather than take eight parameters
// each. It is built per item and read-only from there.
type request struct {
	item     domain.Item
	schedule schedule.Schedule

	// generate is false for an item that should have no future occurrences at
	// all — inactive, or archived. Its slots come out empty, which deletes the
	// pending rows it still has and leaves everything else alone. That is the
	// right behaviour for archiving in P1, and it falls out rather than being a
	// second code path.
	generate bool

	loc *time.Location
	run run
	rnd *rand.Rand
	log *slog.Logger
}

// plan decides what to delete and what to insert, given the item's future rows
// as they are inside the write transaction.
//
// The shape is keep-and-top-up rather than the delete-then-regenerate the
// pseudocode in docs/05-schedule-spec.md#materialization describes. The
// resulting set is the same, and two things fall out of it that the other
// ordering loses: a drawn time that already exists is kept rather than redrawn,
// which is what makes re-materialization idempotent for windowed and fuzzy
// (D-005); and a row the copywriter has already generated message_text for
// survives the night, instead of being deleted and rewritten empty for another
// model call to fill.
func (rq request) plan(existing []domain.Occurrence) (store.Plan, error) {
	// A row inside a pause window cannot fill a slot, because no slot is
	// generated there. A pending one therefore ends up in Delete, which is what
	// "occurrences that already exist inside a newly-created pause window are
	// deleted if pending" asks for, without a second pass that knows about
	// pauses.
	eligible := make([]*domain.Occurrence, 0, len(existing))
	for i := range existing {
		if rq.run.paused(rq.item, existing[i].StartsAt) {
			continue
		}
		eligible = append(eligible, &existing[i])
	}

	var (
		slots []slot
		err   error
	)
	if rq.generate {
		if slots, err = rq.expand(eligible); err != nil {
			return store.Plan{}, err
		}
	}

	var plan store.Plan
	kept := make(map[string]bool, len(slots))
	for _, s := range slots {
		if s.have != nil {
			kept[s.have.ID] = true
			continue
		}
		plan.Insert = append(plan.Insert, domain.NewOccurrence{
			ItemID:   rq.item.ID,
			StartsAt: s.at,
		})
	}

	// Everything else that is still this run's to touch goes. The two exclusions
	// are restated here rather than left to the store's guard, because a planner
	// that names rows it knows are protected is a planner whose delete counts
	// cannot be trusted — and the guard exists to catch the bug, not to be the
	// design.
	for i := range existing {
		occ := existing[i]
		if kept[occ.ID] || occ.IsOverride || occ.Status != domain.StatusPending {
			continue
		}
		plan.Delete = append(plan.Delete, occ.ID)
	}

	return plan, nil
}

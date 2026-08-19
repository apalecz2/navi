package agent

import (
	"context"
	"time"

	"github.com/aidenpaleczny/navi/internal/domain"
	"github.com/aidenpaleczny/navi/internal/schedule"
	"github.com/aidenpaleczny/navi/internal/store"
)

// Occurrence is the next-three shape 07-api-spec.md's next_occurrences and
// 06-agent-spec.md's confirmation both want: an id (so a follow-up "push
// that one" can reference it) and a formatted instant, ready to print or
// hand to a model verbatim without another domain.FormatTime call at the far
// end.
type Occurrence struct {
	ID       string `json:"id"`
	StartsAt string `json:"starts_at"`
}

// Resolved is one row of bulk_resolve's answer: what the occurrence is now, and
// whether this call is what made it so.
//
// Applied false is not a failure. It is the idempotency table's second row —
// the occurrence was already in the requested terminal state — and the
// confirmation is better for saying "already done" than for claiming credit.
type Resolved struct {
	OccurrenceID string `json:"occurrence_id"`
	Status       string `json:"status"`
	Applied      bool   `json:"applied"`

	// ChainCompleted is the snooze chain rolled up (D-011, R7): true when any
	// link of this chain has completed, which is what a streak counts. A
	// reminder that was never snoozed is a chain of one and this simply mirrors
	// its own status.
	ChainCompleted bool `json:"chain_completed"`
}

// Result is what every handler returns. Fields are populated per tool:
// list_items sets Items; create_item and update_item set Item, Applied,
// NextOccurrences and Inferred; delete_item sets Item and Applied only;
// bulk_resolve sets Resolutions only.
type Result struct {
	Item            *domain.Item  `json:"item,omitempty"`
	Items           []domain.Item `json:"items,omitempty"`
	NextOccurrences []Occurrence  `json:"next_occurrences,omitempty"`
	Resolutions     []Resolved    `json:"resolutions,omitempty"`

	// Inferred is the vocabulary-default fields resolveSchedule filled in
	// that the caller did not supply - A5's "state every inferred
	// parameter." Empty when the schedule was fully specified, or for a
	// handler that never resolves a schedule at all.
	Inferred []schedule.Inference `json:"inferred,omitempty"`

	// Applied is diagnostic only - naviseed and future logging read it, a
	// model never sees it.
	Applied store.Applied `json:"-"`
}

// nextOccurrences reads the first three pending, future, non-override rows
// for an item - the confirmation's material, present before any
// confirmation UI exists so the plumbing is provably correct the moment it
// is needed.
func nextOccurrences(ctx context.Context, st *store.Store, itemID string, after time.Time) ([]Occurrence, error) {
	rows, err := st.ListOccurrencesForItem(ctx, itemID) // already ordered by starts_at, id
	if err != nil {
		return nil, err
	}
	out := make([]Occurrence, 0, 3)
	for _, occ := range rows {
		if occ.Status != domain.StatusPending || !occ.StartsAt.After(after) {
			continue
		}
		out = append(out, Occurrence{ID: occ.ID, StartsAt: domain.FormatTime(occ.StartsAt)})
		if len(out) == 3 {
			break
		}
	}
	return out, nil
}

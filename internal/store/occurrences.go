package store

import (
	"context"
	"fmt"
	"time"

	"github.com/aidenpaleczny/navi/internal/domain"
	"github.com/aidenpaleczny/navi/internal/store/sqlc"
)

// CreateOccurrence writes one materialized instance. The parent item is read
// first because the valid status set depends on its kind, and validating that
// here is what keeps the schema free of a CHECK constraint that would be wrong
// for half the rows (D-017).
func (s *Store) CreateOccurrence(ctx context.Context, n domain.NewOccurrence) (domain.Occurrence, error) {
	item, err := s.GetItem(ctx, n.ItemID)
	if err != nil {
		return domain.Occurrence{}, fmt.Errorf("store: create occurrence: %w", err)
	}

	var occ domain.Occurrence
	err = s.tx(ctx, func(q *sqlc.Queries) error {
		var err error
		occ, err = insertOccurrence(ctx, q, n, item.Kind)
		return err
	})
	if err != nil {
		return domain.Occurrence{}, err
	}
	return occ, nil
}

// insertOccurrence is the row-level write, taking the transaction's queries and
// the parent item's kind rather than reading either for itself. Materialization
// inserts a run of rows for an item it has already loaded, and re-reading that
// item once per row would be both wasteful and a second answer to a question
// already settled.
func insertOccurrence(ctx context.Context, q *sqlc.Queries, n domain.NewOccurrence, kind domain.Kind) (domain.Occurrence, error) {
	n = n.WithDefaults()
	if err := n.Validate(kind); err != nil {
		return domain.Occurrence{}, err
	}

	row, err := q.CreateOccurrence(ctx, sqlc.CreateOccurrenceParams{
		ID:                 domain.NewID(),
		ItemID:             n.ItemID,
		StartsAt:           domain.FormatTime(n.StartsAt),
		EndsAt:             formatTimePtr(n.EndsAt),
		Status:             string(*n.Status),
		IsOverride:         boolToInt(n.IsOverride),
		ParentOccurrenceID: n.ParentOccurrenceID,
		SnoozeDepth:        int64(*n.SnoozeDepth),
		MessageText:        n.MessageText,
		CreatedAt:          domain.FormatTime(time.Now()),
	})
	if err != nil {
		return domain.Occurrence{}, fmt.Errorf("store: create occurrence: %w", err)
	}
	return toDomainOccurrence(row)
}

// GetOccurrence returns one occurrence, or ErrNotFound.
func (s *Store) GetOccurrence(ctx context.Context, id string) (domain.Occurrence, error) {
	row, err := s.read.GetOccurrence(ctx, id)
	if err != nil {
		return domain.Occurrence{}, notFound("store: get occurrence", err)
	}
	return toDomainOccurrence(row)
}

// ListOccurrencesForItem returns an item's whole history in start order.
func (s *Store) ListOccurrencesForItem(ctx context.Context, itemID string) ([]domain.Occurrence, error) {
	rows, err := s.read.ListOccurrencesForItem(ctx, itemID)
	if err != nil {
		return nil, fmt.Errorf("store: list occurrences for item: %w", err)
	}

	occurrences := make([]domain.Occurrence, 0, len(rows))
	for _, row := range rows {
		occ, err := toDomainOccurrence(row)
		if err != nil {
			return nil, fmt.Errorf("store: list occurrences for item: %w", err)
		}
		occurrences = append(occurrences, occ)
	}
	return occurrences, nil
}

// Plan is one item's materialization decision: which of its future rows to drop
// and which to add. Rows that are neither are kept, which is what makes a
// re-materialization idempotent and is why this is a plan rather than a list of
// occurrences to write.
type Plan struct {
	// Delete names occurrence ids. Only pending, non-override, future rows can
	// actually go; see MaterializeItem.
	Delete []string

	Insert []domain.NewOccurrence
}

// Applied is what a materialization did, for the loop's log line and for
// naviseed to assert "running it twice changed nothing" against.
type Applied struct {
	Deleted  int
	Inserted int
	Kept     int
}

// Changed reports whether the run touched anything. A second run over unchanged
// items must return false.
func (a Applied) Changed() bool { return a.Deleted > 0 || a.Inserted > 0 }

// MaterializeItem re-materializes one item inside a single transaction.
//
// plan runs inside that transaction, against the item's future rows as they are
// at that moment. That ordering is the point: expansion decides what to keep by
// looking at what exists, and a read taken before the transaction opened can be
// stale by the time the writes land — the nightly run and P1's synchronous
// re-materialization on a schedule change are two callers of exactly this, both
// through the one writer connection.
//
// The three invariants in docs/05-schedule-spec.md#materialization survive a
// buggy planner, because the delete is guarded in SQL rather than by the caller:
// a row that is resolved, or an override, or already in the past cannot be named
// into deletion. History is immutable by statement, not by convention.
func (s *Store) MaterializeItem(
	ctx context.Context,
	item domain.Item,
	now time.Time,
	plan func(existing []domain.Occurrence) (Plan, error),
) (Applied, error) {
	var applied Applied
	err := s.tx(ctx, func(q *sqlc.Queries) error {
		var err error
		applied, err = s.materializeTx(ctx, q, item, now, plan)
		return err
	})
	if err != nil {
		return Applied{}, err
	}
	return applied, nil
}

// materializeTx is the body, separated from the transaction that wraps it so
// that P1 can write an item and re-materialize it in one transaction by calling
// this alongside the item write. Nothing outside this package ever holds the
// queries handle.
func (s *Store) materializeTx(
	ctx context.Context,
	q *sqlc.Queries,
	item domain.Item,
	now time.Time,
	plan func(existing []domain.Occurrence) (Plan, error),
) (Applied, error) {
	nowText := domain.FormatTime(now)

	rows, err := q.ListFutureOccurrencesForItem(ctx, sqlc.ListFutureOccurrencesForItemParams{
		ItemID:   item.ID,
		StartsAt: nowText,
	})
	if err != nil {
		return Applied{}, fmt.Errorf("store: list future occurrences: %w", err)
	}

	existing := make([]domain.Occurrence, 0, len(rows))
	for _, row := range rows {
		occ, err := toDomainOccurrence(row)
		if err != nil {
			return Applied{}, fmt.Errorf("store: list future occurrences: %w", err)
		}
		existing = append(existing, occ)
	}

	p, err := plan(existing)
	if err != nil {
		return Applied{}, err
	}

	var applied Applied
	for _, id := range p.Delete {
		n, err := q.DeleteFuturePendingOccurrence(ctx, sqlc.DeleteFuturePendingOccurrenceParams{
			ID:       id,
			ItemID:   item.ID,
			StartsAt: nowText,
		})
		if err != nil {
			return Applied{}, fmt.Errorf("store: delete future pending occurrence: %w", err)
		}
		if n == 0 {
			// The guard refused it. Nothing else can have moved the row — this
			// is inside the write transaction on the only writer — so this is a
			// planner naming a row it had no business naming, and it is worth a
			// line rather than a silent no-op.
			s.log.Warn("materialize: refused delete", "item", item.ID, "occurrence", id)
			continue
		}
		applied.Deleted += int(n)
	}

	for _, n := range p.Insert {
		if _, err := insertOccurrence(ctx, q, n, item.Kind); err != nil {
			return Applied{}, err
		}
		applied.Inserted++
	}

	applied.Kept = len(existing) - applied.Deleted
	return applied, nil
}

// PendingOverdue counts occurrences the scheduler should already have claimed:
// pending, at least OverdueGrace past their start time, no older than floor, and
// belonging to a reminder that is active, unarchived, unpaused, and notified at
// its time.
//
// Above zero means the scheduler has stalled, which is the one failure in this
// system worth alerting on — everything else degrades to a plainer reminder,
// and this degrades to no reminder.
//
// floor is the scheduler's own claim floor, passed in rather than computed here
// so the gauge and the claim are driven by one value instead of two constants
// that can drift. Without it the count is every pending row that ever aged out,
// which latches above zero forever and turns the design's single alert into
// noise. What that loses — "these rows are past saving and nobody will fire
// them" — is P3's input and does not belong in this number.
//
// A global pause zeroes it. During vacation the scheduler is deliberately idle,
// and a gauge that climbs while the system is behaving correctly is worse than
// no gauge.
func (s *Store) PendingOverdue(ctx context.Context, floor time.Time) (int, error) {
	now := time.Now()

	pausedUntil, paused, err := s.GlobalPauseUntil(ctx)
	if err != nil {
		return 0, err
	}
	if paused && pausedUntil.After(now) {
		return 0, nil
	}

	nowText := domain.FormatTime(now)
	n, err := s.read.CountPendingOverdue(ctx, sqlc.CountPendingOverdueParams{
		StartsAt:    domain.FormatTime(now.Add(-OverdueGrace)),
		StartsAt_2:  domain.FormatTime(floor),
		PausedUntil: &nowText,
	})
	if err != nil {
		return 0, fmt.Errorf("store: count pending overdue: %w", err)
	}
	return int(n), nil
}

// Due is the fire path's projection: one claimable occurrence and the few item
// columns a send needs.
//
// Narrower than an (Occurrence, Item) pair on purpose. The scheduler reads a
// body, a priority, and a kind; a join that handed back two whole rows would
// invite it to read more, and the one thing this package owes the firing path is
// that it stays small enough to reason about.
type Due struct {
	ID     string
	ItemID string

	StartsAt time.Time

	MessageText *string
	Title       string

	Kind     domain.Kind
	Priority int
}

// ClaimDue marks every due occurrence notified and returns what it claimed.
//
// One transaction, which is already BEGIN IMMEDIATE because the writer DSN
// carries _txlock=immediate — do not reach for sql.Conn and an explicit BEGIN.
// The candidate read happens through the transaction's own handle rather than
// through the read pool: reading a WAL snapshot taken outside the write lock and
// then updating inside it would leave correctness resting on the row guard
// alone, which is not what that DSN parameter was for.
//
// upper is the due boundary, normally now. floor is the oldest start time still
// worth firing; rows older than it are left pending and silent for
// reconciliation to find (C9, Q-2). Nothing here ever assigns missed (K6).
//
// The returned instant is the notified_at written to every claimed row, and is
// what ReleaseClaims guards on.
func (s *Store) ClaimDue(ctx context.Context, upper, floor time.Time, limit int) (time.Time, []Due, error) {
	claimedAt := time.Now().Truncate(time.Second)
	claimedAtText := domain.FormatTime(claimedAt)

	upperText := domain.FormatTime(upper)
	floorText := domain.FormatTime(floor)

	var claimed []Due
	err := s.tx(ctx, func(q *sqlc.Queries) error {
		rows, err := q.ListDueOccurrences(ctx, sqlc.ListDueOccurrencesParams{
			StartsAt:    upperText, // starts_at <= upper
			StartsAt_2:  floorText, // starts_at >= floor
			PausedUntil: &upperText,
			Limit:       int64(limit),
		})
		if err != nil {
			return fmt.Errorf("store: list due occurrences: %w", err)
		}

		claimed = make([]Due, 0, len(rows))
		for _, row := range rows {
			startsAt, err := domain.ParseTime(row.StartsAt)
			if err != nil {
				return fmt.Errorf("store: list due occurrences: %w", err)
			}
			kind := domain.Kind(row.Kind)

			// The state machine decides, not the query. The SQL filters kind as
			// well, so a row that reaches here and is refused is a schema that
			// has moved rather than an ordinary case — worth a line.
			if _, err := domain.Transition(kind, domain.StatusPending, domain.StatusNotified); err != nil {
				s.log.Warn("claim: refused transition", "occurrence", row.ID, "kind", kind, "err", err)
				continue
			}

			n, err := q.ClaimOccurrence(ctx, sqlc.ClaimOccurrenceParams{
				NotifiedAt: &claimedAtText,
				ID:         row.ID,
			})
			if err != nil {
				return fmt.Errorf("store: claim occurrence: %w", err)
			}
			if n == 0 {
				// The guard refused it: the row is no longer pending. Inside the
				// write transaction on the only writer that cannot happen, so
				// this is defence against a future caller rather than a case
				// with a story, and it is a skip and not an error.
				continue
			}

			claimed = append(claimed, Due{
				ID:          row.ID,
				ItemID:      row.ItemID,
				StartsAt:    startsAt,
				MessageText: row.MessageText,
				Title:       row.Title,
				Kind:        kind,
				Priority:    int(row.Priority),
			})
		}
		return nil
	})
	if err != nil {
		return time.Time{}, nil, err
	}
	return claimedAt, claimed, nil
}

// ReleaseClaims returns claimed occurrences to pending, and reports how many it
// actually moved.
//
// This is not a state machine transition — notified to pending is not an edge in
// the table and must never be routed through domain.Transition. It is the
// rollback of a claim whose send never happened, which is why the guard names
// both the status and the exact notified_at this claim wrote: a row anything
// else has touched since is left alone.
//
// One transaction for the whole batch rather than one each. Every transaction is
// a BEGIN IMMEDIATE on a pool of exactly one writer connection, and a shutdown
// releasing forty claims serially would race the drain timeout and strand the
// remainder as notified with nothing ever sent.
//
// Callers pass a context that is not the one that failed — a release triggered
// by cancellation cannot run on the cancelled context.
func (s *Store) ReleaseClaims(ctx context.Context, ids []string, claimedAt time.Time) (int, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	claimedAtText := domain.FormatTime(claimedAt)

	released := 0
	err := s.tx(ctx, func(q *sqlc.Queries) error {
		released = 0
		for _, id := range ids {
			n, err := q.ReleaseClaimedOccurrence(ctx, sqlc.ReleaseClaimedOccurrenceParams{
				ID:         id,
				NotifiedAt: &claimedAtText,
			})
			if err != nil {
				return fmt.Errorf("store: release claimed occurrence: %w", err)
			}
			released += int(n)
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return released, nil
}

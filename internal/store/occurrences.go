package store

import (
	"context"
	"errors"
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

// LiveOccurrence returns an occurrence that belongs to itemID and is still
// an editable pending row, or a *domain.ValidationError naming why it is
// not - update_item's scope=single Layer 2 check.
func (s *Store) LiveOccurrence(ctx context.Context, occurrenceID, itemID string) (domain.Occurrence, error) {
	occ, err := s.GetOccurrence(ctx, occurrenceID)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return domain.Occurrence{}, domain.Invalid("occurrence_exists", "occurrence_id",
				"occurrence %q does not exist", occurrenceID)
		}
		return domain.Occurrence{}, fmt.Errorf("store: live occurrence: %w", err)
	}
	if occ.ItemID != itemID {
		return domain.Occurrence{}, domain.Invalid("occurrence_exists", "occurrence_id",
			"occurrence %q does not belong to item %q", occurrenceID, itemID)
	}
	if occ.Status != domain.StatusPending || occ.IsOverride {
		return domain.Occurrence{}, domain.Invalid("occurrence_editable", "occurrence_id",
			"occurrence %q is not an editable pending occurrence", occurrenceID)
	}
	return occ, nil
}

// UpdateOccurrenceOverride retimes exactly one of an item's pending, future,
// non-override occurrences and marks it is_override, so the next
// materialization run leaves it alone. This is update_item's scope=single
// mechanism (docs/05-schedule-spec.md#edit-scope): "modify ... exactly one
// occurrence, set is_override = 1." It bypasses materializeTx entirely,
// since is_override rows are exempt from the plan/delete cycle by design -
// the guard is the same shape as DeleteFuturePendingOccurrence's.
func (s *Store) UpdateOccurrenceOverride(ctx context.Context, occurrenceID, itemID string, startsAt, now time.Time) (domain.Occurrence, error) {
	var occ domain.Occurrence
	err := s.tx(ctx, func(q *sqlc.Queries) error {
		row, err := q.OverrideFuturePendingOccurrence(ctx, sqlc.OverrideFuturePendingOccurrenceParams{
			StartsAt:   domain.FormatTime(startsAt),
			ID:         occurrenceID,
			ItemID:     itemID,
			StartsAt_2: domain.FormatTime(now),
		})
		if err != nil {
			return notFound("store: override occurrence", err)
		}
		occ, err = toDomainOccurrence(row)
		return err
	})
	if err != nil {
		return domain.Occurrence{}, err
	}
	return occ, nil
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

// TodayOccurrence is one row of the agent's "today's occurrences" context
// block (docs/06-agent-spec.md#context-injection) - narrow, like Due, because
// the prompt renderer needs an id, a title, a start time and a resolution and
// nothing else about the row.
type TodayOccurrence struct {
	ID        string
	ItemID    string
	ItemTitle string

	StartsAt time.Time
	Status   domain.Status

	ResolvedAt       *time.Time
	ResolutionSource *domain.ResolutionSource
}

// TodaysOccurrences returns every occurrence starting inside loc's current
// local calendar day, across every unarchived item, oldest first. loc is the
// same device timezone the rest of the injected context resolves
// (Ladder.deviceZone), so "today" here and "today" in "Current time" above it
// in the prompt never disagree.
func (s *Store) TodaysOccurrences(ctx context.Context, loc *time.Location) ([]TodayOccurrence, error) {
	now := time.Now().In(loc)
	start := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc)
	end := start.AddDate(0, 0, 1)

	rows, err := s.read.ListOccurrencesInRange(ctx, sqlc.ListOccurrencesInRangeParams{
		StartsAt:   domain.FormatTime(start),
		StartsAt_2: domain.FormatTime(end),
	})
	if err != nil {
		return nil, fmt.Errorf("store: list today's occurrences: %w", err)
	}

	out := make([]TodayOccurrence, 0, len(rows))
	for _, row := range rows {
		startsAt, err := domain.ParseTime(row.StartsAt)
		if err != nil {
			return nil, fmt.Errorf("store: list today's occurrences: %w", err)
		}
		resolvedAt, err := parseTimePtr(row.ResolvedAt)
		if err != nil {
			return nil, fmt.Errorf("store: list today's occurrences: %w", err)
		}
		var source *domain.ResolutionSource
		if row.ResolutionSource != nil {
			src := domain.ResolutionSource(*row.ResolutionSource)
			source = &src
		}
		out = append(out, TodayOccurrence{
			ID:               row.ID,
			ItemID:           row.ItemID,
			ItemTitle:        row.Title,
			StartsAt:         startsAt,
			Status:           domain.Status(row.Status),
			ResolvedAt:       resolvedAt,
			ResolutionSource: source,
		})
	}
	return out, nil
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

// Resolution is what one call to ResolveOccurrence decided: the outcome the
// state machine returned, the occurrence as it now stands, and the status it
// held on the way in.
//
// Occurrence is the row after the write when Applied, and the row exactly as
// it was when Noop — which is what makes "200 with the current state" and
// "200 with the new state" the same line of handler code rather than two.
// Previous is carried because navi_occurrence_transitions_total needs both
// ends of the edge, and only the caller that asked for the change knows it
// was an edge at all.
type Resolution struct {
	Occurrence domain.Occurrence
	Outcome    domain.Outcome
	Previous   domain.Status
}

// ResolveOccurrence resolves one occurrence through the shared state machine
// (D-014, invariant 4): read the row and its item's kind, ask
// domain.Transition, and write the four resolution columns only when the
// answer is OutcomeApplied.
//
// Every question a resolution surface can ask is answered here, so no caller
// re-decides any of them. A legal edge applies and returns Applied; the same
// terminal state again returns Noop with nothing written; a different terminal
// state and an illegal edge both return a *domain.TransitionError carrying the
// current state. That is the whole of docs/07-api-spec.md#idempotency, and it
// is a property of the state machine rather than of any surface — which is why
// there are no idempotency keys anywhere near this path.
//
// Nothing here restricts which source may request which status. missed is
// reachable from this method exactly as the table allows, and the reason it is
// not guarded is D-014's: a second rule beside the transition table is the
// divergence the single endpoint was bought to avoid. What keeps missed honest
// is that nothing sends it — see the handler in internal/httpapi/resolve.go.
//
// Early resolution needs no cancel path (R3). Both ListDueOccurrences and
// ClaimOccurrence filter status = 'pending', so a row this method moves to a
// terminal status has already left the scheduler's reach; and because both go
// through the one BEGIN IMMEDIATE writer there is no race, only an order. If
// the claim commits first the row is notified and this still succeeds, because
// notified is a legal starting point for the same edges.
func (s *Store) ResolveOccurrence(
	ctx context.Context,
	id string,
	to domain.Status,
	note *string,
	source domain.ResolutionSource,
	now time.Time,
) (Resolution, error) {
	var res Resolution

	err := s.tx(ctx, func(q *sqlc.Queries) error {
		row, err := q.GetOccurrence(ctx, id)
		if err != nil {
			return notFound("store: resolve occurrence", err)
		}
		occ, err := toDomainOccurrence(row)
		if err != nil {
			return err
		}
		res.Occurrence = occ
		res.Previous = occ.Status

		// The kind lives on the item, not the occurrence, and the valid status
		// set depends on it — the same in-transaction read CreateOccurrence
		// makes for the same reason.
		itemRow, err := q.GetItem(ctx, occ.ItemID)
		if err != nil {
			return notFound("store: resolve occurrence: item", err)
		}
		item, err := toDomainItem(itemRow)
		if err != nil {
			return err
		}

		outcome, err := domain.Transition(item.Kind, occ.Status, to)
		res.Outcome = outcome
		if err != nil {
			// A *domain.TransitionError, returned as it is. res.Occurrence
			// already holds the current row, which is what the 409 reports.
			return err
		}
		if outcome != domain.OutcomeApplied {
			return nil
		}

		resolvedAt := domain.FormatTime(now)
		sourceText := string(source)
		n, err := q.ResolveOccurrence(ctx, sqlc.ResolveOccurrenceParams{
			Status:           string(to),
			ResolvedAt:       &resolvedAt,
			ResolutionNote:   note,
			ResolutionSource: &sourceText,
			ID:               id,
			Status_2:         string(res.Previous),
		})
		if err != nil {
			return fmt.Errorf("store: resolve occurrence: %w", err)
		}
		if n == 0 {
			// The guard refused it. Nothing else can have moved the row — this
			// is inside the write transaction on the only writer — so this is a
			// bug surfacing rather than a case with a story, and it is worth an
			// error instead of a silent no-op that would report 200.
			s.log.Warn("resolve: refused update", "occurrence", id,
				"from", res.Previous, "to", to)
			return fmt.Errorf("store: resolve occurrence %s: guard refused the update", id)
		}

		// Mirror what the statement wrote, so the caller can render the new
		// state without a second read.
		resolved := now
		res.Occurrence.Status = to
		res.Occurrence.ResolvedAt = &resolved
		res.Occurrence.ResolutionNote = note
		res.Occurrence.ResolutionSource = &source
		return nil
	})
	if err != nil {
		var te *domain.TransitionError
		if errors.As(err, &te) {
			// Not a failure: the state machine answered, and the answer plus
			// the current row are what the caller reports.
			return res, err
		}
		return Resolution{}, err
	}
	return res, nil
}

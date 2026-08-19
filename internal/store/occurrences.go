package store

import (
	"context"
	"database/sql"
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

	// Chain is the snooze chain this occurrence belongs to, rolled up after
	// the write and inside the same transaction. It is the side effect
	// docs/07-api-spec.md lists as "rolls the snooze chain up", and it is a
	// read: completing a link never writes to its ancestors, which are snoozed
	// and therefore terminal, and history is immutable (invariant 2). The view
	// already is D-011 — any completed link completes the chain — so reading it
	// is the whole roll-up.
	//
	// It is filled on Applied and on Noop, and is the zero value on a rejected
	// transition, where the transaction rolled back and there is nothing to
	// report but the state machine's answer.
	Chain Chain
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

	err := s.txConn(ctx, func(q *sqlc.Queries, tx *sql.Tx) error {
		var err error
		res, err = s.resolveOneTx(ctx, q, tx, id, to, note, source, now)
		return err
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

// resolveOneTx is one occurrence's resolution inside a transaction somebody else
// opened.
//
// It is factored out of ResolveOccurrence rather than inlined there because
// BulkResolve is the second caller and an atomic batch is one txConn calling
// this per row — not a second copy of the read, the transition, and the
// roll-up. The partial Resolution it returns alongside a *domain.TransitionError
// is deliberate: res.Occurrence already holds the current row, which is what a
// 409 reports.
func (s *Store) resolveOneTx(
	ctx context.Context,
	q *sqlc.Queries,
	tx *sql.Tx,
	id string,
	to domain.Status,
	note *string,
	source domain.ResolutionSource,
	now time.Time,
) (Resolution, error) {
	var res Resolution

	row, err := q.GetOccurrence(ctx, id)
	if err != nil {
		return res, notFound("store: resolve occurrence", err)
	}
	occ, err := toDomainOccurrence(row)
	if err != nil {
		return res, err
	}
	res.Occurrence = occ
	res.Previous = occ.Status

	// The kind lives on the item, not the occurrence, and the valid status
	// set depends on it — the same in-transaction read CreateOccurrence
	// makes for the same reason.
	itemRow, err := q.GetItem(ctx, occ.ItemID)
	if err != nil {
		return res, notFound("store: resolve occurrence: item", err)
	}
	item, err := toDomainItem(itemRow)
	if err != nil {
		return res, err
	}

	outcome, err := domain.Transition(item.Kind, occ.Status, to)
	res.Outcome = outcome
	if err != nil {
		// A *domain.TransitionError, returned as it is.
		return res, err
	}
	if outcome != domain.OutcomeApplied {
		// Nothing was written, but the chain is still what the caller
		// asked about, and a double tap should report the same numbers the
		// first tap did rather than none.
		res.Chain, err = chainFrom(ctx, tx, id)
		return res, err
	}

	if err := s.writeResolutionTx(ctx, q, id, res.Previous, to, note, source, now); err != nil {
		return res, err
	}

	// Mirror what the statement wrote, so the caller can render the new
	// state without a second read.
	resolved := now
	res.Occurrence.Status = to
	res.Occurrence.ResolvedAt = &resolved
	res.Occurrence.ResolutionNote = note
	res.Occurrence.ResolutionSource = &source

	// Last, so the roll-up reflects the write above it. This is what makes
	// "completing the child completes the chain" a number the caller can
	// report rather than a property it has to infer.
	res.Chain, err = chainFrom(ctx, tx, id)
	return res, err
}

// BulkResolution is one row of a batch: which occurrence, which terminal status,
// and why, if the user said.
type BulkResolution struct {
	OccurrenceID string
	Status       domain.Status
	Note         *string
}

// BulkResolve resolves a batch in one transaction, all or nothing.
//
// It is what "stretching and vitamins yes, skipped the walk" needs: six
// sequential single resolutions are six chances to fail and a partial
// application when the fourth is wrong, which is the argument
// docs/06-agent-spec.md makes for one tool taking a list. So this is one
// txConn calling resolveOneTx per row — not a third copy of the transition
// rules, and not a second guarded statement beside writeResolutionTx.
//
// A no-op row is not an error. Asking to complete something already completed
// is the idempotency table's second row wherever it appears, so a batch mixing
// one already-resolved occurrence with one pending occurrence applies the
// pending one and reports both. A rejected transition or an id that does not
// exist does abort the whole batch, and the error names which row it was — per
// docs/07-api-spec.md, "if any occurrence id is invalid, nothing is written and
// the response identifies which one".
func (s *Store) BulkResolve(
	ctx context.Context,
	rows []BulkResolution,
	source domain.ResolutionSource,
	now time.Time,
) ([]Resolution, error) {
	if len(rows) == 0 {
		return nil, nil
	}

	results := make([]Resolution, 0, len(rows))
	err := s.txConn(ctx, func(q *sqlc.Queries, tx *sql.Tx) error {
		results = results[:0]
		for _, row := range rows {
			res, err := s.resolveOneTx(ctx, q, tx, row.OccurrenceID, row.Status, row.Note, source, now)
			if err != nil {
				// Wrapped, not replaced: a caller still reaches the
				// *domain.TransitionError or store.ErrNotFound underneath with
				// errors.As and errors.Is, and now knows which row carried it.
				return fmt.Errorf("store: bulk resolve: occurrence %s: %w", row.OccurrenceID, err)
			}
			results = append(results, res)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return results, nil
}

// Snooze is what one call to SnoozeOccurrence decided.
//
// It is a sibling of Resolution rather than a reuse of it, because a snooze
// touches two rows and a resolution touches one — and a caller has to report
// both, since R6's whole point is that the original keeps its timestamp while
// the child carries the new one.
type Snooze struct {
	// Parent is the occurrence that was snoozed, after the write: snoozed on
	// Applied, missed when the cap was reached, untouched on a no-op. Its
	// StartsAt is never modified, by any path (R6, D-010).
	Parent domain.Occurrence

	// Child is the live link the chain continues in. On Applied it is the row
	// just written; on a no-op it is the one the first snooze wrote, read back
	// rather than duplicated. It is the zero value only when the cap was
	// reached, since that path writes no child at all.
	Child domain.Occurrence

	Outcome  domain.Outcome
	Previous domain.Status

	// CapReached says the chain had already been snoozed as many times as the
	// item allows, so it resolved as missed instead (R8).
	//
	// It is a field rather than an error because nothing failed: the cap is a
	// precondition domain.CheckSnoozeCap answered, and the missed write that
	// follows it is a real committed transition the caller has to count and
	// report. An error return would mean discarding a write that happened.
	CapReached bool

	Chain Chain
}

// SnoozeOccurrence snoozes one occurrence: the original is marked snoozed and
// keeps its true timestamp, and a child is written at the resolved time with an
// incremented depth and the override flag (R6, D-010).
//
// at resolves the child's start time. It is a callback, and pure, for the same
// reason MaterializeItem's plan is: the answer depends on the item's schedule
// and zone, which is internal/schedule's business rather than this package's,
// and it has to be computed against the row as it stands inside the transaction
// rather than against one read before it opened. The caller resolves its zones
// once, up front, and hands down a closure that touches nothing.
//
// The four answers, all of them the state machine's:
//
//   - notified -> snoozed under the cap: OutcomeApplied, both rows written.
//   - snoozed -> snoozed: OutcomeNoop, nothing written, and the existing child
//     read back, so a double-tapped button reports what the first tap produced
//     instead of minting a second child.
//   - at the cap: the same row goes notified -> missed instead, CapReached set.
//     This is the only path in this repository that assigns missed, and it is
//     the one 04-data-model's chain rule blesses by name — "a chain is missed if
//     the terminal link is missed, including snooze-cap exhaustion". Everything
//     else waits for P3's reconciler (D-008, K6).
//   - anything else: a *domain.TransitionError carrying the current state.
//     pending -> snoozed lands here, because the table has no such edge: a
//     reminder that has not fired has nothing to be pushed back from.
func (s *Store) SnoozeOccurrence(
	ctx context.Context,
	id string,
	source domain.ResolutionSource,
	now time.Time,
	at func(item domain.Item, occ domain.Occurrence) (time.Time, error),
) (Snooze, error) {
	var res Snooze

	err := s.txConn(ctx, func(q *sqlc.Queries, tx *sql.Tx) error {
		row, err := q.GetOccurrence(ctx, id)
		if err != nil {
			return notFound("store: snooze occurrence", err)
		}
		occ, err := toDomainOccurrence(row)
		if err != nil {
			return err
		}
		res.Parent = occ
		res.Previous = occ.Status

		// The kind decides the valid status set and snooze_cap is the item's,
		// so both come from the one in-transaction read.
		itemRow, err := q.GetItem(ctx, occ.ItemID)
		if err != nil {
			return notFound("store: snooze occurrence: item", err)
		}
		item, err := toDomainItem(itemRow)
		if err != nil {
			return err
		}

		outcome, err := domain.Transition(item.Kind, occ.Status, domain.StatusSnoozed)
		res.Outcome = outcome
		if err != nil {
			return err
		}
		if outcome != domain.OutcomeApplied {
			childRow, err := q.ChildOccurrence(ctx, &occ.ID)
			if err != nil {
				return notFound("store: snooze occurrence: child", err)
			}
			if res.Child, err = toDomainOccurrence(childRow); err != nil {
				return err
			}
			res.Chain, err = chainFrom(ctx, tx, id)
			return err
		}

		if capErr := domain.CheckSnoozeCap(occ.SnoozeDepth, item.SnoozeCap); capErr != nil {
			return s.missChainTx(ctx, q, tx, &res, item, id, source, now, capErr)
		}

		startsAt, err := at(item, occ)
		if err != nil {
			return err
		}

		// The parent's write is the resolution statement, unchanged. starts_at
		// is not in its SET list and never has been, which is what makes "does
		// not mutate the original timestamp" a property of the SQL rather than
		// of this function remembering to leave it alone.
		//
		// resolution_note stays null: a snooze is not a resolution anyone
		// attaches a reason to, and the endpoint's body carries no note field.
		if err := s.writeResolutionTx(ctx, q, id, occ.Status, domain.StatusSnoozed, nil, source, now); err != nil {
			return err
		}
		resolved := now
		res.Parent.Status = domain.StatusSnoozed
		res.Parent.ResolvedAt = &resolved
		res.Parent.ResolutionSource = &source

		depth := occ.SnoozeDepth + 1
		child, err := insertOccurrence(ctx, q, domain.NewOccurrence{
			ItemID:   occ.ItemID,
			StartsAt: startsAt,

			// An override, so the next materialization run leaves it alone.
			// NewOccurrence.Validate refuses a child that is not one, so this
			// cannot be dropped silently.
			IsOverride:         true,
			ParentOccurrenceID: &occ.ID,
			SnoozeDepth:        &depth,
		}, item.Kind)
		if err != nil {
			return err
		}
		res.Child = child

		res.Chain, err = chainFrom(ctx, tx, id)
		return err
	})
	if err != nil {
		var te *domain.TransitionError
		if errors.As(err, &te) {
			// The state machine answered; res.Parent holds the row whose status
			// the 409 reports.
			return res, err
		}
		return Snooze{}, err
	}
	return res, nil
}

// missChainTx is the snooze-cap branch: the chain has run out of snoozes, so
// the same row resolves as missed rather than acquiring another child (R8).
//
// It goes through domain.Transition like everything else. The cap is a
// precondition on an edge and not an edge of its own, so notified -> missed has
// to be asked for in the ordinary way — a direct write here would be the second
// copy of the transition table that D-014 bought a single endpoint to avoid.
func (s *Store) missChainTx(
	ctx context.Context,
	q *sqlc.Queries,
	tx *sql.Tx,
	res *Snooze,
	item domain.Item,
	id string,
	source domain.ResolutionSource,
	now time.Time,
	capErr error,
) error {
	outcome, err := domain.Transition(item.Kind, res.Previous, domain.StatusMissed)
	res.Outcome = outcome
	if err != nil {
		return err
	}
	if outcome != domain.OutcomeApplied {
		// Unreachable from a row that has just answered Applied for snoozed,
		// since both edges leave notified. Worth refusing rather than assuming.
		return fmt.Errorf("store: snooze occurrence %s: cap reached but %s -> missed is %s",
			id, res.Previous, outcome)
	}

	// The cap message names the depth and the limit, and it is what the 409's
	// message field carries — the same contract every other user-facing message
	// in this codebase keeps. It is stored as the resolution_note too, so the
	// row says why it was missed rather than leaving it to be inferred from a
	// snooze_depth that happens to equal the cap.
	note := capErr.Error()
	if err := s.writeResolutionTx(ctx, q, id, res.Previous, domain.StatusMissed, &note, source, now); err != nil {
		return err
	}

	resolved := now
	res.CapReached = true
	res.Parent.Status = domain.StatusMissed
	res.Parent.ResolvedAt = &resolved
	res.Parent.ResolutionNote = &note
	res.Parent.ResolutionSource = &source

	res.Chain, err = chainFrom(ctx, tx, id)
	return err
}

// writeResolutionTx runs the guarded resolution statement, which is the one
// write behind every terminal status this service assigns. It is factored out
// of ResolveOccurrence's body so that snooze reaches the same statement rather
// than a second one that could drift from it.
func (s *Store) writeResolutionTx(
	ctx context.Context,
	q *sqlc.Queries,
	id string,
	from, to domain.Status,
	note *string,
	source domain.ResolutionSource,
	now time.Time,
) error {
	resolvedAt := domain.FormatTime(now)
	sourceText := string(source)

	n, err := q.ResolveOccurrence(ctx, sqlc.ResolveOccurrenceParams{
		Status:           string(to),
		ResolvedAt:       &resolvedAt,
		ResolutionNote:   note,
		ResolutionSource: &sourceText,
		ID:               id,
		Status_2:         string(from),
	})
	if err != nil {
		return fmt.Errorf("store: resolve occurrence: %w", err)
	}
	if n == 0 {
		// The guard refused it. Nothing else can have moved the row — this is
		// inside the write transaction on the only writer — so this is a bug
		// surfacing rather than a case with a story, and it is worth an error
		// instead of a silent no-op that would report 200.
		s.log.Warn("resolve: refused update", "occurrence", id, "from", from, "to", to)
		return fmt.Errorf("store: resolve occurrence %s: guard refused the update", id)
	}
	return nil
}

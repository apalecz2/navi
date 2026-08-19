package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/aidenpaleczny/navi/internal/domain"
)

// The chains view is the one object in this schema sqlc cannot see. Its SQLite
// analyzer cannot resolve a recursive CTE's column list and rejects
// 0002_chains_view.sql outright, so sqlc.yaml omits that file from the schema
// list and says what to do instead: the query is hand-written with database/sql,
// inside this package, which keeps "all SQL lives in the repository module"
// true.
//
// This is the first read of that view. It has existed unexercised since P0
// because nothing created a chain until snooze did.

// Chain is one snooze chain rolled up: the root occurrence plus every
// descendant, counted once (R7, D-011).
//
// An occurrence that was never snoozed is a chain of one, with SnoozeCount 0.
// That is deliberate and not a degenerate case — treating it separately would
// mean a second definition of "chain" living beside the view's.
type Chain struct {
	// RootID is the occurrence the chain started from: the one the schedule
	// materialized, before any snooze moved the live link along.
	RootID string
	ItemID string

	// ScheduledAt is the root's starts_at — the time the item was originally
	// due, which snoozing never rewrites (D-010). This is the field the
	// copywriter and the proposal logic read to notice that something due at
	// 09:00 got pushed three times.
	ScheduledAt time.Time

	// SnoozeCount is the deepest link in the chain, so three snoozes reads as 3
	// however the rows are walked.
	SnoozeCount int

	// WasCompleted is D-011 itself: any completed link completes the chain, so a
	// streak survives an honest snooze. CompletedAt is the earliest such link's
	// resolved_at, and is nil when nothing in the chain completed.
	WasCompleted bool
	CompletedAt  *time.Time

	// NotifiedAt is the root's, so the lag from "asked" to "done" is measured
	// from the first ask rather than from the last snooze.
	NotifiedAt *time.Time
}

// chainQuery walks up from any member of a chain to its root, then reads that
// root's row from the view.
//
// Walking up rather than down is what lets a caller holding a child id get the
// same answer as one holding the root: the resolution endpoint is handed
// whichever link the user actually resolved, and which link that is has nothing
// to do with which row the chain is keyed on. The recursion terminates at the
// row whose parent_occurrence_id is null, which is the same predicate the view's
// own base case uses.
const chainQuery = `
WITH RECURSIVE up(id, parent_occurrence_id) AS (
  SELECT id, parent_occurrence_id FROM occurrences WHERE id = ?
  UNION ALL
  SELECT o.id, o.parent_occurrence_id
    FROM occurrences o JOIN up u ON o.id = u.parent_occurrence_id
)
SELECT c.root_id, c.item_id, c.scheduled_at, c.snooze_count,
       c.was_completed, c.completed_at, c.notified_at
FROM chains c
WHERE c.root_id = (SELECT id FROM up WHERE parent_occurrence_id IS NULL)`

// querier is the shared surface of *sql.DB and *sql.Tx that chainFrom needs, so
// the in-transaction roll-up and the standalone read are one statement rather
// than two copies of it.
type querier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// ChainFor rolls up the chain any occurrence belongs to, from the read pool.
//
// This is the caller for a surface that is not itself writing — a statistics
// query, or naviseed checking the view answers correctly. A resolution or a
// snooze uses chainFrom on its own transaction instead, so the number it reports
// is the one its own write produced.
func (s *Store) ChainFor(ctx context.Context, occurrenceID string) (Chain, error) {
	return chainFrom(ctx, s.reader, occurrenceID)
}

// chainFrom runs the roll-up against whichever handle it is given.
func chainFrom(ctx context.Context, q querier, occurrenceID string) (Chain, error) {
	var (
		c            Chain
		scheduledAt  string
		completedAt  *string
		notifiedAt   *string
		wasCompleted int64
		snoozeCount  int64
	)

	err := q.QueryRowContext(ctx, chainQuery, occurrenceID).Scan(
		&c.RootID, &c.ItemID, &scheduledAt, &snoozeCount,
		&wasCompleted, &completedAt, &notifiedAt,
	)
	if err != nil {
		return Chain{}, notFound("store: chain for occurrence "+occurrenceID, err)
	}

	c.ScheduledAt, err = domain.ParseTime(scheduledAt)
	if err != nil {
		return Chain{}, fmt.Errorf("store: chain for occurrence %s: %w", occurrenceID, err)
	}
	c.SnoozeCount = int(snoozeCount)
	c.WasCompleted = wasCompleted != 0

	if c.CompletedAt, err = parseTimePtr(completedAt); err != nil {
		return Chain{}, fmt.Errorf("store: chain for occurrence %s: %w", occurrenceID, err)
	}
	if c.NotifiedAt, err = parseTimePtr(notifiedAt); err != nil {
		return Chain{}, fmt.Errorf("store: chain for occurrence %s: %w", occurrenceID, err)
	}
	return c, nil
}

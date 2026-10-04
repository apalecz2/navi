package store

import (
	"context"
	"fmt"
	"time"

	"github.com/aidenpaleczny/navi/internal/domain"
)

// StatsChain is one snooze chain as the statistics read it: the chains view's
// columns plus the item facts a statistic needs to label it. It is a row of the
// view and nothing else - how a chain is classified, ordered or counted is
// internal/stats' business, so that there is exactly one place numbers are
// computed (V6, O4).
type StatsChain struct {
	RootID string
	ItemID string

	// ItemTitle and ItemArchived label the chain. History belongs to an item
	// that has since been archived and still needs a name to show (the same
	// reason the calendar returns ItemTitle for archived items).
	ItemTitle    string
	ItemArchived bool

	// ScheduledAt is the root's starts_at, the time the chain was originally due.
	// It is what a range selects on and what a bucket files a chain under.
	ScheduledAt time.Time

	SnoozeCount int

	// Completed is D-011: any link completed. CompletedAt is the earliest such
	// link's resolved_at.
	Completed   bool
	CompletedAt *time.Time

	// NotifiedAt is the root's, the first ask.
	NotifiedAt *time.Time

	// Reconciled is "a check-in named some link of this chain".
	Reconciled bool

	// Terminal is the status of the chain's last link, and TerminalNote that
	// link's resolution note.
	Terminal     domain.Status
	TerminalNote *string
}

// StatsFilter bounds a statistics read to chains whose root is scheduled in the
// half-open range [From, To), optionally for one item. A zero From means "from
// the beginning".
type StatsFilter struct {
	ItemID string
	From   time.Time
	To     time.Time
}

// Two statements rather than one with an optional item predicate, for the reason
// ListCalendar gives: SQLite will not use an index through `(? = ” OR ...)`.
//
// Both put the range predicate on the view's own columns and rely on the view
// being flattenable, which it is since migration 0007 - its outer FROM is plain
// occurrences, so `scheduled_at` and `item_id` select roots (idx_occ_starts,
// idx_occ_item_starts) before any chain is walked. Before 0007 the same WHERE
// filtered a finished recursive CTE and a week cost what "all" cost.
//
// kind = 'reminder' because "completed" and "missed" mean nothing for an event
// (P7's occurred/cancelled), and an event chain in a completion rate would be
// either a phantom failure or a phantom open row.
const statsChainsSelect = `
SELECT c.root_id, c.item_id, i.title, i.archived_at IS NOT NULL,
       c.scheduled_at, c.snooze_count, c.was_completed, c.completed_at,
       c.notified_at, c.was_reconciled, c.terminal_status, c.terminal_note
FROM chains c JOIN items i ON i.id = c.item_id
WHERE i.kind = 'reminder'
  AND c.scheduled_at >= ? AND c.scheduled_at < ?`

const statsChainsAll = statsChainsSelect + `
ORDER BY c.scheduled_at, c.root_id`

const statsChainsByItem = statsChainsSelect + `
  AND c.item_id = ?
ORDER BY c.scheduled_at, c.root_id`

// StatsChains returns every chain in the filter's range, oldest first. It is the
// only read the statistics make of the chains view, hand-written like
// CountCompletedChains because sqlc cannot see the view (sqlc.yaml).
//
// Archived items' chains are included and marked. History is immutable
// (invariant 2), and deleting an item does not un-happen what it recorded.
func (s *Store) StatsChains(ctx context.Context, f StatsFilter) ([]StatsChain, error) {
	// A zero From is the beginning of time. FormatTime of the zero value is
	// "0001-01-01T00:00:00Z", which sorts before every real timestamp as text.
	lo, hi := domain.FormatTime(f.From), domain.FormatTime(f.To)

	query, args := statsChainsAll, []any{lo, hi}
	if f.ItemID != "" {
		query, args = statsChainsByItem, []any{lo, hi, f.ItemID}
	}

	rows, err := s.reader.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("store: stats chains: %w", err)
	}
	defer rows.Close()

	var out []StatsChain
	for rows.Next() {
		var (
			c                               StatsChain
			scheduledAt                     string
			completedAt, notifiedAt         *string
			terminal                        *string
			archived, completed, reconciled int64
			snoozeCount                     int64
		)
		if err := rows.Scan(&c.RootID, &c.ItemID, &c.ItemTitle, &archived,
			&scheduledAt, &snoozeCount, &completed, &completedAt,
			&notifiedAt, &reconciled, &terminal, &c.TerminalNote); err != nil {
			return nil, fmt.Errorf("store: stats chains: %w", err)
		}
		if c.ScheduledAt, err = domain.ParseTime(scheduledAt); err != nil {
			return nil, fmt.Errorf("store: stats chains: chain %s: %w", c.RootID, err)
		}
		if c.CompletedAt, err = parseTimePtr(completedAt); err != nil {
			return nil, fmt.Errorf("store: stats chains: chain %s: %w", c.RootID, err)
		}
		if c.NotifiedAt, err = parseTimePtr(notifiedAt); err != nil {
			return nil, fmt.Errorf("store: stats chains: chain %s: %w", c.RootID, err)
		}
		c.ItemArchived = archived != 0
		c.SnoozeCount = int(snoozeCount)
		c.Completed = completed != 0
		c.Reconciled = reconciled != 0
		if terminal != nil {
			c.Terminal = domain.Status(*terminal)
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: stats chains: %w", err)
	}
	return out, nil
}

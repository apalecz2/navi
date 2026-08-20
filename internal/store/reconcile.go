package store

import (
	"context"
	"fmt"
	"time"

	"github.com/aidenpaleczny/navi/internal/domain"
	"github.com/aidenpaleczny/navi/internal/store/sqlc"
)

// Unreconciled is one row the daily check-in is about to ask about - narrow,
// like Due and TodayOccurrence beside it, because the template needs a title
// to name and an id to mark and nothing else.
//
// Status is carried because the two values it can hold are the two halves of
// K5: pending means a silent item that never pushed, notified means an at_time
// item that pushed and was ignored. Nothing in this session branches on it,
// and the reconciler's log line is better for having it.
type Unreconciled struct {
	ID        string
	ItemID    string
	ItemTitle string

	StartsAt time.Time
	Status   domain.Status
}

// ListUnreconciled returns everything still outstanding for one local day that
// the check-in has not already asked about, for a pass running at slot (K3,
// K5).
//
// from and to are the local day's midnight and the pass's own instant, which
// the caller resolves in the device zone before calling - an occurrence later
// today is future rather than outstanding, and asking about it would be asking
// about something that has not happened.
//
// globalSlot and slot are both zero-padded local HH:MM. An item with no
// reconcile_at of its own is asked about at globalSlot; one with an override is
// asked about at its own time. The query compares them as text, which is the
// chronological comparison because the format is fixed-width - see
// config.LocalTimeLayout.
func (s *Store) ListUnreconciled(
	ctx context.Context,
	from, to time.Time,
	globalSlot, slot string,
) ([]Unreconciled, error) {
	toText := domain.FormatTime(to)
	rows, err := s.read.ListUnreconciled(ctx, sqlc.ListUnreconciledParams{
		StartsAt:   domain.FormatTime(from),
		StartsAt_2: toText,

		// The pause bound is the pass's own instant, matching what ClaimDue
		// passes: an item paused as of now is absent from this check-in, which
		// is the second half of "pausing is not eighteen skips" (I6).
		PausedUntil: &toText,

		// Column4 is the ifnull default - the global slot standing in for a
		// NULL items.reconcile_at. ReconcileAt is the pass. sqlc names both
		// after the expression it found them in rather than after what they
		// mean, which is exactly why they are wrapped here.
		Column4:     globalSlot,
		ReconcileAt: &slot,
	})
	if err != nil {
		return nil, fmt.Errorf("store: list unreconciled: %w", err)
	}

	out := make([]Unreconciled, 0, len(rows))
	for _, row := range rows {
		startsAt, err := domain.ParseTime(row.StartsAt)
		if err != nil {
			return nil, fmt.Errorf("store: list unreconciled: %w", err)
		}
		out = append(out, Unreconciled{
			ID:        row.ID,
			ItemID:    row.ItemID,
			ItemTitle: row.Title,
			StartsAt:  startsAt,
			Status:    domain.Status(row.Status),
		})
	}
	return out, nil
}

// RecordCheckIn writes everything one reconciliation pass leaves behind:
// reconciled_at on every row it asked about, the conversation row carrying the
// message and its context_ref, and the slot latch that stops the pass running
// twice.
//
// One transaction, because a pass that marked four of six rows and then failed
// is a state nothing can interpret: next session's grace window reads
// reconciled_at to decide what "asked and got nothing" applies to (K6, D-008),
// and two rows silently outside that set would become permanent.
//
// conv is nil when the pass found nothing outstanding. Q-6's leaning is silence
// rather than a daily "all done", so there is no message and no conversation
// row - but the latch still advances, or the pass would be re-evaluated every
// sixty seconds for the rest of the evening.
//
// It writes no status and asks domain.Transition nothing. reconciled_at is not
// a transition: the occurrence is still pending or notified, and what changed is
// that it has now been asked about.
func (s *Store) RecordCheckIn(
	ctx context.Context,
	ids []string,
	slot string,
	conv *domain.NewConversation,
	now time.Time,
) error {
	if conv != nil {
		if err := conv.Validate(); err != nil {
			return err
		}
	}
	nowText := domain.FormatTime(now)

	err := s.tx(ctx, func(q *sqlc.Queries) error {
		for _, id := range ids {
			// The rows-affected count is deliberately ignored. Zero means the
			// row already carried a reconciled_at, which the guard in SQL
			// refuses to overwrite - a benign race with a concurrent pass, not
			// a failure, and the same shape ClaimOccurrence's guard absorbs.
			if _, err := q.MarkReconciled(ctx, sqlc.MarkReconciledParams{
				ReconciledAt: &nowText,
				ID:           id,
			}); err != nil {
				return err
			}
		}

		if conv != nil {
			if _, err := q.CreateConversation(ctx, newConversationParams(*conv, nowText)); err != nil {
				return err
			}
		}

		return q.SetKV(ctx, sqlc.SetKVParams{
			Key:       KeyLastReconcileDate,
			Value:     slot,
			UpdatedAt: nowText,
		})
	})
	if err != nil {
		return fmt.Errorf("store: record check-in: %w", err)
	}
	return nil
}

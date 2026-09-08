package store

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/aidenpaleczny/navi/internal/domain"
	"github.com/aidenpaleczny/navi/internal/store/sqlc"
)

// The morning briefing's three kv slots (O6, O8). None of it is an occurrence:
// a single daily message with no per-item variation is a kv flag, not a reason
// to route a global event through the occurrence table (03-architecture). The
// slots mirror the reconciler's last_reconcile_date latch - a singleton whose
// value carries the date - rather than a family of key:{date} rows.

// BriefingPending returns the briefing composed ahead of send time and the
// local date it was composed for, or ok=false when nothing is staged. The
// stored shape is "{date}\n{text}"; a value missing the separator is treated as
// absent rather than as an error, since the only writer is SetBriefingPending.
func (s *Store) BriefingPending(ctx context.Context) (date, text string, ok bool, err error) {
	raw, present, err := s.getKV(ctx, KeyBriefingPending)
	if err != nil || !present {
		return "", "", false, err
	}
	nl := strings.IndexByte(raw, '\n')
	if nl < 0 {
		return "", "", false, nil
	}
	return raw[:nl], raw[nl+1:], true, nil
}

// SetBriefingPending stages the composed briefing for date. It overwrites any
// previous staging, which is what makes a re-compose before the send harmless.
func (s *Store) SetBriefingPending(ctx context.Context, date, text string) error {
	return s.setKV(ctx, KeyBriefingPending, date+"\n"+text)
}

// LastBriefingDate returns the local date of the last briefing sent and whether
// one has ever been. Opaque to this package, like LastReconcileSlot: comparing
// it to today is the briefing loop's business.
func (s *Store) LastBriefingDate(ctx context.Context) (string, bool, error) {
	return s.getKV(ctx, KeyLastBriefingDate)
}

// BriefingAwaiting returns the date and send instant of a briefing still
// waiting on a reply, or ok=false when none is. The stored shape is
// "{date}\t{instant}" in domain.TimeLayout; a malformed value reports an error
// rather than absence, so a corrupt row stays loud.
func (s *Store) BriefingAwaiting(ctx context.Context) (date string, sentAt time.Time, ok bool, err error) {
	raw, present, err := s.getKV(ctx, KeyBriefingAwaiting)
	if err != nil || !present {
		return "", time.Time{}, false, err
	}
	tab := strings.IndexByte(raw, '\t')
	if tab < 0 {
		return "", time.Time{}, false, fmt.Errorf("store: briefing awaiting: malformed value %q", raw)
	}
	sentAt, err = domain.ParseTime(raw[tab+1:])
	if err != nil {
		return "", time.Time{}, false, fmt.Errorf("store: briefing awaiting: %w", err)
	}
	return raw[:tab], sentAt, true, nil
}

// RecordBriefing writes everything a successful send leaves behind, in one
// transaction: the outbound conversation row with its context_ref, the
// last_briefing_date latch, the awaiting-response marker, and the deletion of
// the now-spent pending slot.
//
// One transaction for the same reason RecordCheckIn is one: a send that
// advanced the latch but failed to record the awaiting marker would be a
// briefing that can never be evaluated as unanswered (O8), and one that recorded
// the marker without advancing the latch would send again on the next tick.
//
// It writes no status and asks domain.Transition nothing - there is no
// occurrence here to transition.
func (s *Store) RecordBriefing(ctx context.Context, date, text, contextRef string, sentAt time.Time) error {
	conv := domain.NewConversation{
		Role:       domain.RoleAssistant,
		Content:    text,
		ContextRef: &contextRef,
	}
	if err := conv.Validate(); err != nil {
		return err
	}
	nowText := domain.FormatTime(sentAt)

	err := s.tx(ctx, func(q *sqlc.Queries) error {
		if _, err := q.CreateConversation(ctx, newConversationParams(conv, nowText)); err != nil {
			return err
		}
		if err := q.SetKV(ctx, sqlc.SetKVParams{
			Key: KeyLastBriefingDate, Value: date, UpdatedAt: nowText,
		}); err != nil {
			return err
		}
		if err := q.SetKV(ctx, sqlc.SetKVParams{
			Key: KeyBriefingAwaiting, Value: date + "\t" + nowText, UpdatedAt: nowText,
		}); err != nil {
			return err
		}
		return q.DeleteKV(ctx, KeyBriefingPending)
	})
	if err != nil {
		return fmt.Errorf("store: record briefing: %w", err)
	}
	return nil
}

// ClearBriefingAwaiting removes the awaiting-response marker. Called by the
// evaluate phase once a reply has been seen or the grace window has closed;
// deleting it is what makes the absent case the only "not awaiting" state, the
// same shape ClearGlobalPause uses.
func (s *Store) ClearBriefingAwaiting(ctx context.Context) error {
	err := s.tx(ctx, func(q *sqlc.Queries) error {
		return q.DeleteKV(ctx, KeyBriefingAwaiting)
	})
	if err != nil {
		return fmt.Errorf("store: clear %s: %w", KeyBriefingAwaiting, err)
	}
	return nil
}

// ClearBriefingState deletes all three briefing slots, so the next tick treats
// today as if no briefing had run. Its production use is an operator forcing a
// fresh briefing after a bad one; cmd/naviseed also uses it to run the section
// from a known state on a reused database.
func (s *Store) ClearBriefingState(ctx context.Context) error {
	err := s.tx(ctx, func(q *sqlc.Queries) error {
		for _, k := range []string{KeyBriefingPending, KeyLastBriefingDate, KeyBriefingAwaiting} {
			if err := q.DeleteKV(ctx, k); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("store: clear briefing state: %w", err)
	}
	return nil
}

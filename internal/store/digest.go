package store

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/aidenpaleczny/navi/internal/domain"
	"github.com/aidenpaleczny/navi/internal/store/sqlc"
)

// The weekly digest's two kv slots (docs/06-agent-spec.md, Proactive behaviour).
// Like the briefing it is one message about the whole week, so it is a kv flag
// and not an occurrence; unlike the briefing nothing evaluates it afterwards, so
// there is no awaiting marker and no grace window. A reply is recognised by the
// clock alone (DigestReplyWindow), which is why the latch and the window share
// one slot.

// ContextRefDigest is the LIKE pattern for a digest's context_ref, beside
// ContextRefReconcile, ContextRefBriefing and ContextRefProposal.
const ContextRefDigest = "digest:%"

// DigestRef is the context_ref a digest's outbound conversations row carries.
func DigestRef(date string) string { return "digest:" + date }

// DigestReplyWindow is how long after a send a message may still be a reply to
// the digest. Sunday evening to Monday evening: long enough for "drop the walk,
// then" over breakfast, short enough that the line is not in next Thursday's
// prompt.
const DigestReplyWindow = 24 * time.Hour

// DigestPending returns the digest composed ahead of send time and the local
// date it was composed for, or ok=false when nothing is staged.
func (s *Store) DigestPending(ctx context.Context) (date, text string, ok bool, err error) {
	raw, present, err := s.getKV(ctx, KeyDigestPending)
	if err != nil || !present {
		return "", "", false, err
	}
	nl := strings.IndexByte(raw, '\n')
	if nl < 0 {
		return "", "", false, nil
	}
	return raw[:nl], raw[nl+1:], true, nil
}

// SetDigestPending stages the composed digest for date, overwriting any earlier
// staging.
func (s *Store) SetDigestPending(ctx context.Context, date, text string) error {
	return s.setKV(ctx, KeyDigestPending, date+"\n"+text)
}

// LastDigestDate is the local date of the last digest pass that concluded, sent
// or deliberately not, and whether there has been one.
func (s *Store) LastDigestDate(ctx context.Context) (string, bool, error) {
	date, _, ok, err := s.lastDigest(ctx)
	return date, ok, err
}

// lastDigest splits KeyLastDigest. sentAt is the zero time for a latched pass
// that sent nothing.
func (s *Store) lastDigest(ctx context.Context) (date string, sentAt time.Time, ok bool, err error) {
	raw, present, err := s.getKV(ctx, KeyLastDigest)
	if err != nil || !present {
		return "", time.Time{}, false, err
	}
	date, instant, found := strings.Cut(raw, "\t")
	if !found {
		return "", time.Time{}, false, fmt.Errorf("store: %s: malformed value %q", KeyLastDigest, raw)
	}
	if instant == "" {
		return date, time.Time{}, true, nil
	}
	sentAt, err = domain.ParseTime(instant)
	if err != nil {
		return "", time.Time{}, false, fmt.Errorf("store: %s: %w", KeyLastDigest, err)
	}
	return date, sentAt, true, nil
}

// OpenDigest returns the context_ref of a digest a message at now could still be
// answering, or ok=false. A pass that sent nothing opens nothing.
func (s *Store) OpenDigest(ctx context.Context, now time.Time) (ref string, ok bool, err error) {
	date, sentAt, present, err := s.lastDigest(ctx)
	if err != nil || !present || sentAt.IsZero() {
		return "", false, err
	}
	if age := now.Sub(sentAt); age < 0 || age >= DigestReplyWindow {
		return "", false, nil
	}
	return DigestRef(date), true, nil
}

// RecordDigest writes everything a delivered digest leaves behind in one
// transaction: the outbound conversations row with its context_ref, the latch
// with the send instant, and the deletion of the spent pending slot. After the
// send and not before, the reconciler's order: a crash between the two costs a
// duplicated digest on the next tick, never a latch for a message nobody got.
//
// It touches no occurrence and no proactive_count: the digest is scheduled, and
// the cap counts only what the proactive loop sends.
func (s *Store) RecordDigest(ctx context.Context, date, text string, sentAt time.Time) error {
	ref := DigestRef(date)
	conv := domain.NewConversation{Role: domain.RoleAssistant, Content: text, ContextRef: &ref}
	if err := conv.Validate(); err != nil {
		return err
	}
	nowText := domain.FormatTime(sentAt)

	err := s.tx(ctx, func(q *sqlc.Queries) error {
		if _, err := q.CreateConversation(ctx, newConversationParams(conv, nowText)); err != nil {
			return err
		}
		if err := q.SetKV(ctx, sqlc.SetKVParams{
			Key: KeyLastDigest, Value: date + "\t" + nowText, UpdatedAt: nowText,
		}); err != nil {
			return err
		}
		return q.DeleteKV(ctx, KeyDigestPending)
	})
	if err != nil {
		return fmt.Errorf("store: record digest: %w", err)
	}
	return nil
}

// SkipDigest latches date without a send, for a week with nothing to say, and
// drops anything staged. No conversations row: nothing was said.
func (s *Store) SkipDigest(ctx context.Context, date string, now time.Time) error {
	err := s.tx(ctx, func(q *sqlc.Queries) error {
		if err := q.SetKV(ctx, sqlc.SetKVParams{
			Key: KeyLastDigest, Value: date + "\t", UpdatedAt: domain.FormatTime(now),
		}); err != nil {
			return err
		}
		return q.DeleteKV(ctx, KeyDigestPending)
	})
	if err != nil {
		return fmt.Errorf("store: skip digest: %w", err)
	}
	return nil
}

// ClearDigestState deletes both digest slots, so the next tick treats the week
// as if no digest had run. cmd/naviseed uses it to start a section from a known
// state on a reused database.
func (s *Store) ClearDigestState(ctx context.Context) error {
	err := s.tx(ctx, func(q *sqlc.Queries) error {
		for _, k := range []string{KeyDigestPending, KeyLastDigest} {
			if err := q.DeleteKV(ctx, k); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("store: clear digest state: %w", err)
	}
	return nil
}

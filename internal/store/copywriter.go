package store

import (
	"context"
	"fmt"
	"time"

	"github.com/aidenpaleczny/navi/internal/domain"
	"github.com/aidenpaleczny/navi/internal/store/sqlc"
)

// CopyCandidate is one pending occurrence the copywriter may generate text for:
// the few columns a decision needs, narrow for the reason Due is.
type CopyCandidate struct {
	ID     string
	ItemID string

	StartsAt time.Time

	// ParentOccurrenceID is set on a snooze child. SnoozeDepth is how deep.
	ParentOccurrenceID *string
	SnoozeDepth        int

	// GenerationPass is the pass the row has reached: 0 has no text, 1 has a
	// safety-net text. GenerationAttempts counts tries toward the next one.
	GenerationPass     int
	GenerationAttempts int

	// MessageBasis is the fingerprint the current text was generated from.
	MessageBasis *string
}

// CopyWindow bounds one copywriter pass over the table. All three are instants.
type CopyWindow struct {
	// From is exclusive: a row starting at or before it is too close to fire for
	// a model call to be worth making.
	From time.Time

	// Pass1Until and Pass2Until are inclusive. A row with no text is a candidate
	// once it starts before Pass1Until; a row with safety-net text is a candidate
	// for a refresh once it starts before Pass2Until.
	Pass1Until time.Time
	Pass2Until time.Time

	// Now is the instant a pause is judged at, matching the scheduler's own read.
	Now time.Time

	// MaxAttempts is the per-pass cap (G4).
	MaxAttempts int
	Limit       int
}

// ListCopyCandidates returns the occurrences the copywriter may work on, soonest
// first. It applies the scheduler's own eligibility - a reminder, at_time,
// active, not archived, not paused - because generating text for a row the fire
// path will never send is a model call with no reader.
func (s *Store) ListCopyCandidates(ctx context.Context, w CopyWindow) ([]CopyCandidate, error) {
	nowText := domain.FormatTime(w.Now)
	rows, err := s.read.ListCopyCandidates(ctx, sqlc.ListCopyCandidatesParams{
		StartsAt:           domain.FormatTime(w.From),
		StartsAt_2:         domain.FormatTime(w.Pass1Until),
		GenerationAttempts: int64(w.MaxAttempts),
		StartsAt_3:         domain.FormatTime(w.Pass2Until),
		PausedUntil:        &nowText,
		Limit:              int64(w.Limit),
	})
	if err != nil {
		return nil, fmt.Errorf("store: list copy candidates: %w", err)
	}

	out := make([]CopyCandidate, 0, len(rows))
	for _, row := range rows {
		startsAt, err := domain.ParseTime(row.StartsAt)
		if err != nil {
			return nil, fmt.Errorf("store: list copy candidates: occurrence %s: %w", row.ID, err)
		}
		out = append(out, CopyCandidate{
			ID:                 row.ID,
			ItemID:             row.ItemID,
			StartsAt:           startsAt,
			ParentOccurrenceID: row.ParentOccurrenceID,
			SnoozeDepth:        int(row.SnoozeDepth),
			GenerationPass:     int(row.GenerationPass),
			GenerationAttempts: int(row.GenerationAttempts),
			MessageBasis:       row.MessageBasis,
		})
	}
	return out, nil
}

// ClaimGenerationAttempt records one attempt against a row before the model is
// called, and reports whether it was granted. It is refused (false, no error)
// when the row has left pending, has moved to another pass, or has used its
// attempts - all of which mean "do not spend a call on this one".
//
// Counting before the call rather than after is what keeps the cap honest across
// a crash or a timeout: an attempt that never returned still happened.
func (s *Store) ClaimGenerationAttempt(ctx context.Context, id string, pass, maxAttempts int) (bool, error) {
	var n int64
	err := s.tx(ctx, func(q *sqlc.Queries) error {
		var err error
		n, err = q.ClaimGenerationAttempt(ctx, sqlc.ClaimGenerationAttemptParams{
			ID:                 id,
			GenerationPass:     int64(pass),
			GenerationAttempts: int64(maxAttempts),
		})
		return err
	})
	if err != nil {
		return false, fmt.Errorf("store: claim generation attempt %s: %w", id, err)
	}
	return n == 1, nil
}

// Generated is what a successful generation writes.
type Generated struct {
	Text  string
	Model string
	At    time.Time

	// Pass is the pass this text belongs to (1 or 2). The write is guarded on the
	// row being at the pass before it, so a stale writer cannot stamp a pass it
	// did not run.
	Pass int

	// Basis is the fingerprint of the context the text was written from.
	Basis string
}

// WriteGeneratedMessage stores a generated message and reports whether it landed.
//
// false with no error is the common loser's outcome, not a fault: the scheduler
// claimed the row first (status is no longer pending), or another writer already
// advanced the pass. The text is discarded and the row is untouched. That guard
// is the whole of what keeps the copywriter from rewriting a notified row, and it
// lives in the statement, so no caller has to remember it.
//
// generation_attempts goes back to zero in the same statement, which is what makes
// the cap per pass rather than per row: pass 2 starts with its own two attempts.
// llm_calls, which carries occurrence_id, is the record of how many it took.
func (s *Store) WriteGeneratedMessage(ctx context.Context, id string, g Generated) (bool, error) {
	if g.Pass != domain.GenerationPassSafety && g.Pass != domain.GenerationPassFresh {
		return false, fmt.Errorf("store: write generated message %s: pass %d is not a generation pass", id, g.Pass)
	}
	at := domain.FormatTime(g.At)
	var n int64
	err := s.tx(ctx, func(q *sqlc.Queries) error {
		var err error
		n, err = q.WriteGeneratedMessage(ctx, sqlc.WriteGeneratedMessageParams{
			MessageText:        &g.Text,
			MessageModel:       &g.Model,
			MessageGeneratedAt: &at,
			GenerationPass:     int64(g.Pass),
			MessageBasis:       &g.Basis,
			ID:                 id,
			GenerationPass_2:   int64(g.Pass - 1),
		})
		return err
	})
	if err != nil {
		return false, fmt.Errorf("store: write generated message %s: %w", id, err)
	}
	return n == 1, nil
}

// RecentMessages returns up to n generated messages for an item, newest first,
// leaving out one occurrence (the one being written for). It is G6's input: the
// last few things this reminder said, so the next one does not say them again.
func (s *Store) RecentMessages(ctx context.Context, itemID, excludeID string, n int) ([]string, error) {
	rows, err := s.read.ListRecentMessages(ctx, sqlc.ListRecentMessagesParams{
		ItemID: itemID, ID: excludeID, Limit: int64(n),
	})
	if err != nil {
		return nil, fmt.Errorf("store: list recent messages for %s: %w", itemID, err)
	}
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		if r != nil {
			out = append(out, *r)
		}
	}
	return out, nil
}

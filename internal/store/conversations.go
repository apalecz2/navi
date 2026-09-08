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

// CreateConversation records one message, deduplicating on (transport,
// external_id) when both are set. inserted reports whether this call wrote a
// new row (false means the exact same update was already recorded — a
// harmless outcome of Telegram redelivering an update it did not get a 2xx
// for) so a caller can decide whether to count it.
//
// The lookup-then-insert happens inside one BEGIN IMMEDIATE transaction on
// the single writer connection, which is what makes it race-free without
// help from idx_conv_dedup: a concurrent retry cannot even begin its own
// write transaction until this one commits. The index is a backstop, not the
// mechanism (see 0003_conversations_dedup.sql).
func (s *Store) CreateConversation(ctx context.Context, n domain.NewConversation) (domain.Conversation, bool, error) {
	if err := n.Validate(); err != nil {
		return domain.Conversation{}, false, err
	}

	var (
		conv     domain.Conversation
		inserted bool
	)
	err := s.tx(ctx, func(q *sqlc.Queries) error {
		if n.Transport != nil && n.ExternalID != nil {
			existing, err := q.GetConversationByTransportExternalID(ctx, sqlc.GetConversationByTransportExternalIDParams{
				Transport:  n.Transport,
				ExternalID: n.ExternalID,
			})
			switch {
			case err == nil:
				c, cerr := toDomainConversation(existing)
				if cerr != nil {
					return cerr
				}
				conv = c
				return nil // a dedup hit: inserted stays false
			case !errors.Is(err, sql.ErrNoRows):
				return fmt.Errorf("store: get conversation by transport/external_id: %w", err)
			}
		}

		row, err := q.CreateConversation(ctx, newConversationParams(n, domain.FormatTime(time.Now())))
		if err != nil {
			return fmt.Errorf("store: create conversation: %w", err)
		}
		c, err := toDomainConversation(row)
		if err != nil {
			return err
		}
		conv, inserted = c, true
		return nil
	})
	if err != nil {
		return domain.Conversation{}, false, err
	}
	return conv, inserted, nil
}

// newConversationParams maps a domain row onto sqlc's insert parameters. It is
// shared with RecordCheckIn, which writes its conversation row inside the same
// transaction as the reconciliation it belongs to and so cannot call
// CreateConversation - two copies of this mapping would be two places for a
// field to be forgotten, and context_ref is exactly the field a second copy
// would forget.
func newConversationParams(n domain.NewConversation, createdAt string) sqlc.CreateConversationParams {
	return sqlc.CreateConversationParams{
		ID:         domain.NewID(),
		Role:       string(n.Role),
		Content:    n.Content,
		ToolCalls:  n.ToolCalls,
		ToolCallID: n.ToolCallID,
		Transport:  n.Transport,
		ExternalID: n.ExternalID,
		ContextRef: n.ContextRef,
		CreatedAt:  createdAt,
	}
}

// GetConversation returns one conversation row, or ErrNotFound.
func (s *Store) GetConversation(ctx context.Context, id string) (domain.Conversation, error) {
	row, err := s.read.GetConversation(ctx, id)
	if err != nil {
		return domain.Conversation{}, notFound("store: get conversation", err)
	}
	return toDomainConversation(row)
}

// GetConversationByExternalID returns the row recorded for a given transport
// update, or ErrNotFound. Exposed publicly, not just used inside
// CreateConversation, so naviseed and a future consumer can ask "did we
// already record this" without writing.
func (s *Store) GetConversationByExternalID(ctx context.Context, transport, externalID string) (domain.Conversation, error) {
	row, err := s.read.GetConversationByTransportExternalID(ctx, sqlc.GetConversationByTransportExternalIDParams{
		Transport:  &transport,
		ExternalID: &externalID,
	})
	if err != nil {
		return domain.Conversation{}, notFound("store: get conversation by external id", err)
	}
	return toDomainConversation(row)
}

// ContextRefReconcile is the LIKE pattern matching a check-in's context_ref.
// The value itself is "reconcile:{date}", written by RecordCheckIn.
const ContextRefReconcile = "reconcile:%"

// ContextRefBriefing is the same for the morning briefing: the value is
// "briefing:{date}", written by RecordBriefing, and the agent's Context ref
// block names it so a reply is recognised as answering the briefing rather than
// starting a new request (O8, 06-agent-spec). Same LIKE shape as
// ContextRefReconcile rather than a second query.
const ContextRefBriefing = "briefing:%"

// LatestContextRef returns the most recent context_ref matching pattern, and
// whether there was one at all.
//
// It answers "which question is on the table", not "is a question still open" —
// see the query's comment. A caller wanting the second reads
// ListAwaitingReconciliation and renders nothing when it is empty.
//
// No rows is not an error: a deployment where the reconciler has never sent a
// check-in is the ordinary first-week state, and it reports ("", false, nil) on
// the same convention CurrentTZ and LastTouchedItemID already use.
func (s *Store) LatestContextRef(ctx context.Context, pattern string) (string, bool, error) {
	ref, err := s.read.LatestContextRef(ctx, &pattern)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", false, nil
		}
		return "", false, fmt.Errorf("store: latest context ref: %w", err)
	}
	if ref == nil || *ref == "" {
		return "", false, nil
	}
	return *ref, true, nil
}

// HasInboundSince reports whether any inbound (role user) message has been
// recorded at or after t. It is the morning briefing's reply detection (O8):
// the briefing sets an awaiting marker on send, and the first inbound message
// after that instant - whatever it says - clears it.
func (s *Store) HasInboundSince(ctx context.Context, t time.Time) (bool, error) {
	got, err := s.read.HasInboundSince(ctx, sqlc.HasInboundSinceParams{
		Role:      string(domain.RoleUser),
		CreatedAt: domain.FormatTime(t),
	})
	if err != nil {
		return false, fmt.Errorf("store: has inbound since: %w", err)
	}
	return got, nil
}

// ListRecentConversations returns the most recent conversation rows, newest
// first, capped at limit — naviseed's way to inspect a turn's full
// transcript, on the same shape ListLLMCalls uses. Nothing in the write or
// fire path reads it.
func (s *Store) ListRecentConversations(ctx context.Context, limit int) ([]domain.Conversation, error) {
	rows, err := s.read.ListRecentConversations(ctx, int64(limit))
	if err != nil {
		return nil, fmt.Errorf("store: list recent conversations: %w", err)
	}
	out := make([]domain.Conversation, 0, len(rows))
	for _, row := range rows {
		c, err := toDomainConversation(row)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, nil
}

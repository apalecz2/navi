package store

import (
	"context"
	"fmt"
	"time"

	"github.com/aidenpaleczny/navi/internal/domain"
	"github.com/aidenpaleczny/navi/internal/store/sqlc"
)

// CreateLLMCall records one model-client attempt, successful or not. L6 asks
// for every call to be logged, which is why internal/model calls this from
// every return path of Complete rather than only on success.
func (s *Store) CreateLLMCall(ctx context.Context, n domain.NewLLMCall) (domain.LLMCall, error) {
	if err := n.Validate(); err != nil {
		return domain.LLMCall{}, err
	}

	var call domain.LLMCall
	err := s.tx(ctx, func(q *sqlc.Queries) error {
		row, err := q.CreateLLMCall(ctx, sqlc.CreateLLMCallParams{
			ID:               domain.NewID(),
			Task:             n.Task,
			Tier:             int64(n.Tier),
			Model:            n.Model,
			PromptTokens:     int64Ptr(n.PromptTokens),
			CompletionTokens: int64Ptr(n.CompletionTokens),
			LatencyMs:        int64Ptr(n.LatencyMS),
			Escalated:        boolToInt(n.Escalated),
			EscalationReason: n.EscalationReason,
			Error:            n.Error,
			OccurrenceID:     n.OccurrenceID,
			CreatedAt:        domain.FormatTime(time.Now()),
		})
		if err != nil {
			return fmt.Errorf("store: create llm call: %w", err)
		}
		c, err := toDomainLLMCall(row)
		if err != nil {
			return err
		}
		call = c
		return nil
	})
	if err != nil {
		return domain.LLMCall{}, err
	}
	return call, nil
}

// ListLLMCalls returns the most recent calls, newest first, capped at limit.
// It exists for naviseed and any future admin view to inspect what the
// client actually did; nothing in the fire or write path reads it.
func (s *Store) ListLLMCalls(ctx context.Context, limit int) ([]domain.LLMCall, error) {
	rows, err := s.read.ListLLMCalls(ctx, int64(limit))
	if err != nil {
		return nil, fmt.Errorf("store: list llm calls: %w", err)
	}
	out := make([]domain.LLMCall, 0, len(rows))
	for _, row := range rows {
		c, err := toDomainLLMCall(row)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, nil
}

// PruneLLMCalls deletes rows older than before, per L7's retention policy (90
// days per 04-data-model.md) enforced by the hourly sweeper. It reports how
// many rows were removed so a caller can log a non-zero prune without a
// second query.
func (s *Store) PruneLLMCalls(ctx context.Context, before time.Time) (int64, error) {
	var n int64
	err := s.tx(ctx, func(q *sqlc.Queries) error {
		deleted, err := q.DeleteLLMCallsOlderThan(ctx, domain.FormatTime(before))
		if err != nil {
			return err
		}
		n = deleted
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("store: prune llm calls: %w", err)
	}
	return n, nil
}

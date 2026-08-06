package store

import (
	"context"
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
	n = n.WithDefaults()

	item, err := s.GetItem(ctx, n.ItemID)
	if err != nil {
		return domain.Occurrence{}, fmt.Errorf("store: create occurrence: %w", err)
	}
	if err := n.Validate(item.Kind); err != nil {
		return domain.Occurrence{}, err
	}

	params := sqlc.CreateOccurrenceParams{
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
	}

	var row sqlc.Occurrence
	err = s.tx(ctx, func(q *sqlc.Queries) error {
		var err error
		row, err = q.CreateOccurrence(ctx, params)
		return err
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

// PendingOverdue counts occurrences the scheduler should already have claimed:
// pending, at least OverdueGrace past their start time, belonging to an item
// that is active, unarchived, unpaused, and notified at its time.
//
// Above zero means the scheduler has stalled, which is the one failure in this
// system worth alerting on — everything else degrades to a plainer reminder,
// and this degrades to no reminder.
func (s *Store) PendingOverdue(ctx context.Context) (int, error) {
	now := time.Now()
	nowText := domain.FormatTime(now)

	n, err := s.read.CountPendingOverdue(ctx, sqlc.CountPendingOverdueParams{
		StartsAt:    domain.FormatTime(now.Add(-OverdueGrace)),
		PausedUntil: &nowText,
	})
	if err != nil {
		return 0, fmt.Errorf("store: count pending overdue: %w", err)
	}
	return int(n), nil
}

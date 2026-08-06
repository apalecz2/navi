package store

import (
	"context"
	"fmt"
	"time"

	"github.com/aidenpaleczny/navi/internal/domain"
	"github.com/aidenpaleczny/navi/internal/store/sqlc"
)

// CreateItem writes an item and returns it as stored. The id and the timestamps
// are assigned here, so no caller invents an identifier and no two callers
// disagree about what "now" means in a row.
func (s *Store) CreateItem(ctx context.Context, n domain.NewItem) (domain.Item, error) {
	n = n.WithDefaults()
	if err := n.Validate(); err != nil {
		return domain.Item{}, err
	}

	now := domain.FormatTime(time.Now())
	params := sqlc.CreateItemParams{
		ID:                 domain.NewID(),
		Kind:               string(n.Kind),
		Title:              n.Title,
		Notes:              n.Notes,
		Schedule:           string(n.Schedule),
		Tz:                 n.TZ,
		TzMode:             string(*n.TZMode),
		NotifyPolicy:       string(*n.NotifyPolicy),
		Priority:           int64(*n.Priority),
		GracePeriodMinutes: int64Ptr(n.GracePeriodMinutes),
		ReconcileAt:        n.ReconcileAt,
		SnoozeCap:          int64(*n.SnoozeCap),
		Attrs:              string(n.Attrs),
		CreatedAt:          now,
		UpdatedAt:          now,
	}

	var row sqlc.Item
	err := s.tx(ctx, func(q *sqlc.Queries) error {
		var err error
		row, err = q.CreateItem(ctx, params)
		return err
	})
	if err != nil {
		return domain.Item{}, fmt.Errorf("store: create item: %w", err)
	}
	return toDomainItem(row)
}

// GetItem returns one item, or ErrNotFound.
func (s *Store) GetItem(ctx context.Context, id string) (domain.Item, error) {
	row, err := s.read.GetItem(ctx, id)
	if err != nil {
		return domain.Item{}, notFound("store: get item", err)
	}
	return toDomainItem(row)
}

// ListActiveItems returns every unarchived, active item. This is the set
// injected into every agent turn, so it is one query rather than a filter
// applied by each caller.
func (s *Store) ListActiveItems(ctx context.Context) ([]domain.Item, error) {
	rows, err := s.read.ListActiveItems(ctx)
	if err != nil {
		return nil, fmt.Errorf("store: list active items: %w", err)
	}

	items := make([]domain.Item, 0, len(rows))
	for _, row := range rows {
		item, err := toDomainItem(row)
		if err != nil {
			return nil, fmt.Errorf("store: list active items: %w", err)
		}
		items = append(items, item)
	}
	return items, nil
}

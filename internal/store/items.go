package store

import (
	"context"
	"errors"
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

// LiveItem returns an item that is not archived, or a *domain.ValidationError
// naming why it does not resolve - the store-side half of Layer 2's
// item_id-resolves check (05-schedule-spec.md#validation's last row), the
// same check domain.ValidationError's own doc comment already names as one
// of the three places that produce one.
func (s *Store) LiveItem(ctx context.Context, id string) (domain.Item, error) {
	item, err := s.GetItem(ctx, id)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return domain.Item{}, domain.Invalid("item_exists", "item_id", "item %q does not exist", id)
		}
		return domain.Item{}, fmt.Errorf("store: live item: %w", err)
	}
	if item.ArchivedAt != nil {
		return domain.Item{}, domain.Invalid("item_exists", "item_id", "item %q is archived", id)
	}
	return item, nil
}

// ItemFilter is list_items' filter argument (06-agent-spec.md#tool-catalog).
type ItemFilter string

const (
	FilterActive ItemFilter = "active"
	FilterAll    ItemFilter = "all"
	FilterPaused ItemFilter = "paused"
)

// ListItems returns every unarchived item, filtered in Go rather than in SQL:
// a second parameterized query keyed on the filter is exactly the sqlc.arg()
// shape sqlc.yaml already records as broken once in this codebase, and the
// filtering itself is one field read (domain.Item.IsPaused) against a set
// small enough that a second round trip buys nothing. An unrecognized filter
// value falls back to active - Layer 1 in internal/agent has already
// rejected a bad one by the time this runs.
func (s *Store) ListItems(ctx context.Context, filter ItemFilter, now time.Time) ([]domain.Item, error) {
	rows, err := s.read.ListUnarchivedItems(ctx)
	if err != nil {
		return nil, fmt.Errorf("store: list items: %w", err)
	}

	items := make([]domain.Item, 0, len(rows))
	for _, row := range rows {
		item, err := toDomainItem(row)
		if err != nil {
			return nil, fmt.Errorf("store: list items: %w", err)
		}
		switch filter {
		case FilterAll:
			items = append(items, item)
		case FilterPaused:
			if item.IsPaused(now) {
				items = append(items, item)
			}
		default:
			if item.Active && !item.IsPaused(now) {
				items = append(items, item)
			}
		}
	}
	return items, nil
}

package store

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/aidenpaleczny/navi/internal/domain"
	"github.com/aidenpaleczny/navi/internal/store/sqlc"
)

// CreateItemAndMaterialize writes a new item and materializes its first
// occurrences in one transaction: a failure at any point - the row insert, a
// single occurrence insert - leaves nothing behind
// (docs/06-agent-spec.md#validation, Layer 3).
//
// It is a sibling to MaterializeItem rather than a caller of it, on purpose:
// materializeTx is unexported and separated from the transaction that wraps
// it exactly so that a write path like this one can call it alongside the
// item write, sharing the one transaction MaterializeItem itself opens on
// its own. A caller reaching for materializer.Item here would open a second
// transaction, which is the one shape the "leaves nothing behind" guarantee
// rules out.
//
// plan receives the item exactly as it will be stored (real id, defaults
// applied) because the occurrence rows it returns need a real item_id.
// existing is always empty here - a new item has no occurrences - the
// parameter exists so this signature matches UpdateItemAndMaterialize's.
func (s *Store) CreateItemAndMaterialize(
	ctx context.Context,
	n domain.NewItem,
	now time.Time,
	plan func(item domain.Item, existing []domain.Occurrence) (Plan, error),
) (domain.Item, Applied, error) {
	n = n.WithDefaults()
	if err := n.Validate(); err != nil {
		return domain.Item{}, Applied{}, err
	}

	nowText := domain.FormatTime(time.Now())
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
		CreatedAt:          nowText,
		UpdatedAt:          nowText,
	}

	var item domain.Item
	var applied Applied
	err := s.tx(ctx, func(q *sqlc.Queries) error {
		row, err := q.CreateItem(ctx, params)
		if err != nil {
			return err
		}
		if item, err = toDomainItem(row); err != nil {
			return err
		}
		applied, err = s.materializeTx(ctx, q, item, now, func(existing []domain.Occurrence) (Plan, error) {
			return plan(item, existing)
		})
		return err
	})
	if err != nil {
		return domain.Item{}, Applied{}, fmt.Errorf("store: create item and materialize: %w", err)
	}
	return item, applied, nil
}

// ItemPatch is a partial update: a nil field means "leave this column
// alone." There is no way this session to clear a nullable column
// (Notes, GracePeriodMinutes, ReconcileAt) back to NULL - matching NewItem's
// existing convention that "absent" is representable but "set to null" is
// not yet a case update_item needs.
type ItemPatch struct {
	Title        *string
	Notes        *string
	Schedule     *json.RawMessage
	Kind         *domain.Kind
	TZ           *string
	TZMode       *domain.TZMode
	NotifyPolicy *domain.NotifyPolicy

	Priority           *int
	GracePeriodMinutes *int
	ReconcileAt        *string
	Attrs              *json.RawMessage
}

// apply returns item with every non-nil field of p overwritten onto it.
func (p ItemPatch) apply(item domain.Item) domain.Item {
	if p.Title != nil {
		item.Title = *p.Title
	}
	if p.Notes != nil {
		item.Notes = p.Notes
	}
	if p.Schedule != nil {
		item.Schedule = *p.Schedule
	}
	if p.Kind != nil {
		item.Kind = *p.Kind
	}
	if p.TZ != nil {
		item.TZ = *p.TZ
	}
	if p.TZMode != nil {
		item.TZMode = *p.TZMode
	}
	if p.NotifyPolicy != nil {
		item.NotifyPolicy = *p.NotifyPolicy
	}
	if p.Priority != nil {
		item.Priority = *p.Priority
	}
	if p.GracePeriodMinutes != nil {
		item.GracePeriodMinutes = p.GracePeriodMinutes
	}
	if p.ReconcileAt != nil {
		item.ReconcileAt = p.ReconcileAt
	}
	if p.Attrs != nil {
		item.Attrs = *p.Attrs
	}
	return item
}

// UpdateItemAndMaterialize applies patch to an existing, live item and, when
// rematerialize is non-nil, re-plans its future occurrences - both in one
// transaction. rematerialize is nil exactly when internal/agent's
// field-level diff found nothing schedule-affecting (title, notes,
// priority, or attrs only), and that is the entire mechanism behind "a typo
// fix must not churn the calendar"
// (docs/05-schedule-spec.md#edit-scope): materializeTx is never invoked, so
// not one occurrence row is read, deleted, or inserted.
func (s *Store) UpdateItemAndMaterialize(
	ctx context.Context,
	id string,
	patch ItemPatch,
	now time.Time,
	rematerialize func(item domain.Item, existing []domain.Occurrence) (Plan, error),
) (domain.Item, Applied, error) {
	var item domain.Item
	var applied Applied
	err := s.tx(ctx, func(q *sqlc.Queries) error {
		current, err := q.GetItem(ctx, id)
		if err != nil {
			return notFound("store: update item", err)
		}
		curItem, err := toDomainItem(current)
		if err != nil {
			return err
		}
		merged := patch.apply(curItem)

		row, err := q.UpdateItem(ctx, sqlc.UpdateItemParams{
			Title:              merged.Title,
			Notes:              merged.Notes,
			Schedule:           string(merged.Schedule),
			Kind:               string(merged.Kind),
			Tz:                 merged.TZ,
			TzMode:             string(merged.TZMode),
			NotifyPolicy:       string(merged.NotifyPolicy),
			Priority:           int64(merged.Priority),
			GracePeriodMinutes: int64Ptr(merged.GracePeriodMinutes),
			ReconcileAt:        merged.ReconcileAt,
			Attrs:              string(merged.Attrs),
			UpdatedAt:          domain.FormatTime(time.Now()),
			ID:                 id,
		})
		if err != nil {
			return notFound("store: update item", err)
		}
		if item, err = toDomainItem(row); err != nil {
			return err
		}
		if rematerialize == nil {
			return nil
		}
		applied, err = s.materializeTx(ctx, q, item, now, func(existing []domain.Occurrence) (Plan, error) {
			return rematerialize(item, existing)
		})
		return err
	})
	if err != nil {
		return domain.Item{}, Applied{}, fmt.Errorf("store: update item and materialize: %w", err)
	}
	return item, applied, nil
}

// ArchiveItem replaces deletion (A7): it sets archived_at, then
// re-materializes with rematerialize, which the caller builds from an
// already-archived item so that the plan comes out as "delete every future
// pending non-override row, insert nothing" with no new logic - the same
// generate == false path a paused-forever or inactive item already takes.
// History is untouched because the delete guard on materializeTx's plan
// already excludes every non-pending row.
func (s *Store) ArchiveItem(
	ctx context.Context,
	id string,
	now time.Time,
	rematerialize func(item domain.Item, existing []domain.Occurrence) (Plan, error),
) (domain.Item, Applied, error) {
	var item domain.Item
	var applied Applied
	err := s.tx(ctx, func(q *sqlc.Queries) error {
		archivedAt := domain.FormatTime(now)
		row, err := q.ArchiveItem(ctx, sqlc.ArchiveItemParams{
			ArchivedAt: &archivedAt,
			UpdatedAt:  archivedAt,
			ID:         id,
		})
		if err != nil {
			return notFound("store: archive item", err)
		}
		if item, err = toDomainItem(row); err != nil {
			return err
		}
		applied, err = s.materializeTx(ctx, q, item, now, func(existing []domain.Occurrence) (Plan, error) {
			return rematerialize(item, existing)
		})
		return err
	})
	if err != nil {
		return domain.Item{}, Applied{}, fmt.Errorf("store: archive item: %w", err)
	}
	return item, applied, nil
}

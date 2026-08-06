package store

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/aidenpaleczny/navi/internal/domain"
	"github.com/aidenpaleczny/navi/internal/store/sqlc"
)

// This file is the boundary between the two representations, and it is the only
// place either one leaks into the other.
//
// SQLite has no timestamp type and no boolean, so a row carries ISO-8601 UTC
// strings and 0/1 integers. Domain types carry time.Time and bool. Every
// conversion goes through the helpers below, so the storage format is decided
// in domain.TimeLayout and applied here, and a query that forgets is a compile
// error rather than a row that sorts wrong.

func toDomainItem(row sqlc.Item) (domain.Item, error) {
	createdAt, err := domain.ParseTime(row.CreatedAt)
	if err != nil {
		return domain.Item{}, fmt.Errorf("item %s created_at: %w", row.ID, err)
	}
	updatedAt, err := domain.ParseTime(row.UpdatedAt)
	if err != nil {
		return domain.Item{}, fmt.Errorf("item %s updated_at: %w", row.ID, err)
	}
	pausedUntil, err := parseTimePtr(row.PausedUntil)
	if err != nil {
		return domain.Item{}, fmt.Errorf("item %s paused_until: %w", row.ID, err)
	}
	archivedAt, err := parseTimePtr(row.ArchivedAt)
	if err != nil {
		return domain.Item{}, fmt.Errorf("item %s archived_at: %w", row.ID, err)
	}
	lastSyncedAt, err := parseTimePtr(row.LastSyncedAt)
	if err != nil {
		return domain.Item{}, fmt.Errorf("item %s last_synced_at: %w", row.ID, err)
	}

	return domain.Item{
		ID:                 row.ID,
		Kind:               domain.Kind(row.Kind),
		Title:              row.Title,
		Notes:              row.Notes,
		Schedule:           json.RawMessage(row.Schedule),
		TZ:                 row.Tz,
		TZMode:             domain.TZMode(row.TzMode),
		NotifyPolicy:       domain.NotifyPolicy(row.NotifyPolicy),
		Priority:           int(row.Priority),
		GracePeriodMinutes: intPtr(row.GracePeriodMinutes),
		ReconcileAt:        row.ReconcileAt,
		SnoozeCap:          int(row.SnoozeCap),
		Active:             row.Active != 0,
		PausedUntil:        pausedUntil,
		ArchivedAt:         archivedAt,
		Attrs:              json.RawMessage(row.Attrs),
		Source:             domain.Source(row.Source),
		ExternalID:         row.ExternalID,
		ETag:               row.Etag,
		LastSyncedAt:       lastSyncedAt,
		CreatedAt:          createdAt,
		UpdatedAt:          updatedAt,
	}, nil
}

func toDomainOccurrence(row sqlc.Occurrence) (domain.Occurrence, error) {
	startsAt, err := domain.ParseTime(row.StartsAt)
	if err != nil {
		return domain.Occurrence{}, fmt.Errorf("occurrence %s starts_at: %w", row.ID, err)
	}
	createdAt, err := domain.ParseTime(row.CreatedAt)
	if err != nil {
		return domain.Occurrence{}, fmt.Errorf("occurrence %s created_at: %w", row.ID, err)
	}
	endsAt, err := parseTimePtr(row.EndsAt)
	if err != nil {
		return domain.Occurrence{}, fmt.Errorf("occurrence %s ends_at: %w", row.ID, err)
	}
	notifiedAt, err := parseTimePtr(row.NotifiedAt)
	if err != nil {
		return domain.Occurrence{}, fmt.Errorf("occurrence %s notified_at: %w", row.ID, err)
	}
	reconciledAt, err := parseTimePtr(row.ReconciledAt)
	if err != nil {
		return domain.Occurrence{}, fmt.Errorf("occurrence %s reconciled_at: %w", row.ID, err)
	}
	resolvedAt, err := parseTimePtr(row.ResolvedAt)
	if err != nil {
		return domain.Occurrence{}, fmt.Errorf("occurrence %s resolved_at: %w", row.ID, err)
	}
	generatedAt, err := parseTimePtr(row.MessageGeneratedAt)
	if err != nil {
		return domain.Occurrence{}, fmt.Errorf("occurrence %s message_generated_at: %w", row.ID, err)
	}

	var source *domain.ResolutionSource
	if row.ResolutionSource != nil {
		s := domain.ResolutionSource(*row.ResolutionSource)
		source = &s
	}

	return domain.Occurrence{
		ID:                 row.ID,
		ItemID:             row.ItemID,
		StartsAt:           startsAt,
		EndsAt:             endsAt,
		Status:             domain.Status(row.Status),
		IsOverride:         row.IsOverride != 0,
		ParentOccurrenceID: row.ParentOccurrenceID,
		SnoozeDepth:        int(row.SnoozeDepth),
		NotifiedAt:         notifiedAt,
		ReconciledAt:       reconciledAt,
		ResolvedAt:         resolvedAt,
		ResolutionNote:     row.ResolutionNote,
		ResolutionSource:   source,
		MessageText:        row.MessageText,
		MessageModel:       row.MessageModel,
		MessageGeneratedAt: generatedAt,
		GenerationAttempts: int(row.GenerationAttempts),
		GenerationPass:     int(row.GenerationPass),
		CreatedAt:          createdAt,
	}, nil
}

// parseTimePtr reads a nullable timestamp column.
func parseTimePtr(s *string) (*time.Time, error) {
	if s == nil {
		return nil, nil
	}
	t, err := domain.ParseTime(*s)
	if err != nil {
		return nil, err
	}
	return &t, nil
}

// formatTimePtr writes a nullable timestamp column.
func formatTimePtr(t *time.Time) *string {
	if t == nil {
		return nil
	}
	s := domain.FormatTime(*t)
	return &s
}

// intPtr narrows a nullable INTEGER column. SQLite stores every integer as 64
// bits; the domain uses int for values that are counts and minutes.
func intPtr(v *int64) *int {
	if v == nil {
		return nil
	}
	n := int(*v)
	return &n
}

// int64Ptr widens back for a parameter.
func int64Ptr(v *int) *int64 {
	if v == nil {
		return nil
	}
	n := int64(*v)
	return &n
}

// boolToInt renders a bool for a column that is 0 or 1.
func boolToInt(b bool) int64 {
	if b {
		return 1
	}
	return 0
}

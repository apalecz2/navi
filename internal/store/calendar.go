package store

import (
	"context"
	"fmt"
	"time"

	"github.com/aidenpaleczny/navi/internal/domain"
	"github.com/aidenpaleczny/navi/internal/store/sqlc"
)

// CalendarOccurrence is one row of the calendar's date-range read. It embeds
// TodayOccurrence so the day view's row component and the calendar's detail
// panel are built from the same fields rather than from two parallel types, and
// adds what only a range needs: the chain columns, and whether the item has
// since been archived.
type CalendarOccurrence struct {
	TodayOccurrence

	IsOverride         bool
	ParentOccurrenceID *string

	// ItemArchived marks history that belongs to a deleted item. The row is still
	// returned and its title is still the item's: items are archived rather than
	// deleted so that past days keep a name to show.
	ItemArchived bool
}

// CalendarFilter narrows a range. Both fields are optional and the zero value
// means "no filter".
type CalendarFilter struct {
	ItemID string
	Status domain.Status
}

// ListCalendar is V4's single date-range query: every occurrence starting in
// [from, to), oldest first, archived items included.
//
// "Single" is about the request, not the SQL text. There are three statements
// behind it because SQLite cannot use an index through an optional-filter
// predicate - see the comment on ListCalendarAll in queries/occurrences.sql for
// the measurement - and the choice between them is the only logic here. Which
// index each one rides:
//
//	no filter    -> idx_occ_starts        (migration 0006)
//	item filter  -> idx_occ_item_starts   (status, if given, is a residual)
//	status only  -> idx_occ_status_starts
//
// Snoozed parents are returned like any other row. Whether a chain is drawn once
// is the view's decision, not a filter here, so the API stays a faithful row
// listing and status=snoozed still means something.
func (s *Store) ListCalendar(ctx context.Context, from, to time.Time, f CalendarFilter) ([]CalendarOccurrence, error) {
	lo, hi := domain.FormatTime(from), domain.FormatTime(to)

	var rows []calendarRow
	switch {
	case f.ItemID != "":
		got, err := s.read.ListCalendarByItem(ctx, sqlc.ListCalendarByItemParams{
			ItemID: f.ItemID, StartsAt: lo, StartsAt_2: hi,
			Column4: string(f.Status), Status: string(f.Status),
		})
		if err != nil {
			return nil, fmt.Errorf("store: list calendar: %w", err)
		}
		for _, r := range got {
			rows = append(rows, calendarRow(r))
		}
	case f.Status != "":
		got, err := s.read.ListCalendarByStatus(ctx, sqlc.ListCalendarByStatusParams{
			Status: string(f.Status), StartsAt: lo, StartsAt_2: hi,
		})
		if err != nil {
			return nil, fmt.Errorf("store: list calendar: %w", err)
		}
		for _, r := range got {
			rows = append(rows, calendarRow(r))
		}
	default:
		got, err := s.read.ListCalendarAll(ctx, sqlc.ListCalendarAllParams{StartsAt: lo, StartsAt_2: hi})
		if err != nil {
			return nil, fmt.Errorf("store: list calendar: %w", err)
		}
		for _, r := range got {
			rows = append(rows, calendarRow(r))
		}
	}

	out := make([]CalendarOccurrence, 0, len(rows))
	for _, row := range rows {
		startsAt, err := domain.ParseTime(row.StartsAt)
		if err != nil {
			return nil, fmt.Errorf("store: list calendar: %w", err)
		}
		resolvedAt, err := parseTimePtr(row.ResolvedAt)
		if err != nil {
			return nil, fmt.Errorf("store: list calendar: %w", err)
		}
		var source *domain.ResolutionSource
		if row.ResolutionSource != nil {
			src := domain.ResolutionSource(*row.ResolutionSource)
			source = &src
		}
		out = append(out, CalendarOccurrence{
			TodayOccurrence: TodayOccurrence{
				ID:               row.ID,
				ItemID:           row.ItemID,
				ItemTitle:        row.Title,
				StartsAt:         startsAt,
				Status:           domain.Status(row.Status),
				ResolvedAt:       resolvedAt,
				ResolutionSource: source,
				NotifyPolicy:     domain.NotifyPolicy(row.NotifyPolicy),
				Priority:         int(row.Priority),
				SnoozeDepth:      int(row.SnoozeDepth),
			},
			IsOverride:         row.IsOverride != 0,
			ParentOccurrenceID: row.ParentOccurrenceID,
			ItemArchived:       row.ArchivedAt != nil,
		})
	}
	return out, nil
}

// calendarRow is the shape the three generated row types share, so decoding it
// is written once. They are identical field for field, which is why the
// conversion from each is a plain type conversion.
type calendarRow = sqlc.ListCalendarAllRow

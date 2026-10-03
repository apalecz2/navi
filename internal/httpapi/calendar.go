package httpapi

import (
	"net/http"
	"strings"
	"time"

	"github.com/aidenpaleczny/navi/internal/domain"
	"github.com/aidenpaleczny/navi/internal/store"
)

// MaxCalendarDays is the cap on a range, inclusive of both ends. 07-api-spec
// says "capped at 400 days": a year plus a margin, so a year view and its
// neighbouring weeks are one request, while a typo in a year cannot ask the
// database for a decade.
const MaxCalendarDays = 400

// calendarOccurrence is one element of GET /api/occurrences. It is the
// /api/today element plus the things a range needs and a single day does not:
// the local date the row falls on, and the chain columns.
type calendarOccurrence struct {
	todayOccurrence

	DateLocal          string  `json:"date_local"`
	IsOverride         bool    `json:"is_override"`
	ParentOccurrenceID *string `json:"parent_occurrence_id"`
	ItemArchived       bool    `json:"item_archived"`
}

type calendarResponse struct {
	From     string `json:"from"`
	To       string `json:"to"`
	Timezone string `json:"timezone"`

	// MaterializedThrough is kv.last_materialized_through, the same value
	// /healthz reports as horizon_days. A range reaching past it is not "nothing
	// scheduled" but "not generated yet", and a client cannot tell the two apart
	// from an empty list, so it is carried here. null means nothing has ever
	// been materialized.
	MaterializedThrough *string `json:"materialized_through"`

	Occurrences []calendarOccurrence `json:"occurrences"`
}

// handleListOccurrences is GET /api/occurrences?from=&to=&status=&item_id=.
//
// from and to are inclusive ISO dates read in the device zone,
// schedule.Zones.Local() - the clock /api/today and the check-in use, never an
// item's own zone - so "2026-08-05" names the same day here that the day view
// calls today. They resolve to the half-open instant range [from 00:00, to+1
// 00:00) in that zone, by calendar arithmetic and not by adding 24 hours, which
// is wrong on both DST days.
//
// Every rejection is a *domain.ValidationError through the ordinary envelope,
// and each message names the rule and the offending values.
func (s *Server) handleListOccurrences(w http.ResponseWriter, r *http.Request) {
	fail := func(what string, err error) {
		s.log.Error("occurrences: "+what, "err", err)
		writeJSON(w, s.log, http.StatusInternalServerError, errorBody{
			Error: "internal", Message: "occurrences could not be loaded",
		})
	}

	zones, err := s.zones(r.Context())
	if err != nil {
		fail("resolve zones", err)
		return
	}
	loc := zones.Local()

	q := r.URL.Query()
	from, to, filter, ve := parseCalendarQuery(q.Get("from"), q.Get("to"), q.Get("status"), q.Get("item_id"), loc)
	if ve != nil {
		writeValidationError(w, s.log, ve)
		return
	}

	// Inclusive end date to an exclusive end instant. Date normalizes day+1 across
	// a month boundary.
	end := time.Date(to.Year(), to.Month(), to.Day()+1, 0, 0, 0, 0, loc)

	occs, err := s.store.ListCalendar(r.Context(), from, end, filter)
	if err != nil {
		fail("list calendar", err)
		return
	}
	through, ok, err := s.store.LastMaterializedThrough(r.Context())
	if err != nil {
		fail("horizon", err)
		return
	}

	out := calendarResponse{
		From:        from.Format("2006-01-02"),
		To:          to.Format("2006-01-02"),
		Timezone:    loc.String(),
		Occurrences: make([]calendarOccurrence, 0, len(occs)),
	}
	if ok {
		t := domain.FormatTime(through)
		out.MaterializedThrough = &t
	}
	for _, o := range occs {
		row := calendarOccurrence{
			todayOccurrence: todayOccurrence{
				ID:            o.ID,
				ItemID:        o.ItemID,
				Title:         o.ItemTitle,
				StartsAt:      domain.FormatTime(o.StartsAt),
				StartsAtLocal: o.StartsAt.In(loc).Format("15:04"),
				Status:        string(o.Status),
				ResolvedAt:    formatTimePtr(o.ResolvedAt),
				NotifyPolicy:  string(o.NotifyPolicy),
				Priority:      o.Priority,
				SnoozeDepth:   o.SnoozeDepth,
			},
			DateLocal:          o.StartsAt.In(loc).Format("2006-01-02"),
			IsOverride:         o.IsOverride,
			ParentOccurrenceID: o.ParentOccurrenceID,
			ItemArchived:       o.ItemArchived,
		}
		if o.ResolutionSource != nil {
			src := string(*o.ResolutionSource)
			row.ResolutionSource = &src
		}
		out.Occurrences = append(out.Occurrences, row)
	}
	writeJSON(w, s.log, http.StatusOK, out)
}

// parseCalendarQuery validates the four parameters and returns both dates as
// local midnight (the caller adds the day to the second). It stops at the first
// failure, in the order the parameters are listed, so a request with two
// problems reports the earlier one.
func parseCalendarQuery(fromS, toS, statusS, itemID string, loc *time.Location) (from, to time.Time, f store.CalendarFilter, ve *domain.ValidationError) {
	parse := func(field, v string) (time.Time, *domain.ValidationError) {
		if v == "" {
			return time.Time{}, domain.Invalid("calendar_date_required", field,
				"%s is required: an ISO date, YYYY-MM-DD", field)
		}
		// This layout is strict about width, so "2026-8-5" is rejected rather than
		// guessed at, and an impossible date like "2026-02-30" is rejected rather
		// than rolled over.
		t, err := time.ParseInLocation("2006-01-02", v, loc)
		if err != nil {
			return time.Time{}, domain.Invalid("calendar_date_format", field,
				"%s %q is not an ISO date, YYYY-MM-DD", field, v)
		}
		return t, nil
	}

	if from, ve = parse("from", fromS); ve != nil {
		return
	}
	if to, ve = parse("to", toS); ve != nil {
		return
	}
	if to.Before(from) {
		ve = domain.Invalid("calendar_range_order", "from",
			"from %s is after to %s: the range must run forward", fromS, toS)
		return
	}

	// Inclusive count by civil date, not by instant difference: a range holding
	// a 23-hour or 25-hour day is still that many calendar days.
	utc := func(t time.Time) time.Time { return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC) }
	days := int(utc(to).Sub(utc(from))/(24*time.Hour)) + 1
	if days > MaxCalendarDays {
		ve = domain.Invalid("calendar_range_cap", "to",
			"range from %s to %s is %d days: the maximum is %d", fromS, toS, days, MaxCalendarDays)
		return
	}

	if statusS != "" {
		st, ok := domain.ParseStatus(statusS)
		if !ok {
			names := make([]string, len(domain.Statuses))
			for i, s := range domain.Statuses {
				names[i] = string(s)
			}
			ve = domain.Invalid("calendar_status", "status",
				"status %q is not one of %s", statusS, strings.Join(names, ", "))
			return
		}
		f.Status = st
	}
	if itemID != "" {
		if !domain.ValidID(itemID) {
			ve = domain.Invalid("calendar_item_id", "item_id",
				"item_id %q is not an item id", itemID)
			return
		}
		f.ItemID = itemID
	}
	return
}

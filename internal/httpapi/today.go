package httpapi

import (
	"net/http"
	"time"

	"github.com/aidenpaleczny/navi/internal/domain"
	"github.com/aidenpaleczny/navi/internal/store"
)

// todayOccurrence is one element of GET /api/today, in the shape
// docs/07-api-spec.md#get-apitoday gives it.
type todayOccurrence struct {
	ID               string  `json:"id"`
	ItemID           string  `json:"item_id"`
	Title            string  `json:"title"`
	StartsAt         string  `json:"starts_at"`
	StartsAtLocal    string  `json:"starts_at_local"`
	Status           string  `json:"status"`
	ResolvedAt       *string `json:"resolved_at"`
	ResolutionSource *string `json:"resolution_source"`
	NotifyPolicy     string  `json:"notify_policy"`
	Priority         int     `json:"priority"`
	SnoozeDepth      int     `json:"snooze_depth"`
}

type todayCounts struct {
	Total       int `json:"total"`
	Resolved    int `json:"resolved"`
	Outstanding int `json:"outstanding"`
}

type todayResponse struct {
	Date        string            `json:"date"`
	Timezone    string            `json:"timezone"`
	Occurrences []todayOccurrence `json:"occurrences"`
	Counts      todayCounts       `json:"counts"`
}

// handleToday is GET /api/today: everything starting inside the current local
// day, in the device zone — schedule.Zones.Local(), the clock the check-in and
// the snooze presets resolve against, never an item's own zone.
//
// It is a read over store.TodaysOccurrences, the same method the agent's context
// block and the day view at /app read, so "what is due today" has one
// implementation. A snoozed row is listed but not counted, since its child
// carries the chain (store.CountToday).
func (s *Server) handleToday(w http.ResponseWriter, r *http.Request) {
	zones, err := s.zones(r.Context())
	if err != nil {
		s.log.Error("today: resolve zones", "err", err)
		writeJSON(w, s.log, http.StatusInternalServerError, errorBody{
			Error: "internal", Message: "today could not be loaded",
		})
		return
	}
	loc := zones.Local()

	occs, err := s.store.TodaysOccurrences(r.Context(), loc)
	if err != nil {
		s.log.Error("today: list occurrences", "err", err)
		writeJSON(w, s.log, http.StatusInternalServerError, errorBody{
			Error: "internal", Message: "today could not be loaded",
		})
		return
	}

	out := todayResponse{
		Date:        time.Now().In(loc).Format("2006-01-02"),
		Timezone:    loc.String(),
		Occurrences: make([]todayOccurrence, 0, len(occs)),
	}
	for _, o := range occs {
		row := todayOccurrence{
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
		}
		if o.ResolutionSource != nil {
			src := string(*o.ResolutionSource)
			row.ResolutionSource = &src
		}
		out.Occurrences = append(out.Occurrences, row)
	}
	c := store.CountToday(occs)
	out.Counts = todayCounts{Total: c.Total, Resolved: c.Resolved, Outstanding: c.Outstanding}

	writeJSON(w, s.log, http.StatusOK, out)
}

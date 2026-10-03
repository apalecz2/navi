package web

import (
	"encoding/json"
	"time"

	"github.com/aidenpaleczny/navi/internal/domain"
	"github.com/aidenpaleczny/navi/internal/store"
)

// Day is the view model of the day view: store.TodayOccurrence already resolved
// into the strings and booleans a template needs, so the template holds no
// rule about which status offers which button.
type Day struct {
	Date     string // 2026-08-05
	Heading  string // Wednesday, August 5
	Timezone string
	Counts   store.TodayCounts
	Rows     []Row
}

// Row is one occurrence on the page.
type Row struct {
	ID     string
	Title  string
	Time   string // local HH:MM
	Status string
	Label  string

	// Resolvable is "Done and Skip are offered": the two live statuses, which
	// are exactly the ones domain.Transition allows a resolution from. Snoozable
	// is narrower: notified -> snoozed is the table's only snooze edge, so a
	// pending row has nothing to be pushed back from and the endpoint would 409
	// it. Offering the button anyway would be a tap that always fails.
	Resolvable bool
	Snoozable  bool

	// Meta is the one-line "done 09:12 via web" on a resolved row.
	Meta string

	SnoozeDepth int
	Silent      bool
}

// statusLabels is the one table of what each status is called on screen. It is
// rendered into the page as JSON and read by app.js for the optimistic flip, so
// the label a tap shows instantly and the label the server renders afterwards
// cannot differ.
var statusLabels = map[string]string{
	string(domain.StatusPending):   "Scheduled",
	string(domain.StatusNotified):  "Due",
	string(domain.StatusCompleted): "Done",
	string(domain.StatusSkipped):   "Skipped",
	string(domain.StatusSnoozed):   "Snoozed",
	string(domain.StatusMissed):    "Missed",
}

func labelFor(status string) string {
	if l, ok := statusLabels[status]; ok {
		return l
	}
	return status
}

func labelsJSON() string {
	b, _ := json.Marshal(statusLabels)
	return string(b)
}

// NewDay builds the view model. now is only used for the heading; loc is the
// device zone the occurrences were selected in.
func NewDay(now time.Time, loc *time.Location, occs []store.TodayOccurrence) Day {
	d := Day{
		Date:     now.Format("2006-01-02"),
		Heading:  now.Format("Monday, January 2"),
		Timezone: loc.String(),
		Counts:   store.CountToday(occs),
		Rows:     make([]Row, 0, len(occs)),
	}
	for _, o := range occs {
		r := Row{
			ID:          o.ID,
			Title:       o.ItemTitle,
			Time:        o.StartsAt.In(loc).Format("15:04"),
			Status:      string(o.Status),
			Label:       labelFor(string(o.Status)),
			Resolvable:  o.Status == domain.StatusPending || o.Status == domain.StatusNotified,
			Snoozable:   o.Status == domain.StatusNotified,
			SnoozeDepth: o.SnoozeDepth,
			Silent:      o.NotifyPolicy == domain.NotifySilent,
		}
		if o.ResolvedAt != nil && o.Status != domain.StatusSnoozed {
			r.Meta = o.ResolvedAt.In(loc).Format("15:04")
			if o.ResolutionSource != nil {
				r.Meta += " · " + string(*o.ResolutionSource)
			}
		}
		d.Rows = append(d.Rows, r)
	}
	return d
}

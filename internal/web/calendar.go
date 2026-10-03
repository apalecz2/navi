package web

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"net/url"
	"sort"
	"time"

	"github.com/aidenpaleczny/navi/internal/domain"
	"github.com/aidenpaleczny/navi/internal/store"
)

// MonthLayout is the form the m= parameter takes.
const MonthLayout = "2006-01"

// Calendar is the view model of /app/calendar: one month, already bucketed into
// days, so the template holds no date arithmetic and no rule about which rows
// are drawn.
type Calendar struct {
	Month    string // 2026-10
	Heading  string // October 2026
	Prev     string // 2026-09
	Next     string // 2026-11
	Today    string // the current month, for the "Today" jump
	Current  bool   // Month == Today
	Timezone string

	// Offset is how many blank cells precede the 1st in a week that starts on
	// Monday - the same first day goalPeriod and the materializer use for "week".
	Offset int

	Days   []CalDay
	Legend []LegendItem
	Note   HorizonNote

	// Open is the occurrence whose detail is showing, if any.
	Open *Detail
}

// CalDay is one day of the month.
type CalDay struct {
	Date    string // 2026-10-06
	Num     int
	Weekday string // Tue
	Today   bool
	Past    bool

	// Beyond is a day that starts at or after the materialization horizon: no
	// row exists for it yet, whatever is scheduled. Edge is the one day the
	// horizon falls inside, which is covered up to EdgeAt and not after.
	Beyond bool
	Edge   bool
	EdgeAt string

	// FirstBeyond marks the first uncovered day, where the narrow layout - which
	// hides empty days and so would hide the hatching - draws its horizon line.
	FirstBeyond bool

	Chips []Chip
}

// Chip is one live occurrence in a day cell. A snoozed parent is never a chip:
// see the chain rule on buildCalendar.
type Chip struct {
	ID    string
	Title string
	Time  string // local HH:MM of the drawn instant
	Tip   string

	Status string
	Glyph  string
	Hue    int

	// Snoozes is the chain depth, drawn as a mark. A chip with Snoozes > 0 is the
	// live link of a chain whose earlier links are folded into it.
	Snoozes int

	Archived bool
	Open     bool

	Href     string // the full page, so the link works without a script
	BodyHref string // the fragment htmx swaps in
}

// LegendItem is an item shown this month, with its colour.
type LegendItem struct {
	Title    string
	Hue      int
	Archived bool
}

// HorizonNote says where materialization ends when that matters to this month.
// Kind is "" (nothing to say), "partial", "none" or "unset".
type HorizonNote struct {
	Kind string
	Text string
}

// Detail is the panel under an opened chip.
type Detail struct {
	Row      Row
	Title    string
	Hue      int
	When     string
	Archived bool
	Chain    string

	// ChildHref opens the later link of a snoozed occurrence's chain, when it
	// falls in the month on screen.
	ChildHref string

	// SettleURL is where the day() component re-fetches after a resolution: this
	// same month with this same panel open, so a snooze that moves the row is
	// reflected in the grid and the panel in one swap.
	SettleURL string
	CloseHref string
	CloseBody string
}

// itemHue maps an item to a stable colour, a hue in [0, 360).
//
// The input is the item's id: a ULID is assigned once and never changes, so the
// colour is the same on every load, in every month, on every device, and does
// not depend on how many items exist or in what order a month happens to list
// them. The title is not used because it is editable; render order is not used
// because it makes a colour a property of the page. FNV-1a is enough - this is
// spreading, not security - and hashing the whole id rather than a prefix
// matters because a ULID's leading characters are a timestamp, so two items
// created in the same hour share a prefix.
//
// Two items can still land close together. Colour is therefore never the only
// carrier of identity: every chip shows the title, and the legend lists the
// items on screen.
func itemHue(id string) int {
	h := fnv.New32a()
	_, _ = h.Write([]byte(id))
	return int(h.Sum32() % 360)
}

func glyphFor(s domain.Status) string {
	switch s {
	case domain.StatusCompleted:
		return "✓"
	case domain.StatusSkipped:
		return "–"
	case domain.StatusMissed:
		return "!"
	case domain.StatusNotified:
		return "●"
	default:
		return "○"
	}
}

func monthURLs(month, open string) (page, body string) {
	q := url.Values{"m": {month}}
	if open != "" {
		q.Set("o", open)
	}
	return "/app/calendar?" + q.Encode(), "/app/calendar/body?" + q.Encode()
}

// buildCalendar loads one month with a single ListCalendar call and shapes it.
//
// Chains. The store returns snoozed parents like any other row, and this is
// where they stop being drawn: a chain appears once, as its live link - the
// child - at the child's time, carrying a mark with its depth. That is the
// rule store.CountToday already follows (a snoozed row is in neither bucket
// because its child carries the chain), and it is what keeps this view and the
// statistics view, which count a chain once through the chains view (D-011),
// from counting one reminder twice. The two bucket a chain by different days
// only when a snooze crosses local midnight: the calendar answers "when did or
// will it happen", so it files the chain under the live link's day; statistics
// answer "how did what was due that day go", and file it under the root's
// scheduled_at. The detail panel states the original due time, so the two can
// be reconciled by reading rather than by guessing.
func (a *App) buildCalendar(ctx context.Context, month time.Time, loc *time.Location, openID string) (Calendar, error) {
	start := time.Date(month.Year(), month.Month(), 1, 0, 0, 0, 0, loc)
	end := start.AddDate(0, 1, 0)

	occs, err := a.store.ListCalendar(ctx, start, end, store.CalendarFilter{})
	if err != nil {
		return Calendar{}, fmt.Errorf("web: calendar: %w", err)
	}
	through, haveThrough, err := a.store.LastMaterializedThrough(ctx)
	if err != nil {
		return Calendar{}, fmt.Errorf("web: calendar: %w", err)
	}

	now := time.Now().In(loc)
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc)
	cur := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, loc)
	ym := start.Format(MonthLayout)

	cal := Calendar{
		Month:    ym,
		Heading:  start.Format("January 2006"),
		Prev:     start.AddDate(0, -1, 0).Format(MonthLayout),
		Next:     start.AddDate(0, 1, 0).Format(MonthLayout),
		Today:    cur.Format(MonthLayout),
		Current:  start.Equal(cur),
		Timezone: loc.String(),
		Offset:   (int(start.Weekday()) + 6) % 7,
	}

	// Count by calendar: the hours between two local midnights are not a multiple
	// of 24 across a DST change.
	daysIn := time.Date(start.Year(), start.Month()+1, 0, 0, 0, 0, 0, time.UTC).Day()

	cal.Days = make([]CalDay, daysIn)
	for i := range cal.Days {
		ds := time.Date(start.Year(), start.Month(), i+1, 0, 0, 0, 0, loc)
		de := time.Date(start.Year(), start.Month(), i+2, 0, 0, 0, 0, loc)
		d := CalDay{
			Date:    ds.Format("2006-01-02"),
			Num:     i + 1,
			Weekday: ds.Format("Mon"),
			Today:   ds.Equal(today),
			Past:    ds.Before(today),
		}
		switch {
		case !haveThrough:
			// Nothing has ever been materialized. Past days are history, which is
			// whatever rows exist; from today on nothing has been generated.
			d.Beyond = !d.Past
		case !ds.Before(through):
			d.Beyond = true
		case de.After(through):
			d.Edge = true
			d.EdgeAt = through.In(loc).Format("15:04")
		}
		cal.Days[i] = d
	}
	for i := range cal.Days {
		if cal.Days[i].Beyond {
			cal.Days[i].FirstBeyond = true
			break
		}
	}

	// A snoozed parent is hidden only because its child is drawn. Remember which
	// ids have a child in this range so a parent whose child lies outside it
	// (snoozed past the month's end) is still shown - dropping both would make
	// the reminder vanish from the calendar.
	hasChild := map[string]bool{}
	byParent := map[string]string{}
	for _, o := range occs {
		if o.ParentOccurrenceID != nil {
			hasChild[*o.ParentOccurrenceID] = true
			byParent[*o.ParentOccurrenceID] = o.ID
		}
	}

	seen := map[string]LegendItem{}
	for _, o := range occs {
		if o.Status == domain.StatusSnoozed && hasChild[o.ID] {
			continue
		}
		local := o.StartsAt.In(loc)
		idx := local.Day() - 1
		if idx < 0 || idx >= len(cal.Days) {
			continue
		}
		page, body := monthURLs(ym, o.ID)
		hue := itemHue(o.ItemID)
		c := Chip{
			ID:       o.ID,
			Title:    o.ItemTitle,
			Time:     local.Format("15:04"),
			Status:   string(o.Status),
			Glyph:    glyphFor(o.Status),
			Hue:      hue,
			Snoozes:  o.SnoozeDepth,
			Archived: o.ItemArchived,
			Open:     o.ID == openID,
			Href:     page,
			BodyHref: body,
		}
		c.Tip = fmt.Sprintf("%s, %s, %s", o.ItemTitle, c.Time, labelFor(c.Status))
		cal.Days[idx].Chips = append(cal.Days[idx].Chips, c)
		seen[o.ItemID] = LegendItem{Title: o.ItemTitle, Hue: hue, Archived: o.ItemArchived}
	}
	for _, l := range seen {
		cal.Legend = append(cal.Legend, l)
	}
	sort.Slice(cal.Legend, func(i, j int) bool {
		if cal.Legend[i].Title != cal.Legend[j].Title {
			return cal.Legend[i].Title < cal.Legend[j].Title
		}
		return cal.Legend[i].Hue < cal.Legend[j].Hue
	})

	cal.Note = horizonNote(cal.Days, through, haveThrough, loc)

	if openID != "" {
		d, err := a.detail(ctx, openID, ym, loc, byParent[openID])
		switch {
		case err == nil:
			cal.Open = &d
		case errors.Is(err, store.ErrNotFound):
			// A stale link to a row that is gone: show the month without a panel.
		default:
			return Calendar{}, err
		}
	}
	return cal, nil
}

// horizonNote decides what, if anything, to say about where materialization
// ends. The boundary is read from kv.last_materialized_through, the slot
// /healthz reports horizon_days from, so the calendar and the dashboard cannot
// give two answers.
//
// It speaks only when the month reaches the boundary: a month fully inside the
// horizon has nothing to caveat. And it says what an empty cell means there,
// because "nothing scheduled" and "not generated yet" look identical on a grid.
func horizonNote(days []CalDay, through time.Time, have bool, loc *time.Location) HorizonNote {
	if len(days) == 0 {
		return HorizonNote{}
	}
	beyond, edge := 0, false
	for _, d := range days {
		if d.Beyond {
			beyond++
		}
		if d.Edge {
			edge = true
		}
	}
	if !have {
		if beyond == 0 {
			return HorizonNote{}
		}
		return HorizonNote{Kind: "unset", Text: "Nothing has been scheduled ahead yet: occurrences have not been generated, so empty days here say nothing about what is planned."}
	}
	at := through.In(loc).Format("Mon, Jan 2 15:04")
	switch {
	case beyond == len(days):
		return HorizonNote{Kind: "none", Text: "Past the horizon. Occurrences are generated through " + at + ", so nothing here exists yet. Empty does not mean nothing is planned."}
	case beyond > 0 || edge:
		return HorizonNote{Kind: "partial", Text: "Generated through " + at + ". Days after that are hatched: not generated yet, not empty."}
	}
	return HorizonNote{}
}

// detail loads the opened occurrence. It reads the one row and its item rather
// than searching the month, so a link to a row outside the month on screen
// still opens, and an archived item's title is whatever the item retained.
func (a *App) detail(ctx context.Context, id, month string, loc *time.Location, childID string) (Detail, error) {
	occ, err := a.store.GetOccurrence(ctx, id)
	if err != nil {
		return Detail{}, err
	}
	item, err := a.store.GetItem(ctx, occ.ItemID)
	if err != nil {
		return Detail{}, err
	}

	// The same Row the day view renders, from the same builder, so the buttons
	// offered and the POSTs behind them are the day view's and not a copy.
	row := rowFor(store.TodayOccurrence{
		ID:               occ.ID,
		ItemID:           occ.ItemID,
		ItemTitle:        item.Title,
		StartsAt:         occ.StartsAt,
		Status:           occ.Status,
		ResolvedAt:       occ.ResolvedAt,
		ResolutionSource: occ.ResolutionSource,
		NotifyPolicy:     item.NotifyPolicy,
		Priority:         item.Priority,
		SnoozeDepth:      occ.SnoozeDepth,
	}, loc)

	_, body := monthURLs(month, id)
	closePage, closeBody := monthURLs(month, "")
	d := Detail{
		Row:       row,
		Title:     item.Title,
		Hue:       itemHue(item.ID),
		When:      occ.StartsAt.In(loc).Format("Monday, January 2 · 15:04"),
		Archived:  item.ArchivedAt != nil,
		SettleURL: body,
		CloseHref: closePage,
		CloseBody: closeBody,
	}

	if occ.SnoozeDepth > 0 || occ.Status == domain.StatusSnoozed {
		chain, err := a.store.ChainFor(ctx, id)
		if err != nil {
			return Detail{}, err
		}
		orig := chain.ScheduledAt.In(loc).Format("Mon Jan 2 15:04")
		if occ.Status == domain.StatusSnoozed {
			d.Chain = "Snoozed. This reminder continues at a later time; originally due " + orig + "."
			if childID != "" {
				d.ChildHref, _ = monthURLs(month, childID)
			}
		} else {
			d.Chain = fmt.Sprintf("Snoozed ×%d. Originally due %s.", chain.SnoozeCount, orig)
		}
	}
	return d, nil
}

// parseMonth reads m=, defaulting to the current month in loc.
func parseMonth(m string, loc *time.Location) (time.Time, error) {
	if m == "" {
		now := time.Now().In(loc)
		return time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, loc), nil
	}
	t, err := time.ParseInLocation(MonthLayout, m, loc)
	if err != nil {
		return time.Time{}, fmt.Errorf("month %q is not YYYY-MM", m)
	}
	return t, nil
}

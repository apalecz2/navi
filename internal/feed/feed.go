// Package feed renders the read-only .ics calendar subscription (X1, D-018).
//
// What a feed contains is decided by one rule: inside the materialization
// horizon the occurrence rows are the truth, and past it the schedule is. A
// `fixed` item therefore exports as one recurring VEVENT whose rule carries the
// future, with every place the rows disagree with the rule written as an
// exception; every other kind exports one VEVENT per row, because their times
// are drawn at materialization (D-005) and a rule would assert times that do not
// exist yet.
//
// The exceptions, and why each is shaped as it is:
//
//   - skipped slot            RECURRENCE-ID override, STATUS:CANCELLED, so the
//     calendar shows it struck out and the rest of the series is untouched
//   - snoozed parent          EXDATE; the chain is drawn once, as its live link
//     (the calendar view's rule), which is a standalone VEVENT
//   - is_override single edit EXDATE on the slot it left, standalone VEVENT for
//     the moved row. The row keeps no record of the slot it came from, so an
//     override pairing would be a guess.
//   - pause, deleted pending  EXDATE: a slot with no row inside the horizon is a
//     slot that was removed. Past the horizon the pause ends are known and
//     slots before them are excluded too.
//   - DST gap or fold slot    EXDATE plus a standalone VEVENT at the instant the
//     materializer chose, because clients resolve a nonexistent wall clock their
//     own way (schedule.Instant documents ours).
//
// Time zones are not floating. Google reads floating times in the account's zone
// and Apple in the device's, but Navi fires at an absolute instant resolved in
// the device zone, so the feed resolves each item through schedule.Zones exactly
// as the materializer does and writes a TZID with a generated VTIMEZONE.
package feed

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/teambition/rrule-go"

	"github.com/aidenpaleczny/navi/internal/domain"
	"github.com/aidenpaleczny/navi/internal/ical"
	"github.com/aidenpaleczny/navi/internal/schedule"
	"github.com/aidenpaleczny/navi/internal/store"
)

// UIDDomain ends every UID. A UID has to be globally unique and never change for
// the life of the thing it names; the ids are ULIDs, which already are.
const UIDDomain = "@navi"

// Store is what the feed reads. The range read is the calendar view's own
// (session 21): there is no second "what occurrences are in this range" query.
type Store interface {
	ListCalendar(ctx context.Context, from, to time.Time, f store.CalendarFilter) ([]store.CalendarOccurrence, error)
	ListItems(ctx context.Context, filter store.ItemFilter, now time.Time) ([]domain.Item, error)
	GetItem(ctx context.Context, id string) (domain.Item, error)
	LastMaterializedThrough(ctx context.Context) (time.Time, bool, error)
	GlobalPauseUntil(ctx context.Context) (time.Time, bool, error)
	CurrentTZ(ctx context.Context) (string, bool, error)
}

// Feed renders and serves the subscription.
type Feed struct {
	log       *slog.Logger
	st        Store
	token     string
	defaultTZ *time.Location
	now       func() time.Time
}

// New builds a feed. token is the path token from secrets.Ensure.
func New(log *slog.Logger, st Store, token string, defaultTZ *time.Location) *Feed {
	return &Feed{log: log, st: st, token: token, defaultTZ: defaultTZ, now: time.Now}
}

// sequenceEpoch is where SEQUENCE starts counting minutes. A minute-resolution
// count of the last modification is monotonic with the edit, needs no stored
// counter, and fits an int32 for far longer than this project will run.
var sequenceEpoch = time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)

type builder struct {
	f       *Feed
	zones   schedule.Zones
	lo, hi  time.Time
	horizon time.Time // rows are authoritative up to here
	gpause  time.Time
	used    map[string]*time.Location
	events  []*ical.Component
}

// Build renders the feed for [lo, hi).
func (f *Feed) Build(ctx context.Context, zones schedule.Zones, lo, hi time.Time) ([]byte, error) {
	now := f.now()

	occs, err := f.st.ListCalendar(ctx, lo, hi, store.CalendarFilter{})
	if err != nil {
		return nil, fmt.Errorf("feed: %w", err)
	}
	through, haveThrough, err := f.st.LastMaterializedThrough(ctx)
	if err != nil {
		return nil, fmt.Errorf("feed: %w", err)
	}
	gpause, _, err := f.st.GlobalPauseUntil(ctx)
	if err != nil {
		return nil, fmt.Errorf("feed: %w", err)
	}
	live, err := f.st.ListItems(ctx, store.FilterAll, now)
	if err != nil {
		return nil, fmt.Errorf("feed: %w", err)
	}

	items := make(map[string]domain.Item, len(live))
	for _, it := range live {
		items[it.ID] = it
	}
	rows := map[string][]store.CalendarOccurrence{}
	for _, o := range occs {
		rows[o.ItemID] = append(rows[o.ItemID], o)
		if _, ok := items[o.ItemID]; !ok {
			// History of an archived item: its rows still belong in the past.
			it, err := f.st.GetItem(ctx, o.ItemID)
			if err != nil {
				return nil, fmt.Errorf("feed: %w", err)
			}
			items[o.ItemID] = it
		}
	}

	// Rows are authoritative up to the horizon, and at least up to now: a slot in
	// the past with no row is a removed slot, whether or not anything has ever
	// materialized.
	horizon := now
	if haveThrough && through.After(horizon) {
		horizon = through
	}
	b := &builder{f: f, zones: zones, lo: lo, hi: hi, horizon: horizon, gpause: gpause,
		used: map[string]*time.Location{}}

	ids := make([]string, 0, len(items))
	for id := range items {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		b.item(items[id], rows[id])
	}

	cal := ical.New("VCALENDAR")
	cal.Add("VERSION", "2.0")
	cal.Add("PRODID", "-//Navi//Calendar Feed//EN")
	cal.Add("CALSCALE", "GREGORIAN")
	cal.Add("METHOD", "PUBLISH")
	cal.AddText("X-WR-CALNAME", "Navi")
	cal.Add("X-PUBLISHED-TTL", "PT1H")
	cal.Add("REFRESH-INTERVAL", "PT1H", ical.Param{Name: "VALUE", Value: "DURATION"})
	if haveThrough {
		// Everything not a fixed series stops here; a subscriber reading an empty
		// stretch can tell "not generated yet" from "nothing scheduled".
		cal.Add("X-NAVI-MATERIALIZED-THROUGH", utcStamp(through))
	}

	names := make([]string, 0, len(b.used))
	for n := range b.used {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		cal.Subs = append(cal.Subs, vtimezone(b.used[n], lo, hi))
	}
	cal.Subs = append(cal.Subs, b.events...)

	return ical.Encode(cal), nil
}

// stamp is when a component last changed, from what the feed can read: the item's
// last edit and the occurrence's resolution.
func stamp(it domain.Item, o *store.CalendarOccurrence) time.Time {
	t := it.UpdatedAt
	if o != nil && o.ResolvedAt != nil && o.ResolvedAt.After(t) {
		t = *o.ResolvedAt
	}
	if t.IsZero() {
		t = it.CreatedAt
	}
	return t
}

func (b *builder) vevent(it domain.Item, uid string, at time.Time) *ical.Component {
	e := ical.New("VEVENT")
	e.Add("UID", uid)
	e.Add("DTSTAMP", utcStamp(at))
	e.Add("LAST-MODIFIED", utcStamp(at))
	seq := int(at.Sub(sequenceEpoch) / time.Minute)
	if seq < 0 {
		seq = 0
	}
	e.Add("SEQUENCE", strconv.Itoa(seq))
	e.AddText("SUMMARY", titleOf(it))
	if it.Notes != nil && strings.TrimSpace(*it.Notes) != "" {
		e.AddText("DESCRIPTION", *it.Notes)
	}
	// A reminder is an instant: no DTEND is a zero-duration event (RFC 5545
	// 3.6.1), and TRANSPARENT keeps it out of free/busy.
	e.Add("TRANSP", "TRANSPARENT")
	return e
}

func titleOf(it domain.Item) string {
	if t := strings.TrimSpace(ical.Clean(it.Title)); t != "" {
		return t
	}
	return "Reminder"
}

// dt writes a date-time property in the zone's form: TZID for a named zone, Z
// for UTC.
func dt(e *ical.Component, name string, at time.Time, loc *time.Location) {
	if loc == nil || isUTC(loc) {
		e.Add(name, utcStamp(at))
		return
	}
	e.Add(name, localStamp(at.In(loc)), ical.Param{Name: "TZID", Value: loc.String()})
}

func (b *builder) pauseEnd(it domain.Item) time.Time {
	end := b.gpause
	if it.PausedUntil != nil && it.PausedUntil.After(end) {
		end = *it.PausedUntil
	}
	return end
}

func (b *builder) item(it domain.Item, occs []store.CalendarOccurrence) {
	live := it.Active && it.ArchivedAt == nil
	sched, err := schedule.Parse(it.Schedule)
	parsed := err == nil
	if err != nil {
		b.f.log.Warn("feed: item schedule does not parse; exporting its rows only", "item", it.ID, "err", err)
	}

	claimed := map[string]bool{}
	oneOff := parsed && sched.Kind == schedule.KindOneOff
	if live && parsed {
		switch sched.Kind {
		case schedule.KindFixed:
			b.fixed(it, sched, occs, claimed)
		case schedule.KindOneOff:
			b.synthOneOff(it, sched, occs)
		}
	}

	for i := range occs {
		o := &occs[i]
		if claimed[o.ID] || o.Status == domain.StatusSnoozed {
			continue
		}
		uid := o.ID + UIDDomain
		if oneOff && o.ParentOccurrenceID == nil {
			// The one-off's own row keeps the item's UID, so the event a subscriber
			// holds from before it was materialized is updated and not duplicated.
			uid = it.ID + UIDDomain
		}
		b.standalone(it, o, uid, o.StartsAt)
	}
}

// standalone writes one VEVENT for one row at its instant, in UTC. A drawn time
// is absolute, and absolute is what UTC says.
func (b *builder) standalone(it domain.Item, o *store.CalendarOccurrence, uid string, at time.Time) {
	e := b.vevent(it, uid, stamp(it, o))
	dt(e, "DTSTART", at, time.UTC)
	switch o.Status {
	case domain.StatusSkipped:
		e.Add("STATUS", "CANCELLED")
	default:
		e.Add("STATUS", "CONFIRMED")
		if o.Status != domain.StatusPending && o.Status != domain.StatusNotified {
			e.AddText("DESCRIPTION", statusLine(it, o.Status))
		}
	}
	b.events = append(b.events, e)
}

func statusLine(it domain.Item, s domain.Status) string {
	line := "Status: " + string(s)
	if it.Notes != nil && strings.TrimSpace(*it.Notes) != "" {
		return *it.Notes + "\n" + line
	}
	return line
}

// synthOneOff exports a one-off that has not been materialized yet, straight
// from its schedule, so a reminder set for next quarter is on the phone today.
func (b *builder) synthOneOff(it domain.Item, s schedule.Schedule, occs []store.CalendarOccurrence) {
	for _, o := range occs {
		if o.ParentOccurrenceID == nil {
			return // the row is the truth, and standalone() will write it
		}
	}
	loc, err := b.zones.For(it)
	if err != nil {
		return
	}
	d, err := s.OneOffAt()
	if err != nil {
		return
	}
	at, _ := schedule.Instant(d, loc)
	if at.Before(b.lo) || !at.Before(b.hi) || !at.After(b.horizon) || at.Before(b.pauseEnd(it)) {
		return
	}
	e := b.vevent(it, it.ID+UIDDomain, stamp(it, nil))
	dt(e, "DTSTART", at, time.UTC)
	e.Add("STATUS", "CONFIRMED")
	b.events = append(b.events, e)
}

type slotAt struct {
	date time.Time // midnight UTC of the local date, as the rule yields it
	at   time.Time
	fold schedule.Fold
}

// fixed writes the recurring master and its exceptions, marking in claimed every
// row it accounts for so the caller does not write it a second time.
func (b *builder) fixed(it domain.Item, s schedule.Schedule, occs []store.CalendarOccurrence, claimed map[string]bool) {
	skip := func(why string, err error) {
		b.f.log.Warn("feed: fixed item exported as plain events: "+why, "item", it.ID, "err", err)
	}
	if s.RRule == nil {
		return
	}
	loc, err := b.zones.For(it)
	if err != nil {
		skip("no zone", err)
		return
	}
	tod, err := s.TimeOfDay()
	if err != nil {
		skip("no time of day", err)
		return
	}
	rule := strings.TrimPrefix(strings.TrimSpace(*s.RRule), "RRULE:")
	r, err := rrule.StrToRRule(rule)
	if err != nil {
		skip("rrule does not parse", err)
		return
	}

	// The same anchor the materializer expands from (expand.go dates), so the
	// feed's slots are the rows' slots.
	utcDate := func(t time.Time) time.Time {
		y, m, d := t.In(loc).Date()
		return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
	}
	r.DTStart(utcDate(it.CreatedAt))

	finite := r.OrigOptions.Count > 0 || !r.OrigOptions.Until.IsZero()
	var dates []time.Time
	var last time.Time
	if finite {
		all := r.All()
		if len(all) == 0 {
			return
		}
		dates = all
		l := all[len(all)-1]
		last, _ = schedule.Instant(schedule.Combine(l.Year(), l.Month(), l.Day(), tod), loc)
	} else {
		dates = r.Between(utcDate(b.lo), utcDate(b.hi.Add(-time.Second)), true)
	}

	var slots []slotAt
	for _, d := range dates {
		at, fold := schedule.Instant(schedule.Combine(d.Year(), d.Month(), d.Day(), tod), loc)
		if at.Before(b.lo) || !at.Before(b.hi) {
			continue
		}
		slots = append(slots, slotAt{date: d, at: at, fold: fold})
	}
	if len(slots) == 0 {
		return
	}
	sort.Slice(slots, func(i, j int) bool { return slots[i].at.Before(slots[j].at) })

	// Rows that could sit on the rule: the chain roots. A snooze child is never a
	// slot of the rule, whatever its instant.
	byInstant := map[int64]*store.CalendarOccurrence{}
	for i := range occs {
		if occs[i].ParentOccurrenceID == nil {
			byInstant[occs[i].StartsAt.Unix()] = &occs[i]
		}
	}

	wall := func(sl slotAt) time.Time {
		return time.Date(sl.date.Year(), sl.date.Month(), sl.date.Day(), tod.Hour, tod.Minute, 0, 0, time.UTC)
	}
	pauseEnd := b.pauseEnd(it)
	uid := it.ID + UIDDomain
	at := stamp(it, nil)

	var exdates []string
	var cancelled []*ical.Component
	exclude := func(sl slotAt) { exdates = append(exdates, localStamp(wall(sl))) }

	for _, sl := range slots {
		row := byInstant[sl.at.Unix()]

		if sl.fold != schedule.FoldNone {
			// Clients and this system disagree about this wall clock. Take the slot
			// out of the rule and write the instant we chose.
			exclude(sl)
			if row == nil && sl.at.After(b.horizon) && !sl.at.Before(pauseEnd) {
				e := b.vevent(it, fmt.Sprintf("%s-%s%s", it.ID, sl.date.Format("20060102"), UIDDomain), at)
				dt(e, "DTSTART", sl.at, time.UTC)
				e.Add("STATUS", "CONFIRMED")
				b.events = append(b.events, e)
			}
			if row != nil && row.Status == domain.StatusSnoozed {
				claimed[row.ID] = true
			}
			continue
		}

		switch {
		case row != nil && row.Status == domain.StatusSkipped:
			claimed[row.ID] = true
			e := b.vevent(it, uid, stamp(it, row))
			e.Add("RECURRENCE-ID", localStamp(wall(sl)), tzParam(loc)...)
			e.Add("DTSTART", localStamp(wall(sl)), tzParam(loc)...)
			e.Add("STATUS", "CANCELLED")
			cancelled = append(cancelled, e)
		case row != nil && row.Status == domain.StatusSnoozed:
			claimed[row.ID] = true
			exclude(sl)
		case row != nil:
			claimed[row.ID] = true
		case !sl.at.After(b.horizon):
			exclude(sl) // inside the horizon and no row: a pause or a removal
		case sl.at.Before(pauseEnd):
			exclude(sl) // beyond it, a pause window that has not been materialized
		}
	}

	m := b.vevent(it, uid, at)
	first := wall(slots[0])
	m.Add("DTSTART", localStamp(first), tzParam(loc)...)
	if finite {
		rule = withoutCountUntil(rule) + ";UNTIL=" + utcStamp(last)
	}
	m.Add("RRULE", rule)
	if len(exdates) > 0 {
		m.Add("EXDATE", strings.Join(exdates, ","), tzParam(loc)...)
	}
	m.Add("STATUS", "CONFIRMED")
	if !isUTC(loc) {
		b.used[loc.String()] = loc
	}
	b.events = append(b.events, m)
	b.events = append(b.events, cancelled...)
}

func tzParam(loc *time.Location) []ical.Param {
	if isUTC(loc) {
		return nil
	}
	return []ical.Param{{Name: "TZID", Value: loc.String()}}
}

// withoutCountUntil drops the two terminators from a rule, leaving the rest in
// order. The feed's DTSTART slides with the requested range, and COUNT counts
// from DTSTART, so a counted rule is re-expressed as the UNTIL it implies.
func withoutCountUntil(rule string) string {
	var keep []string
	for _, p := range strings.Split(rule, ";") {
		k := strings.ToUpper(strings.SplitN(p, "=", 2)[0])
		if k == "COUNT" || k == "UNTIL" || p == "" {
			continue
		}
		keep = append(keep, p)
	}
	return strings.Join(keep, ";")
}

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/teambition/rrule-go"

	"github.com/aidenpaleczny/navi/internal/config"
	"github.com/aidenpaleczny/navi/internal/domain"
	"github.com/aidenpaleczny/navi/internal/feed"
	"github.com/aidenpaleczny/navi/internal/health"
	"github.com/aidenpaleczny/navi/internal/httpapi"
	"github.com/aidenpaleczny/navi/internal/ical"
	"github.com/aidenpaleczny/navi/internal/materializer"
	"github.com/aidenpaleczny/navi/internal/metrics"
	"github.com/aidenpaleczny/navi/internal/schedule"
	"github.com/aidenpaleczny/navi/internal/secrets"
	"github.com/aidenpaleczny/navi/internal/store"
)

// icsCal is a parsed feed with the lookups the assertions use.
type icsCal struct {
	root   *ical.Component
	events []*ical.Component
	byUID  map[string][]*ical.Component
	zones  map[string]*ical.Component
}

func parseICS(data []byte) (icsCal, error) {
	root, err := ical.Parse(data)
	if err != nil {
		return icsCal{}, err
	}
	c := icsCal{root: root, byUID: map[string][]*ical.Component{}, zones: map[string]*ical.Component{}}
	for _, s := range root.Subs {
		switch s.Name {
		case "VEVENT":
			c.events = append(c.events, s)
			if p, ok := s.Get("UID"); ok {
				c.byUID[p.Value] = append(c.byUID[p.Value], s)
			}
		case "VTIMEZONE":
			if p, ok := s.Get("TZID"); ok {
				c.zones[p.Value] = s
			}
		}
	}
	return c, nil
}

func prop(e *ical.Component, name string) string {
	p, _ := e.Get(name)
	return p.Value
}

// master is the series VEVENT for a UID: the one with an RRULE.
func (c icsCal) master(uid string) *ical.Component {
	for _, e := range c.byUID[uid] {
		if _, ok := e.Get("RRULE"); ok {
			return e
		}
	}
	return nil
}

// override is the RECURRENCE-ID VEVENT of a series at a wall clock.
func (c icsCal) override(uid, wall string) *ical.Component {
	for _, e := range c.byUID[uid] {
		if p, ok := e.Get("RECURRENCE-ID"); ok && p.Value == wall {
			return e
		}
	}
	return nil
}

// plain is the standalone VEVENT for a UID: no RRULE, no RECURRENCE-ID.
func (c icsCal) plain(uid string) *ical.Component {
	for _, e := range c.byUID[uid] {
		_, r := e.Get("RRULE")
		_, id := e.Get("RECURRENCE-ID")
		if !r && !id {
			return e
		}
	}
	return nil
}

func (c icsCal) exdates(m *ical.Component) map[string]bool {
	out := map[string]bool{}
	if m == nil {
		return out
	}
	for _, p := range m.All("EXDATE") {
		for _, v := range strings.Split(p.Value, ",") {
			out[v] = true
		}
	}
	return out
}

// reportICS drives the calendar feed (P6, X1/X2, D-018): one item of each kind,
// a skip, an override, a snooze child and a pause, encoded, fetched through a
// real httpapi handler, and parsed back.
//
// Expectations are written from a hand table of day offsets, not from a feed
// call: the fixed series' exceptions are a function of what was done to which
// day, and a section that derived them from the feed would only prove the feed
// agrees with itself. The fixed item lives in America/Toronto in fixed mode so
// its wall clocks do not move with the device zone reportZones sets earlier in
// the run.
func reportICS(ctx context.Context, st *store.Store, fallback *time.Location, log *slog.Logger) error {
	fmt.Println("\nics  GET /calendar/{token}.ics")

	zones, err := schedule.LoadZones(ctx, st, fallback)
	if err != nil {
		return err
	}
	mat := materializer.New(log.With("component", "mat-ics"), st, fallback)
	tor, err := time.LoadLocation("America/Toronto")
	if err != nil {
		return err
	}
	runID := strconv.FormatInt(time.Now().Unix(), 36)

	// 1. The token, from the D10 helper.
	dir, err := os.MkdirTemp("", "naviseed-secrets-")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	token, err := secrets.Ensure(dir, secrets.CalendarTokenFile)
	if err != nil {
		return err
	}
	again, err := secrets.Ensure(dir, secrets.CalendarTokenFile)
	if err != nil {
		return err
	}
	if err := os.Remove(filepath.Join(dir, secrets.CalendarTokenFile)); err != nil {
		return err
	}
	rotated, err := secrets.Ensure(dir, secrets.CalendarTokenFile)
	if err != nil {
		return err
	}
	fmt.Printf("  token  %d chars, stable across restarts=%v, deleting the file rotates it=%v  %s\n",
		len(token), token == again, rotated != token,
		verdict(len(token) >= 43 && token == again && rotated != token))
	token = rotated

	// 2. Fixtures.
	today := time.Now().In(tor)
	y, mo, d0 := today.Date()
	dayOf := func(off int) time.Time { return time.Date(y, mo, d0+off, 0, 0, 0, 0, time.UTC) }
	wall := func(off, h, m int) string {
		return fmt.Sprintf("%sT%02d%02d00", dayOf(off).Format("20060102"), h, m)
	}
	instant := func(off, h, m int) time.Time {
		dd := dayOf(off)
		at, _ := schedule.Instant(schedule.Combine(dd.Year(), dd.Month(), dd.Day(), schedule.LocalTime{Hour: h, Minute: m}), tor)
		return at
	}
	fixedMode := domain.TZModeFixed
	mk := func(title, sched string, mode *domain.TZMode) (domain.Item, error) {
		return st.CreateItem(ctx, domain.NewItem{Title: title, Schedule: json.RawMessage(sched),
			TZ: "America/Toronto", TZMode: mode})
	}

	series, err := mk("ics fixed series "+runID, `{"kind":"fixed","rrule":"FREQ=DAILY","at":"08:30"}`, &fixedMode)
	if err != nil {
		return err
	}
	// The snoozed parent exists before the first materialization, notified, on
	// its slot: the materializer keeps the row that already fills a slot.
	notified := domain.StatusNotified
	parent, err := st.CreateOccurrence(ctx, domain.NewOccurrence{
		ItemID: series.ID, StartsAt: instant(6, 8, 30), Status: &notified})
	if err != nil {
		return err
	}
	if _, err := mat.Item(ctx, series.ID); err != nil {
		return err
	}
	rows, err := st.ListOccurrencesForItem(ctx, series.ID)
	if err != nil {
		return err
	}
	rowAt := func(off int) (domain.Occurrence, bool) {
		for _, r := range rows {
			if r.StartsAt.Equal(instant(off, 8, 30)) {
				return r, true
			}
		}
		return domain.Occurrence{}, false
	}
	atSix := 0
	for _, r := range rows {
		if r.StartsAt.Equal(instant(6, 8, 30)) {
			atSix++
		}
	}

	skipRow, okSkip := rowAt(2)
	moveRow, okMove := rowAt(4)
	if !okSkip || !okMove || atSix != 1 {
		return fmt.Errorf("naviseed: ics: fixture rows missing (skip=%v move=%v rows at day 6=%d)", okSkip, okMove, atSix)
	}
	now := time.Now()
	if _, err := st.ResolveOccurrence(ctx, skipRow.ID, domain.StatusSkipped, nil, domain.ResolvedByWeb, now); err != nil {
		return err
	}
	moved := moveRow.StartsAt.Add(3 * time.Hour)
	if _, err := st.UpdateOccurrenceOverride(ctx, moveRow.ID, series.ID, moved, now); err != nil {
		return err
	}
	snoozeTo := parent.StartsAt.Add(90 * time.Minute)
	sn, err := st.SnoozeOccurrence(ctx, parent.ID, domain.ResolvedByWeb, now,
		func(domain.Item, domain.Occurrence) (time.Time, error) { return snoozeTo, nil })
	if err != nil {
		return err
	}
	// A pause through the end of day 10 deletes the pending rows before it and
	// touches nothing else: skipped, notified and override rows survive.
	until := time.Date(y, mo, d0+11, 0, 0, 0, 0, tor)
	if _, _, err := mat.Pause(ctx, series.ID, &until); err != nil {
		return err
	}

	// Other kinds.
	windowed, err := seedByTitle(ctx, st, "ics windowed probe", `{"kind":"windowed","rrule":"FREQ=DAILY","window":["09:00","17:00"]}`, "America/Toronto")
	if err != nil {
		return err
	}
	fuzzy, err := seedByTitle(ctx, st, "ics fuzzy probe", specExamples[3], "America/Toronto")
	if err != nil {
		return err
	}
	hostileTitle := "ics hostile, \"title\"; with \\ back\x00slash\x1b esc \xff bad byte\nsecond line 日本語日本語日本語日本語日本語日本語日本語日本語日本語日本語 end"
	hostile, err := seedByTitle(ctx, st, hostileTitle, `{"kind":"windowed","rrule":"FREQ=DAILY","window":["10:00","11:00"]}`, "America/Toronto")
	if err != nil {
		return err
	}
	nearDay, farDay := dayOf(10), dayOf(60)
	near, err := mk("ics one-off near "+runID, fmt.Sprintf(`{"kind":"one_off","at":"%sT10:00:00"}`, nearDay.Format("2006-01-02")), &fixedMode)
	if err != nil {
		return err
	}
	far, err := mk("ics one-off far "+runID, fmt.Sprintf(`{"kind":"one_off","at":"%sT10:00:00"}`, farDay.Format("2006-01-02")), &fixedMode)
	if err != nil {
		return err
	}
	counted, err := mk("ics counted "+runID, `{"kind":"fixed","rrule":"FREQ=DAILY;COUNT=5","at":"07:15"}`, nil) // floating
	if err != nil {
		return err
	}
	dst, err := mk("ics dst probe "+runID, `{"kind":"fixed","rrule":"FREQ=DAILY","at":"02:30"}`, &fixedMode)
	if err != nil {
		return err
	}
	for _, it := range []domain.Item{windowed, fuzzy, hostile, near, far, counted, dst} {
		if _, err := mat.Item(ctx, it.ID); err != nil {
			return err
		}
	}

	// 3. Serve it through a real handler. The log captures everything the server
	// writes, to prove the token is not in it.
	var logbuf bytes.Buffer
	srvLog := slog.New(slog.NewTextHandler(&logbuf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	cal := feed.New(srvLog, st, token, fallback)
	srv := httpapi.New(config.HTTP{Addr: ":0"}, srvLog, health.New(), metrics.New(), st, mat, time.Time{},
		fallback, nil, nil, nil, cal)
	get := func(path string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		srv.Handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil).WithContext(ctx))
		return rec
	}

	good := get("/calendar/" + token + ".ics")
	unknown := get("/definitely-not-a-route")
	wrongs := []*httptest.ResponseRecorder{
		get("/calendar/" + token[:len(token)-1] + "x.ics"), // one character off
		get("/calendar/" + token),                          // no extension
		get("/calendar/.ics"),                              // empty
		get("/calendar/" + strings.ToUpper(token) + ".ics"),
	}
	all404 := true
	for _, rec := range wrongs {
		if rec.Code != http.StatusNotFound || rec.Body.String() != unknown.Body.String() {
			all404 = false
		}
	}
	badRange := get("/calendar/" + token + ".ics?from=2026-02-30")
	fmt.Printf("  auth  right token %d %q  wrong tokens 404 with the unknown-path body=%v  bad range after a good token %d  %s\n",
		good.Code, good.Header().Get("Content-Type"), all404, badRange.Code,
		verdict(good.Code == 200 && strings.HasPrefix(good.Header().Get("Content-Type"), "text/calendar") &&
			all404 && badRange.Code == 400))
	logged := logbuf.String()
	fmt.Printf("  log   token absent from every log line=%v  path redacted=%v  %s\n",
		!strings.Contains(logged, token), strings.Contains(logged, "/calendar/[redacted]"),
		verdict(!strings.Contains(logged, token) && strings.Contains(logged, "/calendar/[redacted]")))

	data := good.Body.Bytes()
	c, err := parseICS(data)
	if err != nil {
		fmt.Printf("  parse  %v  %s\n", err, verdict(false))
		return nil
	}

	// 4. Structure: the whole feed, including every other section's items.
	bad := 0
	for _, e := range c.events {
		if prop(e, "UID") == "" || !strings.HasSuffix(prop(e, "DTSTAMP"), "Z") || prop(e, "DTSTART") == "" {
			bad++
		}
		if _, ok := e.Get("DTEND"); ok {
			bad++
		}
		if _, ok := e.Get("DURATION"); ok {
			bad++
		}
		if n, err := strconv.Atoi(prop(e, "SEQUENCE")); err != nil || n < 0 {
			bad++
		}
	}
	rawBad := 0
	for _, b := range bytes.ReplaceAll(data, []byte("\r\n"), nil) {
		if b < 0x20 && b != '\t' || b == 0x7f || b == 0xff {
			rawBad++
		}
	}
	hdr := prop(c.root, "VERSION") == "2.0" && prop(c.root, "PRODID") != "" && prop(c.root, "X-PUBLISHED-TTL") == "PT1H" &&
		prop(c.root, "METHOD") == "PUBLISH"
	fmt.Printf("  parse  %d bytes, %d events, %d time zones; header ok=%v; malformed events=%d; control or invalid bytes=%d; valid UTF-8=%v  %s\n",
		len(data), len(c.events), len(c.zones), hdr, bad, rawBad, utf8.Valid(data),
		verdict(hdr && bad == 0 && rawBad == 0 && utf8.Valid(data)))

	// 5. The fixed series and its four kinds of exception.
	uid := series.ID + feed.UIDDomain
	m := c.master(uid)
	if m == nil {
		fmt.Printf("  series  no master VEVENT for the fixed item  %s\n", verdict(false))
		return nil
	}
	ex := c.exdates(m)
	wantEx := map[string]bool{}
	for _, off := range []int{0, 1, 3, 4, 5, 6, 7, 8, 9, 10} {
		wantEx[wall(off, 8, 30)] = true
	}
	exOK := len(ex) == len(wantEx)
	for k := range wantEx {
		exOK = exOK && ex[k]
	}
	dtp, _ := m.Get("DTSTART")
	rr := prop(m, "RRULE")
	_, parses := rrule.StrToRRule(rr)
	fmt.Printf("  series  RRULE %q survives=%v  DTSTART %s TZID=%s  parses=%v\n",
		rr, rr == "FREQ=DAILY", dtp.Value, dtp.Param("TZID"), parses == nil)
	fmt.Printf("  series  EXDATE = pause + move + snooze, not the skip: %d of %d wanted, none extra  %s\n",
		len(ex), len(wantEx), verdict(exOK && rr == "FREQ=DAILY" && dtp.Param("TZID") == "America/Toronto" && parses == nil))
	var beyond []string
	for k := range ex {
		if k > wall(10, 8, 30) {
			beyond = append(beyond, k)
		}
	}
	fmt.Printf("  series  rest intact: no exception after the pause ends=%v, rule runs on past the %d-day horizon (no UNTIL or COUNT)=%v  %s\n",
		len(beyond) == 0, 30, !strings.Contains(rr, "UNTIL") && !strings.Contains(rr, "COUNT"),
		verdict(len(beyond) == 0 && !strings.Contains(rr, "UNTIL") && !strings.Contains(rr, "COUNT")))

	cancel := c.override(uid, wall(2, 8, 30))
	fmt.Printf("  skip    instance %s: override found=%v STATUS=%s DTSTART=%s, series unaffected (master STATUS=%s)  %s\n",
		wall(2, 8, 30), cancel != nil, func() string {
			if cancel == nil {
				return "-"
			}
			return prop(cancel, "STATUS")
		}(), func() string {
			if cancel == nil {
				return "-"
			}
			return prop(cancel, "DTSTART")
		}(), prop(m, "STATUS"),
		verdict(cancel != nil && prop(cancel, "STATUS") == "CANCELLED" && prop(cancel, "DTSTART") == wall(2, 8, 30) && prop(m, "STATUS") == "CONFIRMED"))

	moveEv := c.plain(moveRow.ID + feed.UIDDomain)
	fmt.Printf("  edit    slot left the rule=%v; moved row is standalone at %s (want %s)  %s\n",
		ex[wall(4, 8, 30)], func() string {
			if moveEv == nil {
				return "-"
			}
			return prop(moveEv, "DTSTART")
		}(), moved.UTC().Format("20060102T150405Z"),
		verdict(ex[wall(4, 8, 30)] && moveEv != nil && prop(moveEv, "DTSTART") == moved.UTC().Format("20060102T150405Z")))

	childEv := c.plain(sn.Child.ID + feed.UIDDomain)
	fmt.Printf("  snooze  parent hidden=%v and its slot excluded=%v; child standalone at %s (want %s), drawn once  %s\n",
		c.plain(parent.ID+feed.UIDDomain) == nil && len(c.byUID[parent.ID+feed.UIDDomain]) == 0, ex[wall(6, 8, 30)],
		func() string {
			if childEv == nil {
				return "-"
			}
			return prop(childEv, "DTSTART")
		}(), snoozeTo.UTC().Format("20060102T150405Z"),
		verdict(len(c.byUID[parent.ID+feed.UIDDomain]) == 0 && ex[wall(6, 8, 30)] && childEv != nil &&
			prop(childEv, "DTSTART") == snoozeTo.UTC().Format("20060102T150405Z")))

	pausedAll := true
	for _, off := range []int{1, 3, 5, 7, 8, 9, 10} {
		pausedAll = pausedAll && ex[wall(off, 8, 30)]
	}
	fmt.Printf("  pause   every deleted pending slot through day 10 is excluded=%v, day 11 is not=%v  %s\n",
		pausedAll, !ex[wall(11, 8, 30)], verdict(pausedAll && !ex[wall(11, 8, 30)]))

	tz := c.zones["America/Toronto"]
	var kinds []string
	if tz != nil {
		seen := map[string]bool{}
		for _, s := range tz.Subs {
			if !seen[s.Name] {
				seen[s.Name] = true
				kinds = append(kinds, s.Name)
			}
		}
	}
	sort.Strings(kinds)
	fmt.Printf("  vtimezone  America/Toronto defined=%v observances=%v  %s\n",
		tz != nil, kinds, verdict(tz != nil && strings.Join(kinds, ",") == "DAYLIGHT,STANDARD"))

	// 6. Drawn times for windowed and fuzzy: the feed's instant is the row's.
	for _, it := range []domain.Item{windowed, fuzzy} {
		lo, hi := now.AddDate(0, 0, -30), now.AddDate(0, 0, 90)
		occs, err := st.ListCalendar(ctx, lo, hi, store.CalendarFilter{ItemID: it.ID})
		if err != nil {
			return err
		}
		match, flat, n := 0, true, 0
		for _, o := range occs {
			if o.Status == domain.StatusSnoozed {
				continue
			}
			n++
			e := c.plain(o.ID + feed.UIDDomain)
			if e != nil && prop(e, "DTSTART") == o.StartsAt.UTC().Format("20060102T150405Z") {
				match++
			}
			if e != nil {
				if _, ok := e.Get("RRULE"); ok {
					flat = false
				}
			}
		}
		// The materializer's horizon is 30 days from the run; the section may run
		// alone, where kv.last_materialized_through has never been written, so the
		// bound is the horizon itself with a day of slack.
		bound := now.AddDate(0, 0, 31).UTC().Format("20060102T150405Z")
		last := ""
		for _, o := range occs {
			if s := o.StartsAt.UTC().Format("20060102T150405Z"); s > last {
				last = s
			}
		}
		fmt.Printf("  drawn   %-20s %d rows, %d events at the row's exact instant, none recurring=%v, nothing past the horizon (last %s)  %s\n",
			it.Title, n, match, flat, last,
			verdict(n > 0 && match == n && flat && last <= bound))
	}

	// 7. One-offs: a row keeps the item's UID, an unmaterialized one is written
	// from its schedule and takes the same UID when it later materializes.
	nearRows, _ := st.ListOccurrencesForItem(ctx, near.ID)
	nearEv := c.plain(near.ID + feed.UIDDomain)
	farEv := c.plain(far.ID + feed.UIDDomain)
	farRows, _ := st.ListOccurrencesForItem(ctx, far.ID)
	wantFar := instant(60, 10, 0).UTC().Format("20060102T150405Z")
	fmt.Printf("  one_off  near: row=%d event at %s (want %s)   far: rows=%d event at %s (want %s), no event under the row id  %s\n",
		len(nearRows), func() string {
			if nearEv == nil {
				return "-"
			}
			return prop(nearEv, "DTSTART")
		}(), instant(10, 10, 0).UTC().Format("20060102T150405Z"), len(farRows), func() string {
			if farEv == nil {
				return "-"
			}
			return prop(farEv, "DTSTART")
		}(), wantFar,
		verdict(len(nearRows) == 1 && nearEv != nil && prop(nearEv, "DTSTART") == instant(10, 10, 0).UTC().Format("20060102T150405Z") &&
			len(farRows) == 0 && farEv != nil && prop(farEv, "DTSTART") == wantFar &&
			len(c.byUID[nearRows[0].ID+feed.UIDDomain]) == 0))

	// 8. A counted rule is re-expressed, not truncated.
	cm := c.master(counted.ID + feed.UIDDomain)
	if cm == nil {
		fmt.Printf("  counted  no master  %s\n", verdict(false))
	} else {
		czone, _ := zones.For(counted)
		cdt, _ := cm.Get("DTSTART")
		crule := prop(cm, "RRULE")
		r, err := rrule.StrToRRule(crule)
		n := 0
		if err == nil {
			start, perr := time.ParseInLocation("20060102T150405", cdt.Value, czone)
			if perr == nil {
				r.DTStart(start)
				n = len(r.All())
			}
		}
		fmt.Printf("  counted  RRULE %q (floating item -> TZID %s, device zone %s), expands to %d  %s\n",
			crule, cdt.Param("TZID"), czone, n,
			verdict(!strings.Contains(crule, "COUNT") && strings.Contains(crule, "UNTIL=") && n == 5 &&
				cdt.Param("TZID") == czone.String() && c.zones[czone.String()] != nil))
	}

	// 9. DST: a nonexistent wall clock, in a stretch past the horizon where only
	// the rule speaks. Built directly, because the default range ends in January.
	lo := time.Date(2027, 3, 1, 0, 0, 0, 0, tor)
	hi := time.Date(2027, 3, 20, 0, 0, 0, 0, tor)
	dstData, err := cal.Build(ctx, zones, lo, hi)
	if err != nil {
		return err
	}
	dc, err := parseICS(dstData)
	if err != nil {
		return err
	}
	dm := dc.master(dst.ID + feed.UIDDomain)
	gapUID := fmt.Sprintf("%s-20270314%s", dst.ID, feed.UIDDomain)
	gapEv := dc.plain(gapUID)
	want := time.Date(2027, 3, 14, 7, 0, 0, 0, time.UTC).Format("20060102T150405Z")
	dex := dc.exdates(dm)
	fmt.Printf("  dst     02:30 on 2027-03-14 does not exist in Toronto: slot excluded=%v, standalone at %s (want %s, the first valid time after the gap); 03-13 and 03-15 left to the rule=%v  %s\n",
		dex["20270314T023000"], func() string {
			if gapEv == nil {
				return "-"
			}
			return prop(gapEv, "DTSTART")
		}(), want, !dex["20270313T023000"] && !dex["20270315T023000"],
		verdict(dm != nil && dex["20270314T023000"] && gapEv != nil && prop(gapEv, "DTSTART") == want &&
			!dex["20270313T023000"] && !dex["20270315T023000"]))

	// 10. Re-materializing and editing update in place. The identity of an event
	// is its UID, and nothing in either path is allowed to change one.
	// Over the whole feed, other sections' items included: nothing a section did
	// before this one may be unstable either.
	identities := func(c icsCal) map[string]string {
		out := map[string]string{}
		for uid, es := range c.byUID {
			for _, e := range es {
				key := uid + "|" + prop(e, "RECURRENCE-ID")
				out[key] = prop(e, "DTSTART") + "|" + prop(e, "RRULE") + "|" + prop(e, "EXDATE")
			}
		}
		return out
	}
	before := identities(c)
	seqBefore, _ := strconv.Atoi(prop(m, "SEQUENCE"))
	for _, it := range []domain.Item{series, windowed, fuzzy, near, far, counted} {
		if _, err := mat.Item(ctx, it.ID); err != nil {
			return err
		}
	}
	renamed := series.Title + " (renamed)"
	if _, _, err := st.UpdateItemAndMaterialize(ctx, series.ID, store.ItemPatch{Title: &renamed}, time.Now(), nil); err != nil {
		return err
	}
	c2, err := parseICS(get("/calendar/" + token + ".ics").Body.Bytes())
	if err != nil {
		return err
	}
	after := identities(c2)
	same := len(before) == len(after)
	for k, v := range before {
		same = same && after[k] == v
	}
	m2 := c2.master(uid)
	seqAfter := -1
	title2 := ""
	if m2 != nil {
		seqAfter, _ = strconv.Atoi(prop(m2, "SEQUENCE"))
		title2 = ical.UnescapeText(prop(m2, "SUMMARY"))
	}
	fmt.Printf("  stable  %d events across a re-materialization and a rename: identical times and exceptions=%v; series still one master (%d VEVENTs under its UID, was %d); SEQUENCE %d -> %d; title now %q  %s\n",
		len(before), same, len(c2.byUID[uid]), len(c.byUID[uid]), seqBefore, seqAfter, title2,
		verdict(same && len(c2.byUID[uid]) == len(c.byUID[uid]) && seqAfter >= seqBefore && title2 == renamed))

	// 11. Hostile text. The title carries a NUL, an ESC, an invalid byte, a
	// newline, every character TEXT escapes, and enough multi-byte characters to
	// force a fold. The feed has already parsed; this checks what came out.
	var hostileEv *ical.Component
	for _, e := range c.events {
		if strings.HasPrefix(ical.UnescapeText(prop(e, "SUMMARY")), "ics hostile") {
			hostileEv = e
			break
		}
	}
	wantTitle := strings.TrimSpace(ical.Clean(hostileTitle))
	folded := bytes.Contains(data, []byte("\r\n "))
	fmt.Printf("  text    summary round-trips cleaned=%v  fold present=%v  no NUL/ESC/0xFF in the feed (counted above)  %s\n",
		hostileEv != nil && ical.UnescapeText(prop(hostileEv, "SUMMARY")) == wantTitle, folded,
		verdict(hostileEv != nil && ical.UnescapeText(prop(hostileEv, "SUMMARY")) == wantTitle && folded && rawBad == 0))

	if out := os.Getenv("NAVISEED_ICS_OUT"); out != "" {
		if err := os.WriteFile(out, data, 0o600); err != nil {
			return err
		}
		fmt.Printf("  wrote   %s (%d bytes) for an external validator\n", out, len(data))
	}
	return nil
}

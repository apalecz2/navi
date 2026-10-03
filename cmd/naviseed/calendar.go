package main

import (
	"context"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"time"

	"github.com/aidenpaleczny/navi/internal/config"
	"github.com/aidenpaleczny/navi/internal/domain"
	"github.com/aidenpaleczny/navi/internal/health"
	"github.com/aidenpaleczny/navi/internal/httpapi"
	"github.com/aidenpaleczny/navi/internal/materializer"
	"github.com/aidenpaleczny/navi/internal/metrics"
	"github.com/aidenpaleczny/navi/internal/schedule"
	"github.com/aidenpaleczny/navi/internal/scheduler"
	"github.com/aidenpaleczny/navi/internal/store"
	"github.com/aidenpaleczny/navi/internal/web"
)

type calRow struct {
	ID                 string  `json:"id"`
	ItemID             string  `json:"item_id"`
	Title              string  `json:"title"`
	StartsAt           string  `json:"starts_at"`
	StartsAtLocal      string  `json:"starts_at_local"`
	DateLocal          string  `json:"date_local"`
	Status             string  `json:"status"`
	SnoozeDepth        int     `json:"snooze_depth"`
	ParentOccurrenceID *string `json:"parent_occurrence_id"`
	ItemArchived       bool    `json:"item_archived"`
}

type calBody struct {
	From                string   `json:"from"`
	To                  string   `json:"to"`
	Timezone            string   `json:"timezone"`
	MaterializedThrough *string  `json:"materialized_through"`
	Occurrences         []calRow `json:"occurrences"`
}

func (b calBody) has(id string) (calRow, bool) {
	for _, r := range b.Occurrences {
		if r.ID == id {
			return r, true
		}
	}
	return calRow{}, false
}

// chipHTML cuts one chip's <a> out of the rendered month, found by the
// occurrence id its link carries, so an assertion about a chip reads that chip
// and not the page.
func chipHTML(page, id string) string {
	i := strings.Index(page, "o="+id)
	if i < 0 {
		return ""
	}
	open := strings.LastIndex(page[:i], "<a ")
	end := strings.Index(page[i:], "</a>")
	if open < 0 || end < 0 {
		return ""
	}
	return page[open : i+end]
}

var timeRe = regexp.MustCompile(`\d\d:\d\d`)

// seedByTitle creates an item on first run and reuses it after, the same way
// seedFuzzyItem does.
func seedByTitle(ctx context.Context, st *store.Store, title, sched, tz string) (domain.Item, error) {
	existing, err := st.ListActiveItems(ctx)
	if err != nil {
		return domain.Item{}, err
	}
	for _, it := range existing {
		if it.Title == title {
			return it, nil
		}
	}
	return st.CreateItem(ctx, domain.NewItem{Title: title, Schedule: json.RawMessage(sched), TZ: tz})
}

// reportCalendar drives GET /api/occurrences and the /app/calendar view through
// a real httpapi handler with the real web.App mounted on it.
//
// What it is for: V4's calendar is one date-range query, the exit criterion is
// that a random-time item shows its drawn instant, and two properties - the
// device-zone day boundary and "a chain counts once" - are exactly the kind that
// look right on every run except the ones that matter. The boundary row is
// therefore built from the device zone at run time, the same discipline the
// day-view section follows after the from_date bug.
//
// The device zone is read the way the handlers read it. reportZones moves it to
// Europe/Lisbon early in the run, so anything computed against DEFAULT_TZ here
// would be hours out.
func reportCalendar(ctx context.Context, st *store.Store, tz string, fallback *time.Location, log *slog.Logger) error {
	fmt.Println("\ncalendar  GET /api/occurrences, /app/calendar")

	zones, err := schedule.LoadZones(ctx, st, fallback)
	if err != nil {
		return err
	}
	loc := zones.Local()

	m := metrics.New()
	mat := materializer.New(log.With("component", "mat-cal"), st, fallback)
	srv := httpapi.New(config.HTTP{Addr: ":0"}, log.With("component", "httpapi"),
		health.New(), m, st, mat, time.Time{}, fallback, nil,
		web.New(log.With("component", "web"), st, fallback))

	do := func(method, path, body string) *httptest.ResponseRecorder {
		var req *http.Request
		if body == "" {
			req = httptest.NewRequest(method, path, nil)
		} else {
			req = httptest.NewRequest(method, path, strings.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
		}
		rec := httptest.NewRecorder()
		srv.Handler.ServeHTTP(rec, req.WithContext(ctx))
		return rec
	}
	get := func(path string) *httptest.ResponseRecorder { return do(http.MethodGet, path, "") }
	list := func(q string) (calBody, int) {
		rec := get("/api/occurrences?" + q)
		var b calBody
		_ = json.Unmarshal(rec.Body.Bytes(), &b)
		return b, rec.Code
	}
	day := func(t time.Time) string { return t.In(loc).Format("2006-01-02") }
	month := func(t time.Time) string { return t.In(loc).Format(web.MonthLayout) }
	body := func(mo string, open string) string {
		p := "/app/calendar/body?m=" + mo
		if open != "" {
			p += "&o=" + open
		}
		return get(p).Body.String()
	}

	// 1. Range validation, through the ordinary envelope.
	fmt.Printf("  zone for from/to  %s (schedule.Zones.Local)\n", loc)
	type bad struct{ name, q, field, want string }
	for _, c := range []bad{
		{"from missing", "to=2026-01-01", "from", "required"},
		{"to missing", "from=2026-01-01", "to", "required"},
		{"malformed date", "from=2026-8-5&to=2026-08-06", "from", `"2026-8-5"`},
		{"impossible date", "from=2026-02-30&to=2026-03-02", "from", `"2026-02-30"`},
		{"from after to", "from=2026-03-02&to=2026-03-01", "from", "after"},
		{"401 days", "from=2025-01-01&to=2026-02-05", "to", "401"},
		{"unknown status", "from=2026-01-01&to=2026-01-02&status=done", "status", `"done"`},
		{"bad item id", "from=2026-01-01&to=2026-01-02&item_id=nope", "item_id", `"nope"`},
	} {
		rec := get("/api/occurrences?" + c.q)
		var e struct{ Error, Message, Field string }
		_ = json.Unmarshal(rec.Body.Bytes(), &e)
		ok := rec.Code == http.StatusBadRequest && e.Error == "validation_failed" &&
			e.Field == c.field && strings.Contains(e.Message, c.want)
		fmt.Printf("  reject %-16s status %d  field=%-8s %q  %s\n", c.name, rec.Code, e.Field, e.Message, verdict(ok))
	}
	_, code400 := list("from=2025-01-01&to=2026-02-04")
	fmt.Printf("  exactly 400 days accepted  status %d  %s\n", code400, verdict(code400 == http.StatusOK))

	// 2. The boundary row. At the boundary the device zone and UTC disagree about
	// which date a row is on; a query that compared UTC dates would put it on the
	// wrong one. 00:30 local when the zone is ahead of UTC, 23:30 when behind.
	boundaryItem, err := seedFireItem(ctx, st, "calendar boundary probe", domain.NotifySilent, tz)
	if err != nil {
		return err
	}
	d := time.Date(2024, 7, 14, 0, 0, 0, 0, loc)
	_, off := d.Zone()
	at := time.Date(2024, 7, 14, 0, 30, 0, 0, loc)
	if off < 0 {
		at = time.Date(2024, 7, 14, 23, 30, 0, 0, loc)
	}
	edge, err := fireOccurrence(ctx, st, boundaryItem, at, nil)
	if err != nil {
		return err
	}
	onD, _ := list("from=2024-07-14&to=2024-07-14")
	prev, _ := list("from=2024-07-13&to=2024-07-13")
	next, _ := list("from=2024-07-15&to=2024-07-15")
	row, found := onD.has(edge.ID)
	_, inPrev := prev.has(edge.ID)
	_, inNext := next.has(edge.ID)
	utcDate := at.UTC().Format("2006-01-02")
	if off == 0 {
		fmt.Printf("  boundary row  device zone is UTC here; the two dates cannot differ, skipped  %s\n", verdict(true))
	} else {
		other, _ := list("from=" + utcDate + "&to=" + utcDate)
		_, inUTC := other.has(edge.ID)
		fmt.Printf("  boundary row  %s is %s local, %s UTC  on local day=%v  not on neighbours=%v  not on UTC day=%v  %s\n",
			domain.FormatTime(at), day(at), utcDate, found && row.DateLocal == "2024-07-14", !inPrev && !inNext, !inUTC,
			verdict(found && row.DateLocal == "2024-07-14" && !inPrev && !inNext && !inUTC && utcDate != "2024-07-14"))
	}

	// 3. Resolved random times: one windowed item, one fuzzy item, drawn by the
	// real materializer. The row's instant is what the API and the chip show.
	today := time.Now().In(loc)
	from := day(today)
	to := day(today.AddDate(0, 0, 35))
	kinds := []struct {
		title, sched, lo, hi string
	}{
		{"calendar windowed probe", `{"kind":"windowed","rrule":"FREQ=DAILY","window":["09:00","17:00"]}`, "09:00", "17:00"},
		{"calendar fuzzy probe", specExamples[3], "09:00", "21:00"},
	}
	var windowed domain.Item
	var windowedRows []calRow
	for _, k := range kinds {
		item, err := seedByTitle(ctx, st, k.title, k.sched, tz)
		if err != nil {
			return err
		}
		if _, err := mat.Item(ctx, item.ID); err != nil {
			return err
		}
		iz, err := itemZone(ctx, st, item, fallback)
		if err != nil {
			return err
		}
		b, code := list("from=" + from + "&to=" + to + "&item_id=" + item.ID)
		concrete, inWindow, rounded, sameItem := len(b.Occurrences) > 0, true, true, true
		for _, r := range b.Occurrences {
			t, err := domain.ParseTime(r.StartsAt)
			if err != nil {
				return err
			}
			lt := t.In(iz).Format("15:04")
			if lt < k.lo || lt > k.hi {
				inWindow = false
			}
			if t.Minute()%5 != 0 || t.Second() != 0 {
				rounded = false
			}
			if r.ItemID != item.ID || r.StartsAtLocal != t.In(loc).Format("15:04") {
				sameItem = false
			}
		}
		fmt.Printf("  %-24s status %d  %d rows  inside %s-%s=%v  5-minute instants=%v  local time matches row=%v  %s\n",
			k.title, code, len(b.Occurrences), k.lo, k.hi, inWindow, rounded, sameItem,
			verdict(code == http.StatusOK && concrete && inWindow && rounded && sameItem))

		// The rendered chip: exactly the drawn time, and never a second one that
		// would make it a range.
		if len(b.Occurrences) > 0 {
			first := b.Occurrences[0]
			t, _ := domain.ParseTime(first.StartsAt)
			chip := chipHTML(body(month(t), ""), first.ID)
			times := timeRe.FindAllString(chip, -1)
			one := len(times) > 0
			for _, s := range times {
				if s != first.StartsAtLocal {
					one = false
				}
			}
			fmt.Printf("    chip for %s  shows only %s=%v  no range separator=%v  %s\n", first.ID, first.StartsAtLocal, one,
				!strings.Contains(chip, k.lo+"–") && !strings.Contains(chip, k.lo+"-"), verdict(chip != "" && one))
		}
		if k.title == "calendar windowed probe" {
			windowed, windowedRows = item, b.Occurrences
		}
	}

	// 4. Colour is the item's id, hashed: identical across reloads and months, and
	// equal to the independent computation here.
	hue := func(page, id string) string {
		re := regexp.MustCompile(`style="--h:(\d+);?"[^>]*href="[^"]*o=` + id)
		if g := re.FindStringSubmatch(page); len(g) == 2 {
			return g[1]
		}
		return ""
	}
	h := fnv.New32a()
	_, _ = h.Write([]byte(windowed.ID))
	wantHue := fmt.Sprint(h.Sum32() % 360)
	var firstA, firstB *calRow
	var monthA, monthB string
	for i, r := range windowedRows {
		t, _ := domain.ParseTime(r.StartsAt)
		switch {
		case firstA == nil:
			firstA, monthA = &windowedRows[i], month(t)
		case firstB == nil && month(t) != monthA:
			firstB, monthB = &windowedRows[i], month(t)
		}
	}
	if firstA != nil {
		a1, a2 := hue(body(monthA, ""), firstA.ID), hue(body(monthA, ""), firstA.ID)
		stable, across := a1 != "" && a1 == a2 && a1 == wantHue, true
		if firstB != nil {
			across = hue(body(monthB, ""), firstB.ID) == wantHue
		}
		fmt.Printf("  item colour  hue %s from id, reload=%v  across months %s -> %s=%v  %s\n", wantHue, stable, monthA,
			map[bool]string{true: monthB, false: "(one month only)"}[firstB != nil], across, verdict(stable && across))
	}

	// 5. History, archived items included. Resolved the way a person would and then
	// archived: the rows stay and so does the title.
	hist, err := seedFireItem(ctx, st, "calendar history probe", domain.NotifySilent, tz)
	if err != nil {
		return err
	}
	type h3 struct {
		day    int
		status domain.Status
		glyph  string
		class  string
	}
	var ids []string
	hists := []h3{{20, domain.StatusCompleted, "✓", "st-completed"}, {21, domain.StatusSkipped, "–", "st-skipped"}, {22, domain.StatusMissed, "!", "st-missed"}}
	for _, c := range hists {
		occ, err := fireOccurrence(ctx, st, hist, time.Date(2024, 7, c.day, 12, 0, 0, 0, loc), nil)
		if err != nil {
			return err
		}
		if _, err := st.ResolveOccurrence(ctx, occ.ID, c.status, nil, domain.ResolvedByWeb, time.Now()); err != nil {
			return err
		}
		ids = append(ids, occ.ID)
	}
	r, err := mat.Begin(ctx)
	if err != nil {
		return err
	}
	if _, _, err := st.ArchiveItem(ctx, hist.ID, r.Now(), func(it domain.Item, existing []domain.Occurrence) (store.Plan, error) {
		return mat.PlanFor(it, schedule.Schedule{}, r, existing)
	}); err != nil {
		return err
	}
	hb, hcode := list("from=2024-07-20&to=2024-07-22&item_id=" + hist.ID)
	page := body("2024-07", "")
	allOK := hcode == http.StatusOK && len(hb.Occurrences) == 3
	for i, c := range hists {
		row, ok := hb.has(ids[i])
		chip := chipHTML(page, ids[i])
		allOK = allOK && ok && row.ItemArchived && row.Title == hist.Title && row.Status == string(c.status) &&
			strings.Contains(chip, c.class) && strings.Contains(chip, c.glyph) && strings.Contains(chip, hist.Title)
	}
	fmt.Printf("  archived item's history  3 rows, archived=true, title kept, completed/skipped/missed drawn with ✓ – !  legend marks archived=%v  %s\n",
		strings.Contains(page, `<span class="tag">archived</span>`), verdict(allOK && strings.Contains(page, `<span class="tag">archived</span>`)))

	// 6. Filters, against rows whose status is known. The completed-only range is
	// the history just written.
	comp, c1 := list("from=2024-07-20&to=2024-07-22&status=completed")
	onlyCompleted := c1 == http.StatusOK && len(comp.Occurrences) >= 1
	for _, r := range comp.Occurrences {
		onlyCompleted = onlyCompleted && r.Status == "completed"
	}
	_, hasOurs := comp.has(ids[0])
	_, hasSkipped := comp.has(ids[1])
	both, c2 := list("from=2024-07-20&to=2024-07-22&status=skipped&item_id=" + hist.ID)
	_, bothHas := both.has(ids[1])
	none, c3 := list("from=2024-07-20&to=2024-07-22&status=pending&item_id=" + hist.ID)
	fmt.Printf("  status filter     only completed=%v  includes the completed probe=%v  excludes the skipped one=%v  %s\n",
		onlyCompleted, hasOurs, !hasSkipped, verdict(onlyCompleted && hasOurs && !hasSkipped))
	fmt.Printf("  item+status       skipped for that item: %d row  pending for that item: %d rows  %s\n",
		len(both.Occurrences), len(none.Occurrences),
		verdict(c2 == http.StatusOK && c3 == http.StatusOK && len(both.Occurrences) == 1 && bothHas && len(none.Occurrences) == 0))
	all, _ := list("from=" + from + "&to=" + to)
	wOnly, _ := list("from=" + from + "&to=" + to + "&item_id=" + windowed.ID)
	fmt.Printf("  item filter       %d rows for the windowed item of %d in range, all its own=%v  %s\n",
		len(wOnly.Occurrences), len(all.Occurrences), func() bool {
			for _, r := range wOnly.Occurrences {
				if r.ItemID != windowed.ID {
					return false
				}
			}
			return true
		}(), verdict(len(wOnly.Occurrences) > 0 && len(all.Occurrences) >= len(wOnly.Occurrences)))

	// 7. A snooze chain: the parent keeps its row in the API, the calendar draws
	// the chain once.
	loud, err := seedFireItem(ctx, st, "calendar snooze probe", domain.NotifyAtTime, tz)
	if err != nil {
		return err
	}
	parent, err := fireOccurrence(ctx, st, loud, time.Now().Add(-time.Minute), ptr("calendar snooze body"))
	if err != nil {
		return err
	}
	if _, err := scheduler.New(log.With("component", "scheduler-cal"), st, &recordingTransport{}, m, time.Now()).Fire(ctx); err != nil {
		return err
	}
	snz := do(http.MethodPost, "/api/occurrences/"+parent.ID+"/snooze", `{"delta":"10m","source":"web"}`)
	var sres struct {
		Occurrence struct {
			ID       string `json:"id"`
			StartsAt string `json:"starts_at"`
		} `json:"occurrence"`
	}
	_ = json.Unmarshal(snz.Body.Bytes(), &sres)
	childStart, _ := domain.ParseTime(sres.Occurrence.StartsAt)
	if month(childStart) != month(parent.StartsAt) {
		fmt.Printf("  snooze chain  parent and child fall in different months right now; skipped  %s\n", verdict(snz.Code == http.StatusOK))
	} else {
		mo := month(childStart)
		pg := body(mo, "")
		rows, _ := list("from=" + day(parent.StartsAt) + "&to=" + day(childStart) + "&item_id=" + loud.ID)
		_, apiParent := rows.has(parent.ID)
		_, apiChild := rows.has(sres.Occurrence.ID)
		chip := chipHTML(pg, sres.Occurrence.ID)
		fmt.Printf("  snooze chain  status %d  API has both rows=%v  parent drawn=%v  child drawn once=%v  mark ↷1=%v  %s\n",
			snz.Code, apiParent && apiChild, chipHTML(pg, parent.ID) != "", strings.Count(pg, `href="`+"/app/calendar?m="+mo+"&amp;o="+sres.Occurrence.ID) == 1,
			strings.Contains(chip, "↷1"),
			verdict(snz.Code == http.StatusOK && apiParent && apiChild && chipHTML(pg, parent.ID) == "" &&
				strings.Count(pg, `href="`+"/app/calendar?m="+mo+"&amp;o="+sres.Occurrence.ID) == 1 && strings.Contains(chip, "↷1")))

		// The detail panel: the day view's row component, and the chain's origin.
		det := body(mo, sres.Occurrence.ID)
		fmt.Printf("  detail of the child  row component=%v  Done+Skip offered=%v  no Snooze (pending)=%v  states original time=%v  settles to the month=%v  %s\n",
			strings.Contains(det, `data-id="`+sres.Occurrence.ID+`"`),
			strings.Contains(det, ">Done<") && strings.Contains(det, ">Skip<"),
			!strings.Contains(det, ">Snooze<"),
			strings.Contains(det, "Snoozed ×1") && strings.Contains(det, "Originally due"),
			strings.Contains(det, `data-settle="/app/calendar/body?m=`+mo),
			verdict(strings.Contains(det, `data-id="`+sres.Occurrence.ID+`"`) && strings.Contains(det, ">Done<") &&
				strings.Contains(det, "Snoozed ×1") && strings.Contains(det, `data-settle="/app/calendar/body?m=`+mo)))
		pdet := body(mo, parent.ID)
		fmt.Printf("  detail of the parent  says snoozed=%v  links to the later one=%v  no buttons=%v  %s\n",
			strings.Contains(pdet, "Snoozed. This reminder continues"), strings.Contains(pdet, "See the later one"),
			!strings.Contains(pdet, "<button"),
			verdict(strings.Contains(pdet, "Snoozed. This reminder continues") && strings.Contains(pdet, "See the later one") &&
				!strings.Contains(pdet, "<button")))

		// Resolution through the same endpoint the day view posts to, and the settle
		// the panel then re-fetches.
		done := do(http.MethodPost, "/api/occurrences/"+sres.Occurrence.ID+"/resolve", `{"status":"completed","note":null,"source":"web"}`)
		after := body(mo, sres.Occurrence.ID)
		fmt.Printf("  resolve from the detail path  status %d  chip is done=%v  panel shows Done, no buttons=%v  %s\n",
			done.Code, strings.Contains(chipHTML(after, sres.Occurrence.ID), "st-completed"),
			strings.Contains(after, "st-completed") && !strings.Contains(after, ">Skip<"),
			verdict(done.Code == http.StatusOK && strings.Contains(chipHTML(after, sres.Occurrence.ID), "st-completed") &&
				!strings.Contains(after, ">Skip<")))
	}

	// 8. The horizon, read from kv.last_materialized_through.
	through, haveThrough, err := st.LastMaterializedThrough(ctx)
	if err != nil {
		return err
	}
	cur, _ := list("from=" + from + "&to=" + from)
	apiThrough := cur.MaterializedThrough != nil && haveThrough && *cur.MaterializedThrough == domain.FormatTime(through)
	if haveThrough {
		inMonth := body(month(through), "")
		far := body("2031-01", "")
		past := body("2024-07", "")
		fmt.Printf("  horizon  API carries materialized_through=%v  its own month notes it=%v  a month past it says so=%v (%d hatched days of 31)  a month before it says nothing=%v  %s\n",
			apiThrough, strings.Contains(inMonth, "Generated through") && strings.Contains(inMonth, "hz-partial"),
			strings.Contains(far, "Past the horizon") && strings.Contains(far, "hz-none"), strings.Count(far, `class="day beyond`),
			!strings.Contains(past, `class="horizon`),
			verdict(apiThrough && strings.Contains(inMonth, "hz-partial") && strings.Contains(far, "hz-none") &&
				strings.Count(far, `class="day beyond`) == 31 && !strings.Contains(past, `class="horizon`)))
	} else {
		fmt.Printf("  horizon  kv.last_materialized_through unset: API reports null=%v  %s\n",
			cur.MaterializedThrough == nil, verdict(cur.MaterializedThrough == nil))
	}

	// 9. Shell: month navigation is a link htmx upgrades, never a client router,
	// and the narrow layout is the stylesheet's.
	pg := get("/app/calendar")
	bad2 := get("/app/calendar?m=2026-13")
	css := get("/app/static/app.css").Body.String()
	fmt.Printf("  /app/calendar  status %d  no-store=%v  hx-get steps=%v  hrefs work without script=%v  bad month %d  narrow agenda rule=%v  %s\n",
		pg.Code, pg.Header().Get("Cache-Control") == "no-store",
		strings.Contains(pg.Body.String(), `hx-push-url="/app/calendar?m=`),
		strings.Contains(pg.Body.String(), `href="/app/calendar?m=`), bad2.Code,
		strings.Contains(css, "max-width: 639px") && strings.Contains(css, "repeat(7"),
		verdict(pg.Code == http.StatusOK && pg.Header().Get("Cache-Control") == "no-store" &&
			strings.Contains(pg.Body.String(), `hx-push-url="/app/calendar?m=`) &&
			bad2.Code == http.StatusBadRequest && strings.Contains(css, "max-width: 639px")))
	return nil
}

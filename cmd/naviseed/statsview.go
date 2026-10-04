package main

import (
	"context"
	"encoding/json"
	"fmt"
	"html"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/aidenpaleczny/navi/internal/agent"
	"github.com/aidenpaleczny/navi/internal/config"
	"github.com/aidenpaleczny/navi/internal/conversation"
	"github.com/aidenpaleczny/navi/internal/domain"
	"github.com/aidenpaleczny/navi/internal/health"
	"github.com/aidenpaleczny/navi/internal/httpapi"
	"github.com/aidenpaleczny/navi/internal/materializer"
	"github.com/aidenpaleczny/navi/internal/metrics"
	"github.com/aidenpaleczny/navi/internal/schedule"
	"github.com/aidenpaleczny/navi/internal/stats"
	"github.com/aidenpaleczny/navi/internal/store"
	"github.com/aidenpaleczny/navi/internal/web"
)

// statsViewFixture is what reportStats hands the view section: the items it
// built and the hand table for A, so every expectation here is a count a person
// did and not a second stats call.
type statsViewFixture struct {
	itemA, itemB, itemE domain.Item
	chainsA             []fxChain
	linked, free        string
	runID               string
	sinceMonday         int
}

var (
	reRatePlot = regexp.MustCompile(`data-plot="rate"\s+data-series="([^"]*)"`)
	reCell     = regexp.MustCompile(`title="(\w{3}) (\d\d):00, (\d+)"`)
	reMax      = regexp.MustCompile(`--max:(\d+)`)
	reURL      = regexp.MustCompile(`https?://`)
)

// reportStatsView is the exit check for the statistics screen: the numbers on
// the page are the numbers get_stats reports, a thin dataset gets a sentence
// instead of a chart, both goal kinds come back in one shape, and no statistic
// can be served from the worker's cache. It reads the rendered HTML, since a
// browser is not available here; layout and the charts' pixels are not checked.
func reportStatsView(ctx context.Context, st *store.Store, tools *agent.Tools, svc *stats.Service,
	fallback *time.Location, log *slog.Logger, fx statsViewFixture) error {
	fmt.Println("  the view  /app/stats and /api/goals/{id}/progress")

	zones, err := schedule.LoadZones(ctx, st, fallback)
	if err != nil {
		return err
	}
	loc := zones.Local()
	now := time.Now().In(loc)
	f4 := func(p *float64) string {
		if p == nil {
			return "null"
		}
		return fmt.Sprintf("%g", *p)
	}
	same := func(p *float64, want float64) bool { return p != nil && *p == want }

	mat := materializer.New(log.With("component", "mat-statsview"), st, fallback)
	srv := httpapi.New(config.HTTP{Addr: ":0"}, log.With("component", "httpapi"),
		health.New(), metrics.New(), st, mat, time.Time{}, fallback, nil,
		svc, web.New(log.With("component", "web"), st, svc, fallback))
	do := func(path string) (int, string, http.Header) {
		rec := httptest.NewRecorder()
		srv.Handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil).WithContext(ctx))
		return rec.Code, strings.TrimSpace(rec.Body.String()), rec.Header()
	}
	page := func(path string) (string, error) {
		code, body, hdr := do(path)
		if code != http.StatusOK || hdr.Get("Cache-Control") != "no-store" {
			return "", fmt.Errorf("naviseed: GET %s: status %d, Cache-Control %q", path, code, hdr.Get("Cache-Control"))
		}
		return body, nil
	}

	// ---- thresholds: rate line -------------------------------------------
	fmt.Printf("  thresholds (stated on the page, evaluated server-side): %d settled and %d buckets with a rate for the line, %d completions for the grid, %d points for a goal\n",
		web.MinSettled, web.MinRateBuckets, web.MinCompletions, web.MinGoalPoints)

	// E: thirty-five days of a completion each. Its line must be drawn, and the
	// JSON handed to the chart must be the timeseries route's body, byte for byte.
	pE, err := page("/app/stats?range=quarter&item=" + fx.itemE.ID)
	if err != nil {
		return err
	}
	_, tsE, _ := do("/api/stats/timeseries?range=quarter&bucket=week&item_id=" + fx.itemE.ID)
	m := reRatePlot.FindStringSubmatch(pE)
	fmt.Printf("  E (35 completed, quarter): rate line drawn from the API's own bytes  %s\n",
		verdict(m != nil && html.UnescapeString(m[1]) == tsE))

	// B has one settled chain: a sentence, never a chart.
	pB, err := page("/app/stats?range=all&item=" + fx.itemB.ID)
	if err != nil {
		return err
	}
	fmt.Printf("  B (1 settled): no rate plot, the page says how far along it is (1 of %d), no grid  %s\n",
		web.MinSettled, verdict(!strings.Contains(pB, `data-plot="rate"`) &&
			strings.Contains(pB, fmt.Sprintf("Not enough data yet: 1 of the %d settled", web.MinSettled)) &&
			!strings.Contains(pB, `class="heat"`) && strings.Contains(pB, "completions this needs")))

	// A: readiness from the hand table. Settled = completed or missed; buckets =
	// distinct local Mondays among those days.
	settledA, weeks := 0, map[string]bool{}
	for _, c := range fx.chainsA {
		if c.class == scDone || c.class == scMissed {
			settledA++
			d := time.Date(now.Year(), now.Month(), now.Day()-c.daysAgo, 12, 0, 0, 0, loc)
			weeks[d.AddDate(0, 0, -((int(d.Weekday())+6)%7)).Format(domain.DateLayout)] = true
		}
	}
	wantReadyA := settledA >= web.MinSettled && len(weeks) >= web.MinRateBuckets
	pA, err := page("/app/stats?range=all&item=" + fx.itemA.ID)
	if err != nil {
		return err
	}
	fmt.Printf("  A (%d settled over %d weeks by hand): line drawn=%v, expected %v  %s\n", settledA, len(weeks),
		strings.Contains(pA, `data-plot="rate"`), wantReadyA, verdict(strings.Contains(pA, `data-plot="rate"`) == wantReadyA))

	// ---- the heatmap ------------------------------------------------------
	cells := map[string]int{}
	for _, mm := range reCell.FindAllStringSubmatch(pE, -1) {
		n, _ := strconv.Atoi(mm[3])
		cells[mm[1]+mm[2]] = n
	}
	var hm stats.Heatmap
	_, hmBody, _ := do("/api/stats/heatmap?range=quarter&item_id=" + fx.itemE.ID)
	_ = json.Unmarshal([]byte(hmBody), &hm)
	var want [7][24]int // by hand: every completion lands at 09:10 local
	for d := 1; d <= 35; d++ {
		want[(int(time.Date(now.Year(), now.Month(), now.Day()-d, 9, 10, 0, 0, loc).Weekday())+6)%7][9]++
	}
	match := len(cells) == 168 && hm.Cells == want && hm.Total == 35
	for d, wd := range hm.Weekdays {
		for h := 0; h < 24; h++ {
			if cells[fmt.Sprintf("%s%02d", wd, h)] != want[d][h] {
				match = false
			}
		}
	}
	mx := reMax.FindStringSubmatch(pE)
	fmt.Printf("  E grid: 168 cells on the page equal the heatmap route and a hand count (35 at 09:00, by weekday), --max=%s  %s\n",
		firstOr(mx, "?"), verdict(match && mx != nil && mx[1] == strconv.Itoa(hm.Max)))

	// ---- the numbers equal get_stats --------------------------------------
	checkAgainstTool := func(label, path, rng string, item *domain.Item) error {
		args := agent.GetStatsArgs{Range: rng}
		if item != nil {
			args.ItemID = &item.ID
		}
		res, err := callTool(ctx, tools, "get_stats", args)
		if err != nil {
			return err
		}
		p, err := page(path)
		if err != nil {
			return err
		}
		said := conversation.Confirmation("get_stats", res)
		s := res.Stats
		ok := true
		if s.CompletionRate != nil {
			ok = ok && strings.Contains(p, `class="tv">`+stats.Percent(*s.CompletionRate)+`<`) &&
				strings.Contains(p, fmt.Sprintf("%d of %d completed", s.Totals.Completed, s.Totals.Settled)) &&
				strings.Contains(said, fmt.Sprintf("%d of %d completed (%s)", s.Totals.Completed, s.Totals.Settled, stats.Percent(*s.CompletionRate)))
		}
		if s.MedianLagMinutes != nil {
			ok = ok && strings.Contains(p, `class="tv">`+stats.MinutesPhrase(*s.MedianLagMinutes)+`<`) &&
				strings.Contains(said, stats.MinutesPhrase(*s.MedianLagMinutes))
		}
		for _, it := range s.Items {
			phrase := stats.StreakPhrase(it.CurrentStreak, it.LongestStreak)
			ok = ok && strings.Contains(p, `class="istreak">`+phrase+`<`) && strings.Contains(said, phrase) &&
				strings.Contains(p, html.EscapeString(it.Title))
		}
		fmt.Printf("  page == get_stats (%s): rate, lag and %d streak line(s) on the page and in the chat text  %s\n",
			label, len(s.Items), verdict(ok))
		return nil
	}
	if err := checkAgainstTool("A, all", "/app/stats?range=all&item="+fx.itemA.ID, "all", &fx.itemA); err != nil {
		return err
	}
	if err := checkAgainstTool("E, quarter", "/app/stats?range=quarter&item="+fx.itemE.ID, "quarter", &fx.itemE); err != nil {
		return err
	}
	if err := checkAgainstTool("every item, month", "/app/stats", "month", nil); err != nil {
		return err
	}
	if err := checkAgainstTool("every item, week", "/app/stats?range=week", "week", nil); err != nil {
		return err
	}

	// The streak wording, once: a plain count with "in a row", never a unit of time.
	res, err := callTool(ctx, tools, "get_stats", agent.GetStatsArgs{Range: "all", ItemID: &fx.itemA.ID})
	if err != nil {
		return err
	}
	row := res.Stats.Items[0]
	said := conversation.Confirmation("get_stats", res)
	fmt.Printf("  A streak reads %q on the page and in the chat, with no 'days' or 'weekdays'  %s\n",
		stats.StreakPhrase(row.CurrentStreak, row.LongestStreak),
		verdict(row.CurrentStreak == 2 && row.LongestStreak == 4 &&
			strings.Contains(pA, "2 in a row (best 4)") && strings.Contains(said, "2 in a row (best 4)") &&
			!strings.Contains(said, "streak ") && !strings.Contains(pA, "weekdays")))

	// ---- goals: one shape for both kinds -----------------------------------
	fmt.Println("  goals  GET /api/goals/{id}/progress")
	series := func(id string) (stats.GoalSeries, map[string]any, []map[string]any, error) {
		code, body, hdr := do("/api/goals/" + id + "/progress")
		var gs stats.GoalSeries
		var generic struct {
			Points []map[string]any `json:"points"`
		}
		var top map[string]any
		if code != http.StatusOK || hdr.Get("Cache-Control") != "no-store" ||
			json.Unmarshal([]byte(body), &gs) != nil || json.Unmarshal([]byte(body), &top) != nil ||
			json.Unmarshal([]byte(body), &generic) != nil {
			return gs, nil, nil, fmt.Errorf("naviseed: goal progress %s: status %d: %s", id, code, body)
		}
		return gs, top, generic.Points, nil
	}
	keys := func(m map[string]any) string {
		ks := make([]string, 0, len(m))
		for k := range m {
			ks = append(ks, k)
		}
		sort.Strings(ks)
		return strings.Join(ks, ",")
	}

	// One update so far: a trail of one is not a trail.
	pFree1, err := page("/app/stats?range=month")
	if err != nil {
		return err
	}
	figure := func(p, title string) string {
		for _, seg := range strings.Split(p, `<figure class="goal">`)[1:] {
			if strings.Contains(seg, title) {
				return seg
			}
		}
		return ""
	}
	freeTitle, linkedTitle := "stats freestanding "+fx.runID, "stats linked "+fx.runID
	fig := figure(pFree1, freeTitle)
	fmt.Printf("  freestanding, one update: headline 60%%, a sentence instead of a trail, no velocity invented  %s\n",
		verdict(fig != "" && strings.Contains(fig, "60%") && strings.Contains(fig, "there are 1") &&
			!strings.Contains(fig, "data-plot") && !strings.Contains(fig, "Velocity")))

	if _, err := callTool(ctx, tools, "log_goal_progress", agent.LogGoalProgressArgs{GoalID: fx.free, ProgressPct: intp(80)}); err != nil {
		return err
	}
	pFree2, err := page("/app/stats?range=month")
	if err != nil {
		return err
	}
	gF, topF, ptsF, err := series(fx.free)
	if err != nil {
		return err
	}
	fig = figure(pFree2, freeTitle)
	jsonF := regexp.MustCompile(`data-plot="progress"\s+data-series="([^"]*)"`).FindStringSubmatch(fig)
	_, bodyF, _ := do("/api/goals/" + fx.free + "/progress")
	velNil := true
	for _, p := range ptsF {
		velNil = velNil && p["velocity_per_week"] == nil
	}
	fmt.Printf("  freestanding, two updates: kind=%s unit=%s target=%d current=%s points=%d, trail drawn from the API's bytes, no velocity plot and every velocity null  %s\n",
		gF.Kind, gF.Unit, gF.Target, ptrInt(gF.Current), len(gF.Points),
		verdict(gF.Kind == stats.GoalKindFreestanding && gF.Unit == "percent" && gF.Target == 100 &&
			gF.Current != nil && *gF.Current == 80 && len(gF.Points) == 2 &&
			*gF.Points[0].Value == 60 && *gF.Points[1].Value == 80 && gF.VelocityPerWeek == nil && velNil &&
			jsonF != nil && html.UnescapeString(jsonF[1]) == bodyF && !strings.Contains(fig, "Velocity")))

	gL, topL, ptsL, err := series(fx.linked)
	if err != nil {
		return err
	}
	handWeek := 0
	for _, c := range fx.chainsA {
		if c.class == scDone && c.daysAgo <= fx.sinceMonday {
			handWeek++
		}
	}
	sum, err := svc.Summary(ctx, stats.Query{Range: stats.RangeMonth, Bucket: stats.BucketDay})
	if err != nil {
		return err
	}
	var rowL *stats.GoalStats
	for i := range sum.Goals {
		if sum.Goals[i].GoalID == fx.linked {
			rowL = &sum.Goals[i]
		}
	}
	mono := true
	for i := 1; i < len(gL.Points); i++ {
		mono = mono && *gL.Points[i].Value >= *gL.Points[i-1].Value
	}
	days := fx.sinceMonday + 1 // Monday through today
	last := gL.Points[len(gL.Points)-1]
	fmt.Printf("  linked: kind=%s unit=%s target=%d current=%s points=%d (hand: %d days), last point %s = summary %s = hand count %d, velocity %s  %s\n",
		gL.Kind, gL.Unit, gL.Target, ptrInt(gL.Current), len(gL.Points), days, ptrInt(last.Value),
		ptrInt(func() *int {
			if rowL == nil {
				return nil
			}
			return rowL.Completed
		}()), handWeek, f4(gL.VelocityPerWeek),
		verdict(gL.Kind == stats.GoalKindItemLinked && gL.Unit == "completions" && gL.Target == 3 &&
			len(gL.Points) == days && rowL != nil && rowL.Completed != nil &&
			*gL.Current == *rowL.Completed && *last.Value == *rowL.Completed && *last.Value == handWeek &&
			same(gL.VelocityPerWeek, *rowL.Velocity) && same(last.VelocityPerWeek, *rowL.Velocity) && mono))

	// One shape: the same top-level fields, and the same fields on a point.
	fmt.Printf("  both kinds, same top-level fields and same point fields  %s\n",
		verdict(keys(topF) == keys(topL) && len(ptsF) > 0 && len(ptsL) > 0 && keys(ptsF[0]) == keys(ptsL[0])))

	pLinked, err := page("/app/stats?range=month&item=" + fx.itemA.ID)
	if err != nil {
		return err
	}
	fig = figure(pLinked, linkedTitle)
	wantReady := days >= web.MinGoalPoints
	hasProg, hasVel := strings.Contains(fig, `data-plot="progress"`), strings.Contains(fig, `data-plot="velocity"`)
	_, bodyL, _ := do("/api/goals/" + fx.linked + "/progress")
	jsonL := regexp.MustCompile(`data-plot="progress"\s+data-series="([^"]*)"`).FindStringSubmatch(fig)
	ok := fig != "" && hasProg == wantReady && hasVel == wantReady
	if wantReady {
		ok = ok && jsonL != nil && sameSeries(html.UnescapeString(jsonL[1]), bodyL)
	} else {
		ok = ok && strings.Contains(fig, "Not enough data yet")
	}
	if ok == false && jsonL != nil {
		fmt.Printf("    page %s\n    api  %s\n", html.UnescapeString(jsonL[1]), bodyL)
	}
	fmt.Printf("  linked figure: %d day(s) into the week, progress+velocity plots=%v (expected %v), data is the API's bytes  %s\n",
		days, hasProg && hasVel, wantReady, verdict(ok))

	code404, _, _ := do("/api/goals/01ARZ3NDEKTSV4RRFFQ69G5FAV/progress")
	code400, _, _ := do("/api/goals/nope/progress")
	fmt.Printf("  unknown goal %d (want 404), malformed id %d (want 400)  %s\n", code404, code400,
		verdict(code404 == http.StatusNotFound && code400 == http.StatusBadRequest))

	// ---- the shell: navigation, assets, no CDN ----------------------------
	fmt.Println("  shell")
	day, err := page("/app/")
	if err != nil {
		return err
	}
	cal, err := page("/app/calendar")
	if err != nil {
		return err
	}
	all, err := page("/app/stats?range=all")
	if err != nil {
		return err
	}
	body, err := page("/app/stats/body?range=week")
	if err != nil {
		return err
	}
	navOK := true
	for _, p := range []string{day, cal, all} {
		navOK = navOK && strings.Contains(p, `href="/app/"`) && strings.Contains(p, `href="/app/calendar"`) && strings.Contains(p, `href="/app/stats"`)
	}
	fmt.Printf("  Today, Calendar and Stats link to one another; Stats marks itself current  %s\n",
		verdict(navOK && strings.Contains(all, `<a href="/app/stats" aria-current="page">`)))
	fmt.Printf("  no http(s):// URL in the day view, the calendar, the stats page or its fragment  %s\n",
		verdict(!reURL.MatchString(day) && !reURL.MatchString(cal) && !reURL.MatchString(all) && !reURL.MatchString(body)))
	fmt.Printf("  the fragment is a fragment, and the page loads uPlot and stats.js while the day view does not  %s\n",
		verdict(!strings.Contains(body, "<html") && strings.Contains(all, "uPlot.iife.min.js?v=") &&
			strings.Contains(all, "stats.js?v=") && strings.Contains(all, "uPlot.min.css?v=") &&
			!strings.Contains(day, "uPlot") && !strings.Contains(cal, "uPlot")))

	assetsOK := true
	for _, a := range []string{"uPlot.iife.min.js", "uPlot.min.css", "stats.js"} {
		m := regexp.MustCompile(a + `\?v=([0-9a-f]+)`).FindStringSubmatch(all)
		if m == nil {
			assetsOK = false
			continue
		}
		code, b, hdr := do("/app/static/" + a + "?v=" + m[1])
		assetsOK = assetsOK && code == http.StatusOK && len(b) > 100 && strings.Contains(hdr.Get("Cache-Control"), "immutable")
	}
	fmt.Printf("  uPlot (js, css) and stats.js are served from /app/static, immutable  %s\n", verdict(assetsOK))

	for _, bad := range []string{"/app/stats?range=year", "/app/stats?item=nope", "/app/stats/body?range=year"} {
		code, _, _ := do(bad)
		fmt.Printf("  reject %-30s status %d  %s\n", bad, code, verdict(code == http.StatusBadRequest))
	}

	// ---- the worker --------------------------------------------------------
	_, sw, _ := do("/app/sw.js")
	precache := regexp.MustCompile(`const PRECACHE = \[(.*)\];`).FindStringSubmatch(sw)
	preOK := precache != nil
	if preOK {
		for _, u := range regexp.MustCompile(`"([^"]*)"`).FindAllStringSubmatch(precache[1], -1) {
			preOK = preOK && (u[1] == "/app/offline" || strings.HasPrefix(u[1], "/app/static/"))
		}
		preOK = preOK && strings.Contains(precache[1], "uPlot.iife.min.js") && strings.Contains(precache[1], "stats.js")
	}
	var code []string
	for _, l := range strings.Split(sw, "\n") {
		// The precache list names stats.js on purpose; it is the code that must not.
		if t := strings.TrimSpace(l); !strings.HasPrefix(t, "//") && !strings.HasPrefix(t, "const PRECACHE") {
			code = append(code, l)
		}
	}
	src := strings.Join(code, "\n")
	fmt.Printf("  worker precaches only /app/static assets and the offline page (uPlot and stats.js among them); its code names neither /api nor stats  %s\n",
		verdict(preOK && !strings.Contains(src, "/api") && !strings.Contains(src, "stats")))

	noStoreAll := true
	for _, p := range []string{
		"/app/stats", "/app/stats?range=all", "/app/stats/body", "/api/stats/summary", "/api/stats/timeseries",
		"/api/stats/heatmap", "/api/goals/" + fx.linked + "/progress", "/api/goals/" + fx.free + "/progress",
	} {
		c, _, hdr := do(p)
		noStoreAll = noStoreAll && c == http.StatusOK && hdr.Get("Cache-Control") == "no-store"
	}
	fmt.Printf("  every statistic the app serves is Cache-Control: no-store, and none is a worker-cacheable path  %s\n", verdict(noStoreAll))
	return nil
}

func firstOr(m []string, def string) string {
	if len(m) > 1 {
		return m[1]
	}
	return def
}

// sameSeries compares two renderings of one item-linked series, ignoring the
// instant of its closing point: that point is "as of now", so two requests a
// second apart legitimately differ there and nowhere else.
func sameSeries(a, b string) bool {
	norm := func(raw string) string {
		var gs stats.GoalSeries
		if json.Unmarshal([]byte(raw), &gs) != nil || len(gs.Points) == 0 {
			return raw
		}
		gs.Points[len(gs.Points)-1].At = ""
		out, _ := json.Marshal(gs)
		return string(out)
	}
	return norm(a) == norm(b)
}

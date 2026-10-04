package main

import (
	"context"
	"encoding/json"
	"fmt"
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

type todayBody struct {
	Date        string `json:"date"`
	Timezone    string `json:"timezone"`
	Occurrences []struct {
		ID               string  `json:"id"`
		Title            string  `json:"title"`
		StartsAtLocal    string  `json:"starts_at_local"`
		Status           string  `json:"status"`
		ResolvedAt       *string `json:"resolved_at"`
		ResolutionSource *string `json:"resolution_source"`
		NotifyPolicy     string  `json:"notify_policy"`
		SnoozeDepth      int     `json:"snooze_depth"`
	} `json:"occurrences"`
	Counts struct {
		Total, Resolved, Outstanding int
	} `json:"counts"`
}

// rowHTML cuts one occurrence's <li> out of rendered HTML, so an assertion
// about which buttons a row offers reads that row and not the page.
func rowHTML(page, id string) string {
	i := strings.Index(page, `data-id="`+id+`"`)
	if i < 0 {
		return ""
	}
	// Back up to the opening tag: the status class sits before data-id on it.
	if open := strings.LastIndex(page[:i], "<li"); open >= 0 {
		i = open
	}
	end := strings.Index(page[i:], "</li>")
	if end < 0 {
		return page[i:]
	}
	return page[i : i+end]
}

// reportDayView drives GET /api/today and the /app day view through a real
// httpapi handler with the real web.App mounted on it.
//
// The day view's write path is the browser POSTing JSON to the two /api
// routes, exactly as app.js does, so "a resolution through the day view's path"
// is those requests with source "web" - and the assertion that matters is that
// the fragment the client then swaps in tells the truth after a no-op and after
// a 409, because that fragment is the whole of how the optimistic flip settles.
//
// The device zone is read the way the handlers read it. reportZones moves it
// to Europe/Lisbon early in the run, so anything computed against DEFAULT_TZ
// here would be hours out by the time of day this ran.
func reportDayView(ctx context.Context, st *store.Store, tz string, fallback *time.Location, log *slog.Logger) error {
	fmt.Println("\nday view  GET /api/today, /app/")

	zones, err := schedule.LoadZones(ctx, st, fallback)
	if err != nil {
		return err
	}
	loc := zones.Local()

	m := metrics.New()
	srv := httpapi.New(config.HTTP{Addr: ":0"}, log.With("component", "httpapi"),
		health.New(), m, st, materializer.New(log.With("component", "mat-day"), st, fallback),
		time.Time{}, fallback, nil, nil, web.New(log.With("component", "web"), st, nil, fallback))

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

	// Three rows reached the way production reaches them. The silent item is
	// never claimed, so its row stays pending whatever scheduler passes run.
	silent, err := seedFireItem(ctx, st, "day view silent probe", domain.NotifySilent, tz)
	if err != nil {
		return err
	}
	loud, err := seedFireItem(ctx, st, "day view notified probe", domain.NotifyAtTime, tz)
	if err != nil {
		return err
	}
	at := time.Now().Add(-time.Minute)
	pending, err := fireOccurrence(ctx, st, silent, at, nil)
	if err != nil {
		return err
	}
	a, err := fireOccurrence(ctx, st, loud, at, ptr("day view body a"))
	if err != nil {
		return err
	}
	b, err := fireOccurrence(ctx, st, loud, at.Add(time.Second), ptr("day view body b"))
	if err != nil {
		return err
	}
	rec := &recordingTransport{}
	if _, err := scheduler.New(log.With("component", "scheduler-day"), st, rec, m, time.Now()).Fire(ctx); err != nil {
		return err
	}

	// 1. GET /api/today.
	res := do(http.MethodGet, "/api/today", "")
	var today todayBody
	decodeErr := json.Unmarshal(res.Body.Bytes(), &today)
	byID := map[string]int{}
	for i, o := range today.Occurrences {
		byID[o.ID] = i
	}
	pi, pok := byID[pending.ID]
	ai, aok := byID[a.ID]
	shape := decodeErr == nil && res.Code == http.StatusOK &&
		today.Timezone == loc.String() && today.Date == time.Now().In(loc).Format("2006-01-02") &&
		pok && aok &&
		today.Occurrences[pi].Status == "pending" && today.Occurrences[pi].NotifyPolicy == "silent" &&
		today.Occurrences[ai].Status == "notified" &&
		today.Occurrences[ai].StartsAtLocal == at.In(loc).Format("15:04") &&
		today.Counts.Resolved+today.Counts.Outstanding == today.Counts.Total
	if !shape && pok && aok {
		fmt.Printf("    debug pending=%+v notified=%+v want local=%s\n", today.Occurrences[pi], today.Occurrences[ai], at.In(loc).Format("15:04"))
	}
	fmt.Printf("  GET /api/today          status %d  date=%s tz=%s  %d rows  counts %d/%d/%d  %s\n",
		res.Code, today.Date, today.Timezone, len(today.Occurrences),
		today.Counts.Total, today.Counts.Resolved, today.Counts.Outstanding, verdict(shape))
	if res.Header().Get("Cache-Control") != "no-store" {
		fmt.Printf("  /api/today Cache-Control %q  %s\n", res.Header().Get("Cache-Control"), verdict(false))
	}

	// 2. The page: no third-party origin anywhere in what is rendered.
	page := do(http.MethodGet, "/app/", "")
	html := page.Body.String()
	external := strings.Contains(html, "http://") || strings.Contains(html, "https://") || strings.Contains(html, "//cdn")
	fmt.Printf("  GET /app/               status %d  no-store=%v  external URL=%v  %s\n",
		page.Code, page.Header().Get("Cache-Control") == "no-store", external,
		verdict(page.Code == http.StatusOK && page.Header().Get("Cache-Control") == "no-store" && !external))

	// 3. Which buttons each status offers.
	pendRow, notRow := rowHTML(html, pending.ID), rowHTML(html, a.ID)
	pendOK := strings.Contains(pendRow, ">Done<") && strings.Contains(pendRow, ">Skip<") && !strings.Contains(pendRow, "Snooze")
	notOK := strings.Contains(notRow, ">Done<") && strings.Contains(notRow, ">Skip<") &&
		strings.Contains(notRow, ">Snooze<") && strings.Contains(notRow, ">Tomorrow<")
	fmt.Printf("  pending row offers      Done+Skip, no Snooze  %s\n", verdict(pendOK))
	fmt.Printf("  notified row offers     Done+Skip+Snooze menu  %s\n", verdict(notOK))

	// 4. Resolution the way the client does it, and the settle that follows.
	fragment := func() string { return do(http.MethodGet, "/app/today", "").Body.String() }
	post := func(id, path, body string) *httptest.ResponseRecorder {
		return do(http.MethodPost, "/api/occurrences/"+id+"/"+path, body)
	}

	applied := post(a.ID, "resolve", `{"status":"completed","note":null,"source":"web"}`)
	after, err := st.GetOccurrence(ctx, a.ID)
	if err != nil {
		return err
	}
	srcWeb := after.ResolutionSource != nil && *after.ResolutionSource == domain.ResolvedByWeb
	fmt.Printf("  tap Done                status %d  source=%s  %s\n", applied.Code, sourceOf(after.ResolutionSource),
		verdict(applied.Code == http.StatusOK && after.Status == domain.StatusCompleted && srcWeb))

	noop := post(a.ID, "resolve", `{"status":"completed","note":null,"source":"web"}`)
	again, err := st.GetOccurrence(ctx, a.ID)
	if err != nil {
		return err
	}
	fmt.Printf("  tap Done again          status %d  nothing rewritten=%v  %s\n", noop.Code,
		again.ResolvedAt != nil && after.ResolvedAt != nil && again.ResolvedAt.Equal(*after.ResolvedAt),
		verdict(noop.Code == http.StatusOK))

	conflict := post(a.ID, "resolve", `{"status":"skipped","note":null,"source":"web"}`)
	settled := rowHTML(fragment(), a.ID)
	fmt.Printf("  tap Skip after Done     status %d  current_state=%q  settled row is completed=%v  %s\n",
		conflict.Code, currentState(conflict.Body.Bytes()), strings.Contains(settled, "st-completed"),
		verdict(conflict.Code == http.StatusConflict && currentState(conflict.Body.Bytes()) == "completed" &&
			strings.Contains(settled, "st-completed") && !strings.Contains(settled, "st-skipped") &&
			!strings.Contains(settled, "<button")))

	// A snooze on a pending row is the 409 the UI never offers; the settle must
	// leave it pending.
	badSnooze := post(pending.ID, "snooze", `{"delta":"1h","source":"web"}`)
	pendAfter := rowHTML(fragment(), pending.ID)
	fmt.Printf("  snooze a pending row    status %d  settled row is pending=%v  %s\n", badSnooze.Code,
		strings.Contains(pendAfter, "st-pending"),
		verdict(badSnooze.Code == http.StatusConflict && strings.Contains(pendAfter, "st-pending")))

	// Snooze on the notified one: the parent settles to snoozed.
	snz := post(b.ID, "snooze", `{"delta":"10m","source":"web"}`)
	snzRow := rowHTML(fragment(), b.ID)
	fmt.Printf("  snooze a notified row   status %d  settled row is snoozed=%v  %s\n", snz.Code,
		strings.Contains(snzRow, "st-snoozed"),
		verdict(snz.Code == http.StatusOK && strings.Contains(snzRow, "st-snoozed") && !strings.Contains(snzRow, "<button")))

	// Early resolution of a pending row from the page.
	early := post(pending.ID, "resolve", `{"status":"skipped","note":null,"source":"web"}`)
	pendDone, err := st.GetOccurrence(ctx, pending.ID)
	if err != nil {
		return err
	}
	fmt.Printf("  tap Skip on pending     status %d  source=%s  %s\n", early.Code, sourceOf(pendDone.ResolutionSource),
		verdict(early.Code == http.StatusOK && pendDone.Status == domain.StatusSkipped &&
			pendDone.ResolutionSource != nil && *pendDone.ResolutionSource == domain.ResolvedByWeb))

	wantSeries := `navi_occurrence_transitions_total{from="notified",source="web",to="completed"} 1`
	mrec := httptest.NewRecorder()
	m.Handler().ServeHTTP(mrec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	fmt.Printf("  one transition counted  %s  %s\n", wantSeries, verdict(strings.Contains(mrec.Body.String(), wantSeries)))

	// 5. The service worker and the shell.
	sw := do(http.MethodGet, "/app/sw.js", "")
	list := regexp.MustCompile(`const PRECACHE = \[(.*)\];`).FindStringSubmatch(sw.Body.String())
	precache := ""
	if len(list) == 2 {
		precache = list[1]
	}
	swOK := sw.Code == http.StatusOK && sw.Header().Get("Cache-Control") == "no-cache" &&
		!strings.Contains(sw.Body.String(), "__VERSION__") && !strings.Contains(sw.Body.String(), "__PRECACHE__") &&
		strings.Contains(precache, `"/app/offline"`) && strings.Contains(precache, "/app/static/app.js?v=") &&
		!strings.Contains(precache, "/api") && !strings.Contains(precache, "/app/today") &&
		!strings.Contains(precache, `"/app/"`)
	fmt.Printf("  GET /app/sw.js          status %d  precache is shell only  %s\n", sw.Code, verdict(swOK))

	// The worker's fetch handler must not answer anything under /api or the
	// fragment. Both are checked against the source because there is no browser
	// here; the rule it states is "respondWith appears only for static and for a
	// navigation".
	swSrc := sw.Body.String()
	respondWith := strings.Count(swSrc, "event.respondWith(")
	neverAPI := !strings.Contains(swSrc, "'/api") && !strings.Contains(swSrc, `"/api`) &&
		strings.Contains(swSrc, "req.method !== 'GET'") && respondWith == 2
	fmt.Printf("  worker answers          %d branch(es), no /api path, non-GET refused  %s\n", respondWith, verdict(neverAPI))

	manifest := do(http.MethodGet, "/app/manifest.webmanifest", "")
	var mf struct {
		StartURL string `json:"start_url"`
		Scope    string `json:"scope"`
		Display  string `json:"display"`
		Icons    []struct {
			Src   string `json:"src"`
			Sizes string `json:"sizes"`
		} `json:"icons"`
	}
	_ = json.Unmarshal(manifest.Body.Bytes(), &mf)
	iconsOK, sizes := len(mf.Icons) > 0, map[string]bool{}
	for _, ic := range mf.Icons {
		sizes[ic.Sizes] = true
		r := do(http.MethodGet, ic.Src, "")
		iconsOK = iconsOK && r.Code == http.StatusOK && r.Header().Get("Content-Type") == "image/png"
	}
	fmt.Printf("  GET manifest            start_url=%s display=%s 192+512 icons served=%v  %s\n",
		mf.StartURL, mf.Display, iconsOK && sizes["192x192"] && sizes["512x512"],
		verdict(manifest.Code == http.StatusOK && mf.StartURL == "/app/" && mf.Scope == "/app/" &&
			mf.Display == "standalone" && iconsOK && sizes["192x192"] && sizes["512x512"]))

	static := do(http.MethodGet, "/app/static/htmx.min.js", "")
	hidden := do(http.MethodGet, "/app/static/sw.js", "")
	redirect := do(http.MethodGet, "/app", "")
	offline := do(http.MethodGet, "/app/offline", "")
	fmt.Printf("  static asset            status %d  immutable=%v  worker not served as static=%v  /app -> %s  offline page %d  %s\n",
		static.Code, strings.Contains(static.Header().Get("Cache-Control"), "immutable"),
		hidden.Code == http.StatusNotFound, redirect.Header().Get("Location"), offline.Code,
		verdict(static.Code == http.StatusOK && strings.Contains(static.Header().Get("Cache-Control"), "immutable") &&
			hidden.Code == http.StatusNotFound && redirect.Header().Get("Location") == "/app/" && offline.Code == http.StatusOK))

	return nil
}

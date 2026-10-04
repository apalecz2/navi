package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"time"

	"github.com/aidenpaleczny/navi/internal/agent"
	"github.com/aidenpaleczny/navi/internal/config"
	"github.com/aidenpaleczny/navi/internal/defaults"
	"github.com/aidenpaleczny/navi/internal/domain"
	"github.com/aidenpaleczny/navi/internal/health"
	"github.com/aidenpaleczny/navi/internal/httpapi"
	"github.com/aidenpaleczny/navi/internal/materializer"
	"github.com/aidenpaleczny/navi/internal/metrics"
	"github.com/aidenpaleczny/navi/internal/schedule"
	"github.com/aidenpaleczny/navi/internal/stats"
	"github.com/aidenpaleczny/navi/internal/store"
)

// statsClass is a fixture chain's intended outcome, written by hand next to the
// day it sits on. Every expectation below is derived from this table and from
// the instants the fixture chose, never from a stats call, so an assertion
// compares the aggregation to a count a person did.
type statsClass string

const (
	scDone    statsClass = "completed"
	scMissed  statsClass = "missed"
	scSkipped statsClass = "skipped"
	scAwait   statsClass = "awaiting"
	scOpen    statsClass = "open"
)

type fxChain struct {
	daysAgo int
	class   statsClass

	// done is the local time of completion for a completed chain, which is what
	// the heatmap buckets by. lagMin is minutes from the first notification to it,
	// or -1 for a completion that has no lag (resolved before it was ever sent).
	done   time.Time
	lagMin int
}

// reportStats is the exit criterion for the statistics: every definition in
// internal/stats' package doc, checked against hand counts on a fixture built to
// hit each one, and then get_stats and /api/stats/summary shown to return the
// same bytes for the same range.
//
// The fixture goes through real surfaces wherever the store allows it -
// ResolveOccurrence, SnoozeOccurrence, delete_item, create_goal - so a chain here
// is a chain the system could make. The one exception is two timestamps:
// notified_at is stamped by the scheduler's real clock and reconciled_at by
// RecordCheckIn, which also advances a global latch, and a fixture spread over
// forty days needs both in the past. They are written with one UPDATE on a
// second connection (stampTimes), which touches nothing else.
//
// Each run builds fresh items under a unique title, so a second run on the same
// DATA_DIR counts its own rows and not the last run's. It runs after the other
// occurrence sections for the same reason the calendar does: it reads what they
// left and writes nothing they read.
func reportStats(ctx context.Context, st *store.Store, table *defaults.Table, dbPath, tz string, fallback *time.Location, log *slog.Logger) error {
	fmt.Println("\nstats  internal/stats, GET /api/stats/*, get_stats")

	zones, err := schedule.LoadZones(ctx, st, fallback)
	if err != nil {
		return err
	}
	loc := zones.Local()

	raw, err := sql.Open("sqlite", dbPath+"?_pragma=busy_timeout(10000)")
	if err != nil {
		return err
	}
	defer func() { _ = raw.Close() }()

	mat := materializer.New(log.With("component", "mat-stats"), st, fallback)
	tools := agent.New(st, mat, table, fallback, nil)
	svc := stats.New(st, fallback)
	srv := httpapi.New(config.HTTP{Addr: ":0"}, log.With("component", "httpapi"),
		health.New(), metrics.New(), st, mat, time.Time{}, fallback, nil,
		svc, nil)

	get := func(path string) (int, string) {
		rec := httptest.NewRecorder()
		srv.Handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil).WithContext(ctx))
		return rec.Code, strings.TrimSpace(rec.Body.String())
	}
	runID := strings.ToLower(domain.NewID()[16:])

	now := time.Now().In(loc)
	at := func(daysAgo, h, m int) time.Time {
		return time.Date(now.Year(), now.Month(), now.Day()-daysAgo, h, m, 0, 0, loc)
	}

	// ---- fixture ----------------------------------------------------------

	stamp := func(id string, notified, reconciled *time.Time) error {
		f := func(t *time.Time) any {
			if t == nil {
				return nil
			}
			return domain.FormatTime(*t)
		}
		_, err := raw.ExecContext(ctx,
			`UPDATE occurrences SET notified_at = COALESCE(?, notified_at), reconciled_at = COALESCE(?, reconciled_at) WHERE id = ?`,
			f(notified), f(reconciled), id)
		return err
	}
	root := func(item domain.Item, when time.Time, status domain.Status) (domain.Occurrence, error) {
		return st.CreateOccurrence(ctx, domain.NewOccurrence{
			ItemID: item.ID, StartsAt: when, IsOverride: true, Status: &status,
		})
	}
	resolve := func(id string, to domain.Status, note *string, src domain.ResolutionSource, when time.Time) error {
		res, err := st.ResolveOccurrence(ctx, id, to, note, src, when)
		if err != nil {
			return err
		}
		if res.Outcome != domain.OutcomeApplied {
			return fmt.Errorf("fixture: resolving %s to %s was %s, not applied", id, to, res.Outcome)
		}
		return nil
	}

	// Each builder returns the chain it made, with the expectation attached.
	completed := func(item domain.Item, d, lagMin int) (fxChain, error) {
		when := at(d, 9, 0)
		o, err := root(item, when, domain.StatusNotified)
		if err != nil {
			return fxChain{}, err
		}
		if err := stamp(o.ID, &when, nil); err != nil {
			return fxChain{}, err
		}
		done := when.Add(time.Duration(lagMin) * time.Minute)
		return fxChain{d, scDone, done, lagMin}, resolve(o.ID, domain.StatusCompleted, nil, domain.ResolvedByWeb, done)
	}
	missed := func(item domain.Item, d int) (fxChain, error) {
		when, asked := at(d, 9, 0), at(d, 21, 0)
		o, err := root(item, when, domain.StatusNotified)
		if err != nil {
			return fxChain{}, err
		}
		if err := stamp(o.ID, &when, &asked); err != nil {
			return fxChain{}, err
		}
		return fxChain{daysAgo: d, class: scMissed},
			resolve(o.ID, domain.StatusMissed, nil, domain.ResolvedByReconciler, at(d, 23, 59))
	}
	skipped := func(item domain.Item, d int, note string) (fxChain, error) {
		o, err := root(item, at(d, 9, 0), domain.StatusPending)
		if err != nil {
			return fxChain{}, err
		}
		return fxChain{daysAgo: d, class: scSkipped},
			resolve(o.ID, domain.StatusSkipped, &note, domain.ResolvedByWeb, at(d, 12, 0))
	}
	early := func(item domain.Item, d int) (fxChain, error) {
		o, err := root(item, at(d, 9, 0), domain.StatusPending)
		if err != nil {
			return fxChain{}, err
		}
		done := at(d, 8, 0)
		return fxChain{d, scDone, done, -1}, resolve(o.ID, domain.StatusCompleted, nil, domain.ResolvedByWeb, done)
	}
	snoozed := func(item domain.Item, d int) (fxChain, error) {
		when := at(d, 9, 0)
		o, err := root(item, when, domain.StatusNotified)
		if err != nil {
			return fxChain{}, err
		}
		if err := stamp(o.ID, &when, nil); err != nil {
			return fxChain{}, err
		}
		sn, err := st.SnoozeOccurrence(ctx, o.ID, domain.ResolvedByWeb, at(d, 9, 5),
			func(_ domain.Item, occ domain.Occurrence) (time.Time, error) { return occ.StartsAt.Add(time.Hour), nil })
		if err != nil {
			return fxChain{}, err
		}
		done := at(d, 10, 30)
		// The lag is from the first ask, 09:00, not from the snooze: 90 minutes.
		return fxChain{d, scDone, done, 90}, resolve(sn.Child.ID, domain.StatusCompleted, nil, domain.ResolvedByWeb, done)
	}
	awaiting := func(item domain.Item, d int) (fxChain, error) {
		when, asked := at(d, 9, 0), at(d, 21, 0)
		o, err := root(item, when, domain.StatusNotified)
		if err != nil {
			return fxChain{}, err
		}
		return fxChain{daysAgo: d, class: scAwait}, stamp(o.ID, &when, &asked)
	}
	open := func(item domain.Item, d int) (fxChain, error) {
		when := at(d, 9, 0)
		o, err := root(item, when, domain.StatusNotified)
		if err != nil {
			return fxChain{}, err
		}
		return fxChain{daysAgo: d, class: scOpen}, stamp(o.ID, &when, nil)
	}

	newItem := func(label string) (domain.Item, error) {
		return seedFireItem(ctx, st, "stats fixture "+label+" "+runID, domain.NotifyAtTime, tz)
	}
	var fail error
	add := func(c fxChain, err error) fxChain {
		if err != nil && fail == nil {
			fail = err
		}
		return c
	}

	itemA, err := newItem("A")
	if err != nil {
		return err
	}
	// Oldest first. Read the comments as the hand count: C C M C C S C C M C C ?
	//   - d10 and d4 are misses, and they end a run.
	//   - d8 is one snooze chain (two rows); it completes at its second link.
	//   - d7 is a skip with a note, and d1 is awaiting a check-in answer: neither
	//     may end a run and neither may extend one.
	//   - d5 was resolved early, so it has no notification and so no lag.
	chainsA := []fxChain{
		add(completed(itemA, 12, 10)),
		add(completed(itemA, 11, 20)),
		add(missed(itemA, 10)),
		add(completed(itemA, 9, 30)),
		add(snoozed(itemA, 8)),
		add(skipped(itemA, 7, "on vacation")),
		add(completed(itemA, 6, 40)),
		add(early(itemA, 5)),
		add(missed(itemA, 4)),
		add(completed(itemA, 3, 50)),
		add(completed(itemA, 2, 60)),
		add(awaiting(itemA, 1)),
	}

	itemB, err := newItem("B")
	if err != nil {
		return err
	}
	chainsB := []fxChain{
		add(completed(itemB, 3, 15)),
		add(open(itemB, 2)),
		add(skipped(itemB, 1, "sick")),
	}

	itemC, err := newItem("C")
	if err != nil {
		return err
	}
	chainsC := []fxChain{
		add(completed(itemC, 40, 5)),
		add(completed(itemC, 4, 5)),
		add(completed(itemC, 3, 25)),
	}
	itemD, err := newItem("D")
	if err != nil {
		return err
	}
	chainsD := []fxChain{add(completed(itemD, 40, 5))}
	if fail != nil {
		return fail
	}

	// C and D are archived through the real tool. Their history must survive.
	for _, it := range []domain.Item{itemC, itemD} {
		if _, err := callTool(ctx, tools, "delete_item", agent.DeleteItemArgs{ItemID: it.ID, Confirmed: true}); err != nil {
			return err
		}
	}

	occsA, err := st.ListOccurrencesForItem(ctx, itemA.ID)
	if err != nil {
		return err
	}
	fmt.Printf("  fixture  A: %d rows -> %d chains   B: %d chains   C: %d (archived)   D: %d (archived)\n",
		len(occsA), len(chainsA), len(chainsB), len(chainsC), len(chainsD))
	fmt.Printf("  a snooze chain is two rows: A has %d rows and %d chains  %s\n",
		len(occsA), len(chainsA), verdict(len(occsA) == len(chainsA)+1))

	// ---- helpers over the answers ------------------------------------------

	summary := func(q string) (stats.Summary, error) {
		code, body := get("/api/stats/summary?" + q)
		var s stats.Summary
		if code != http.StatusOK {
			return s, fmt.Errorf("summary %q: status %d: %s", q, code, body)
		}
		return s, json.Unmarshal([]byte(body), &s)
	}
	f4 := func(p *float64) string {
		if p == nil {
			return "null"
		}
		return fmt.Sprintf("%g", *p)
	}
	same := func(p *float64, want float64) bool { return p != nil && *p == want }

	// expected totals from a hand table, filtered by a window in days (0 = all).
	expect := func(chains []fxChain, within int) (t stats.Totals) {
		for _, c := range chains {
			if within > 0 && c.daysAgo > within-1 {
				continue
			}
			t.Chains++
			switch c.class {
			case scDone:
				t.Completed++
				t.Settled++
			case scMissed:
				t.Missed++
				t.Settled++
			case scSkipped:
				t.Skipped++
			case scAwait:
				t.Awaiting++
			case scOpen:
				t.Open++
			}
		}
		return t
	}
	// expected median lag over the hand table.
	expectLag := func(chains []fxChain, within int) (float64, int) {
		var lags []float64
		for _, c := range chains {
			if c.class == scDone && c.lagMin >= 0 && (within == 0 || c.daysAgo <= within-1) {
				lags = append(lags, float64(c.lagMin))
			}
		}
		if len(lags) == 0 {
			return -1, 0
		}
		for i := range lags { // insertion sort: no import to reach for
			for j := i; j > 0 && lags[j] < lags[j-1]; j-- {
				lags[j], lags[j-1] = lags[j-1], lags[j]
			}
		}
		n := len(lags)
		if n%2 == 1 {
			return lags[n/2], n
		}
		return (lags[n/2-1] + lags[n/2]) / 2, n
	}

	// ---- 1. the three definitions, item A, every range ----------------------

	fmt.Println("  definitions (item A: C C M C C S C C M C C ?)")
	type window struct {
		name   string
		within int // days; 0 = all
	}
	for _, w := range []window{{"all", 0}, {"quarter", 90}, {"month", 30}, {"week", 7}} {
		s, err := summary("range=" + w.name + "&item_id=" + itemA.ID)
		if err != nil {
			return err
		}
		if len(s.Items) != 1 {
			fmt.Printf("  %-8s expected one item row, got %d  %s\n", w.name, len(s.Items), verdict(false))
			continue
		}
		row := s.Items[0]
		want := expect(chainsA, w.within)
		lag, samples := expectLag(chainsA, w.within)
		wantRate := float64(want.Completed) / float64(want.Settled)
		lagOK := (lag < 0 && row.MedianLagMinutes == nil) || (lag >= 0 && same(row.MedianLagMinutes, lag))
		fmt.Printf("  %-8s totals %+v  rate %s (want %g)  lag %s min over %d (want %g over %d)  %s\n",
			w.name, row.Totals, f4(row.CompletionRate), wantRate, f4(row.MedianLagMinutes), row.LagSamples, lag, samples,
			verdict(row.Totals == want && s.Totals == want && same(row.CompletionRate, wantRate) &&
				same(s.CompletionRate, wantRate) && lagOK && row.LagSamples == samples && s.LagSamples == samples))

		// Streaks are lifetime, so they must not move with the range.
		fmt.Printf("  %-8s streak current %d longest %d (want 2, 4: a skip does not extend or break, awaiting neither)  %s\n",
			w.name, row.CurrentStreak, row.LongestStreak, verdict(row.CurrentStreak == 2 && row.LongestStreak == 4))
	}

	sNotes, err := summary("range=all&item_id=" + itemA.ID)
	if err != nil {
		return err
	}
	rowA := sNotes.Items[0]
	fmt.Printf("  skip note travels with the number: %q  %s\n", rowA.RecentSkipNotes,
		verdict(len(rowA.RecentSkipNotes) == 1 && rowA.RecentSkipNotes[0] == "on vacation"))
	fmt.Printf("  week window excludes the d7 skip, so no note there  %s\n", func() string {
		s, err := summary("range=week&item_id=" + itemA.ID)
		return verdict(err == nil && len(s.Items[0].RecentSkipNotes) == 0)
	}())
	fmt.Printf("  an early resolution has no lag: d5 is not a sample (7 of 8 completions)  %s\n",
		verdict(rowA.LagSamples == 7 && sNotes.Totals.Completed == 8))
	fmt.Printf("  snooze lag runs from the first ask: median 40 (30 if it ran from the last snooze)  %s\n",
		verdict(same(rowA.MedianLagMinutes, 40)))

	// B: a skip and an open chain are neither success nor failure.
	sB, err := summary("range=all&item_id=" + itemB.ID)
	if err != nil {
		return err
	}
	rowB := sB.Items[0]
	wantB := expect(chainsB, 0)
	fmt.Printf("  B totals %+v  rate %s (want 1: skip and open are not failures)  streak %d/%d  notes %q  %s\n",
		rowB.Totals, f4(rowB.CompletionRate), rowB.CurrentStreak, rowB.LongestStreak, rowB.RecentSkipNotes,
		verdict(rowB.Totals == wantB && same(rowB.CompletionRate, 1) && rowB.CurrentStreak == 1 &&
			rowB.LongestStreak == 1 && len(rowB.RecentSkipNotes) == 1 && rowB.RecentSkipNotes[0] == "sick" &&
			same(rowB.MedianLagMinutes, 15)))

	// ---- 2. archived items --------------------------------------------------

	fmt.Println("  archived items")
	for _, c := range []struct {
		name string
		item domain.Item
		all  []fxChain
	}{{"C", itemC, chainsC}, {"D", itemD, chainsD}} {
		s, err := summary("range=all&item_id=" + c.item.ID)
		if err != nil {
			return err
		}
		ok := len(s.Items) == 1 && s.Items[0].Archived && s.Items[0].Totals == expect(c.all, 0)
		fmt.Printf("  %s per-item, all: archived=%v totals %+v  %s\n", c.name,
			len(s.Items) == 1 && s.Items[0].Archived, s.Totals, verdict(ok))
	}
	for _, w := range []struct {
		name         string
		within       int
		wantC, wantD bool
	}{{"week", 7, true, false}, {"month", 30, true, false}, {"quarter", 90, true, true}, {"all", 0, true, true}} {
		s, err := summary("range=" + w.name)
		if err != nil {
			return err
		}
		var gotC, gotD bool
		var cRow stats.ItemStats
		firstArchived, lastActive := -1, -1
		for i, it := range s.Items {
			switch it.ItemID {
			case itemC.ID:
				gotC, cRow = true, it
			case itemD.ID:
				gotD = true
			}
			if it.Archived && firstArchived < 0 {
				firstArchived = i
			}
			if !it.Archived {
				lastActive = i
			}
		}
		lag, _ := expectLag(chainsC, w.within)
		ok := gotC == w.wantC && gotD == w.wantD && cRow.Totals == expect(chainsC, w.within) &&
			same(cRow.MedianLagMinutes, lag) && cRow.CurrentStreak == 3 && cRow.LongestStreak == 3 &&
			(firstArchived < 0 || lastActive < firstArchived)

		// Summary-level totals are the sum of the item rows: nothing is counted
		// that is not listed, and nothing listed is left out.
		var sum stats.Totals
		for _, it := range s.Items {
			sum.Chains += it.Totals.Chains
			sum.Completed += it.Totals.Completed
			sum.Skipped += it.Totals.Skipped
			sum.Missed += it.Totals.Missed
			sum.Awaiting += it.Totals.Awaiting
			sum.Open += it.Totals.Open
			sum.Settled += it.Totals.Settled
		}
		fmt.Printf("  %-8s unfiltered: C listed=%v D listed=%v  C streak %d (lifetime)  active before archived  totals = sum of items (%d chains)  %s\n",
			w.name, gotC, gotD, cRow.CurrentStreak, s.Totals.Chains, verdict(ok && sum == s.Totals))
	}

	// ---- 3. timeseries ------------------------------------------------------

	fmt.Println("  timeseries")
	getTS := func(q string) (stats.Timeseries, error) {
		code, body := get("/api/stats/timeseries?" + q)
		var ts stats.Timeseries
		if code != http.StatusOK {
			return ts, fmt.Errorf("timeseries %q: status %d: %s", q, code, body)
		}
		return ts, json.Unmarshal([]byte(body), &ts)
	}
	byDay := map[int]fxChain{}
	for _, c := range chainsA {
		byDay[c.daysAgo] = c
	}
	{
		ts, err := getTS("range=week&bucket=day&item_id=" + itemA.ID)
		if err != nil {
			return err
		}
		ok := len(ts.Buckets) == 7
		var detail []string
		for i, b := range ts.Buckets {
			d := 6 - i
			want := expect(func() []fxChain {
				if c, found := byDay[d]; found {
					return []fxChain{c}
				}
				return nil
			}(), 0)
			wantDate := at(d, 0, 0).Format(domain.DateLayout)
			rateOK := (want.Settled == 0 && b.CompletionRate == nil) || (want.Settled > 0 &&
				same(b.CompletionRate, float64(want.Completed)/float64(want.Settled)))
			ok = ok && b.Start == wantDate && b.Totals == want && rateOK
			detail = append(detail, fmt.Sprintf("%s:%dc/%dm/%dw/%s", b.Start[5:], b.Totals.Completed, b.Totals.Missed,
				b.Totals.Awaiting, f4(b.CompletionRate)))
		}
		fmt.Printf("  week by day (zero-filled, today empty, miss day is rate 0, awaiting day is null)  %s\n    %s\n",
			verdict(ok), strings.Join(detail, "  "))
	}
	{
		ts, err := getTS("range=month&bucket=week&item_id=" + itemA.ID)
		if err != nil {
			return err
		}
		monday := func(t time.Time) time.Time {
			for t.Weekday() != time.Monday {
				t = t.AddDate(0, 0, -1)
			}
			return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, loc)
		}
		wantTotals := map[string]*stats.Totals{}
		var order []string
		for b := monday(at(29, 0, 0)); !b.After(monday(at(0, 0, 0))); b = b.AddDate(0, 0, 7) {
			k := b.Format(domain.DateLayout)
			wantTotals[k] = &stats.Totals{}
			order = append(order, k)
		}
		for _, c := range chainsA {
			t := wantTotals[monday(at(c.daysAgo, 0, 0)).Format(domain.DateLayout)]
			one := expect([]fxChain{c}, 0)
			t.Chains += one.Chains
			t.Completed += one.Completed
			t.Skipped += one.Skipped
			t.Missed += one.Missed
			t.Awaiting += one.Awaiting
			t.Open += one.Open
			t.Settled += one.Settled
		}
		ok := len(ts.Buckets) == len(order)
		var sum int
		for i, b := range ts.Buckets {
			if i >= len(order) {
				break
			}
			ok = ok && b.Start == order[i] && b.Totals == *wantTotals[order[i]]
			sum += b.Totals.Chains
		}
		fmt.Printf("  month by week (Monday starts, matching goals): %d buckets, %d chains across them (want %d)  %s\n",
			len(ts.Buckets), sum, len(chainsA), verdict(ok && sum == len(chainsA)))
	}

	// ---- 4. heatmap ---------------------------------------------------------

	fmt.Println("  heatmap")
	weekdayIndex := map[time.Weekday]int{time.Monday: 0, time.Tuesday: 1, time.Wednesday: 2, time.Thursday: 3,
		time.Friday: 4, time.Saturday: 5, time.Sunday: 6}
	for _, w := range []window{{"all", 0}, {"week", 7}} {
		code, body := get("/api/stats/heatmap?range=" + w.name + "&item_id=" + itemA.ID)
		var hm stats.Heatmap
		if code != http.StatusOK || json.Unmarshal([]byte(body), &hm) != nil {
			return fmt.Errorf("heatmap %s: status %d: %s", w.name, code, body)
		}
		var want [7][24]int
		total, max := 0, 0
		for _, c := range chainsA {
			if c.class != scDone || (w.within > 0 && c.daysAgo > w.within-1) {
				continue
			}
			d, h := weekdayIndex[c.done.In(loc).Weekday()], c.done.In(loc).Hour()
			want[d][h]++
			total++
			if want[d][h] > max {
				max = want[d][h]
			}
		}
		fmt.Printf("  %-4s %d completed chains bucketed by local weekday and hour (want %d; the snooze chain counts once, at its completing link)  %s\n",
			w.name, hm.Total, total, verdict(hm.Cells == want && hm.Total == total && hm.Max == max))
	}

	// ---- 5. goals -----------------------------------------------------------

	fmt.Println("  goals (from the reads the context block uses)")
	linked, err := callTool(ctx, tools, "create_goal", agent.CreateGoalArgs{
		Title: "stats linked " + runID, PeriodKind: "week", ItemID: &itemA.ID, TargetCount: intp(3),
	})
	if err != nil {
		return err
	}
	free, err := callTool(ctx, tools, "create_goal", agent.CreateGoalArgs{
		Title: "stats freestanding " + runID, PeriodKind: "week",
	})
	if err != nil {
		return err
	}
	if _, err := callTool(ctx, tools, "log_goal_progress", agent.LogGoalProgressArgs{GoalID: free.Goal.ID, ProgressPct: intp(60)}); err != nil {
		return err
	}

	// The hand count: A's completed chains whose root falls in this local
	// calendar week, Monday through today.
	sinceMonday := 0
	for d := at(0, 0, 0); d.Weekday() != time.Monday; d = d.AddDate(0, 0, -1) {
		sinceMonday++
	}
	handWeek := 0
	for _, c := range chainsA {
		if c.class == scDone && c.daysAgo <= sinceMonday {
			handWeek++
		}
	}
	sAll, err := summary("range=month")
	if err != nil {
		return err
	}
	var gl, gf *stats.GoalStats
	for i := range sAll.Goals {
		switch sAll.Goals[i].GoalID {
		case linked.Goal.ID:
			gl = &sAll.Goals[i]
		case free.Goal.ID:
			gf = &sAll.Goals[i]
		}
	}
	if gl == nil || gf == nil {
		fmt.Printf("  goal rows present in the summary: linked=%v freestanding=%v  %s\n", gl != nil, gf != nil, verdict(false))
	} else {
		weekStartInstant := time.Date(now.Year(), now.Month(), now.Day()-sinceMonday, 0, 0, 0, 0, loc)
		viaCount, err := st.CountCompletedChains(ctx, itemA.ID, weekStartInstant, weekStartInstant.AddDate(0, 0, 7))
		if err != nil {
			return err
		}
		// Velocity is the existing GoalProgress.Velocity over the same read, to two
		// places; the summary must hand it through rather than recompute it.
		prog, err := st.GoalProgressFor(ctx, *linked.Goal, loc)
		if err != nil {
			return err
		}
		wantVel := math.Round(*prog.Velocity(now, loc)*100) / 100
		fmt.Printf("  linked: completed %v of %v this week (hand count %d, CountCompletedChains %d), velocity %s/wk  freestanding: %s%%  %s\n",
			ptrInt(gl.Completed), ptrInt(gl.Target), handWeek, viaCount, f4(gl.Velocity), ptrInt(gf.ProgressPct),
			verdict(gl.Completed != nil && *gl.Completed == handWeek && handWeek == viaCount &&
				gl.Target != nil && *gl.Target == 3 && same(gl.Velocity, wantVel) &&
				gf.ProgressPct != nil && *gf.ProgressPct == 60 && gf.Completed == nil && gl.ProgressPct == nil))
	}
	sGoalItem, err := summary("range=month&item_id=" + itemA.ID)
	if err != nil {
		return err
	}
	fmt.Printf("  item filter keeps only goals linked to that item (1 of 2 of mine)  %s\n",
		verdict(len(sGoalItem.Goals) == 1 && sGoalItem.Goals[0].GoalID == linked.Goal.ID))

	// ---- 6. get_stats and /api/stats/summary --------------------------------

	fmt.Println("  get_stats == /api/stats/summary")
	identical, total := 0, 0
	var firstDiff string
	for _, item := range []string{"", itemA.ID, itemB.ID, itemC.ID, itemD.ID} {
		for _, rng := range []string{"week", "month", "quarter", "all"} {
			args := agent.GetStatsArgs{Range: rng}
			q := "range=" + rng
			if item != "" {
				id := item
				args.ItemID = &id
				q += "&item_id=" + item
			}
			res, err := callTool(ctx, tools, "get_stats", args)
			if err != nil {
				return err
			}
			toolJSON, err := json.Marshal(res.Stats)
			if err != nil {
				return err
			}
			_, httpJSON := get("/api/stats/summary?" + q)
			total++
			if string(toolJSON) == httpJSON {
				identical++
			} else if firstDiff == "" {
				firstDiff = fmt.Sprintf("%s\n      tool %s\n      http %s", q, toolJSON, httpJSON)
			}
		}
	}
	fmt.Printf("  byte-identical for %d of %d (every range x whole database, A, B, archived C, archived D)  %s\n",
		identical, total, verdict(identical == total))
	if firstDiff != "" {
		fmt.Println("    first difference:", firstDiff)
	}
	// The default range is the same on both sides.
	{
		res, err := callTool(ctx, tools, "get_stats", agent.GetStatsArgs{})
		if err != nil {
			return err
		}
		toolJSON, _ := json.Marshal(res.Stats)
		_, httpJSON := get("/api/stats/summary")
		fmt.Printf("  default range: tool %q, http %q  %s\n", res.Stats.Range, stats.DefaultRange,
			verdict(string(toolJSON) == httpJSON && res.Stats.Range == "month"))
	}

	// ---- 7. validation ------------------------------------------------------

	fmt.Println("  validation")
	type bad struct{ name, path, field, want string }
	for _, c := range []bad{
		{"range", "/api/stats/summary?range=year", "range", `"year"`},
		{"bucket", "/api/stats/timeseries?bucket=hour", "bucket", `"hour"`},
		{"item id shape", "/api/stats/heatmap?item_id=nope", "item_id", `"nope"`},
		{"unknown item", "/api/stats/summary?item_id=01ARZ3NDEKTSV4RRFFQ69G5FAV", "item_id", "does not exist"},
	} {
		code, body := get(c.path)
		var e struct{ Error, Message, Field string }
		_ = json.Unmarshal([]byte(body), &e)
		fmt.Printf("  reject %-14s status %d  field=%-8s %q  %s\n", c.name, code, e.Field, e.Message,
			verdict(code == http.StatusBadRequest && e.Error == "validation_failed" && e.Field == c.field &&
				strings.Contains(e.Message, c.want)))
	}
	_, errRange := callTool(ctx, tools, "get_stats", map[string]any{"range": "year"})
	_, errItem := callTool(ctx, tools, "get_stats", map[string]any{"item_id": "01ARZ3NDEKTSV4RRFFQ69G5FAV"})
	fmt.Printf("  tool rejects range=year (%v) and an unknown item (%v)  %s\n", errRange != nil, errItem != nil,
		verdict(errRange != nil && errItem != nil))

	inCatalog := false
	for _, tl := range tools.Catalog() {
		if tl.Name == "get_stats" && strings.Contains(string(tl.Parameters), `"quarter"`) {
			inCatalog = true
		}
	}
	fmt.Printf("  get_stats is in the catalog with its range enum  %s\n", verdict(inCatalog))

	// ---- 8. the view -------------------------------------------------------
	//
	// E is the one fixture built for the view: thirty-five consecutive days of a
	// completion each, so a rate line, a populated grid and five-plus weekly
	// buckets exist on any day the file is run. It is created last so nothing
	// above counts it.
	itemE, err := newItem("E")
	if err != nil {
		return err
	}
	for d := 35; d >= 1; d-- {
		add(completed(itemE, d, 10))
	}
	if fail != nil {
		return fail
	}
	return reportStatsView(ctx, st, tools, svc, fallback, log, statsViewFixture{
		itemA: itemA, itemB: itemB, itemE: itemE, chainsA: chainsA,
		linked: linked.Goal.ID, free: free.Goal.ID, runID: runID, sinceMonday: sinceMonday,
	})
}

func intp(n int) *int { return &n }

func ptrInt(p *int) string {
	if p == nil {
		return "null"
	}
	return fmt.Sprintf("%d", *p)
}

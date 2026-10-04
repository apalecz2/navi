package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/aidenpaleczny/navi/internal/domain"
	"github.com/aidenpaleczny/navi/internal/stats"
)

// Stats is what the statistics view reads: *stats.Service, the same value the
// /api/stats routes and the agent's get_stats hold. The page calls it in process
// and renders what comes back, rather than fetching its own API, so there is no
// second serialisation to drift and no client that could compute a figure. The
// charts are handed the service's typed values marshalled as they are (the same
// bytes the API route would send), and the script only draws them.
type Stats interface {
	Summary(ctx context.Context, q stats.Query) (stats.Summary, error)
	Timeseries(ctx context.Context, q stats.Query) (stats.Timeseries, error)
	Heatmap(ctx context.Context, q stats.Query) (stats.Heatmap, error)
	GoalProgress(ctx context.Context, id string) (stats.GoalSeries, error)
}

// The empty-state thresholds. A chart under them is replaced by a sentence that
// says what it is waiting for and how far along it is; a week of history drawn
// as a three-point line or a nearly blank grid reads as broken rather than
// sparse. These are presentation policy, not statistics, so they live here and
// not in internal/stats; they compare counts the service returned and compute
// nothing. Each is evaluated once, server-side, in NewStatsPage.
const (
	// MinSettled is how many settled chains (completed or missed) the window needs
	// before a completion-rate line is drawn. Below it one chain moves the rate by
	// ten points or more.
	MinSettled = 10
	// MinRateBuckets is how many buckets must have a rate (null buckets are gaps)
	// for the line to be a line.
	MinRateBuckets = 3
	// MinCompletions is how many completions the time-of-day grid needs.
	MinCompletions = 10
	// MinGoalPoints is how many points a goal's trail needs.
	MinGoalPoints = 2
)

// StatsPage is everything /app/stats renders.
type StatsPage struct {
	Summary stats.Summary

	Ranges      []RangeLink
	RangeHeader string // "Over the last 30 days"
	ItemID      string
	ItemTitle   string
	AllHref     string // the same range, every item

	Rate  RateChart
	Heat  HeatView
	Items []ItemRow
	Goals []GoalView

	Rated    string // headline completion rate, or a dash
	RatedSub string
	Lag      string
	LagSub   string
}

// RangeLink is one pill of the range selector.
type RangeLink struct {
	Label    string
	Href     string
	BodyHref string
	Current  bool
}

// RateChart is the completion-rate-over-time plot, or why there is not one.
type RateChart struct {
	Ready bool
	Why   string
	JSON  string // json.Marshal(stats.Timeseries)
	Label string // accessible name
}

// HeatView is the weekday-by-hour grid. It is a table, not a canvas: 168 cells
// of one number each are what a table is for, every cell has a text value for a
// screen reader, and the shading is one CSS expression over --n and --max, which
// are the service's own Cells value and Max.
type HeatView struct {
	Ready bool
	Why   string
	Map   stats.Heatmap
	Hours []int // the header labels, each over six columns
}

// ItemRow is one item's line: stats.ItemStats and the words for it.
type ItemRow struct {
	stats.ItemStats
	Streak   string
	Meta     string // "100% done · median 10 min · skipped: on vacation", parts omitted when absent
	Href     string
}

// GoalView is one goal: its summary row, its series, and whether the series is
// long enough to draw.
type GoalView struct {
	Row      stats.GoalStats
	Series   stats.GoalSeries
	JSON     string
	Headline string
	Ready    bool
	Why      string
	// Velocity is whether a velocity plot is offered, which is whether the series
	// carries velocity at all: an item-linked goal's does, a freestanding one's
	// never does and no rate is invented for it.
	Velocity bool
}

// NewStatsPage reads the three statistics and every listed goal's series and
// shapes them. q is already validated.
func NewStatsPage(ctx context.Context, svc Stats, q stats.Query) (StatsPage, error) {
	// Day buckets for a week, week buckets for anything longer: a daily rate over
	// a month is one chain a day, so every point is 0% or 100%.
	if q.Range == stats.RangeWeek {
		q.Bucket = stats.BucketDay
	} else {
		q.Bucket = stats.BucketWeek
	}

	sum, err := svc.Summary(ctx, q)
	if err != nil {
		return StatsPage{}, err
	}
	ts, err := svc.Timeseries(ctx, q)
	if err != nil {
		return StatsPage{}, err
	}
	hm, err := svc.Heatmap(ctx, q)
	if err != nil {
		return StatsPage{}, err
	}

	p := StatsPage{Summary: sum, RangeHeader: stats.RangePhrase(sum.Range), ItemID: q.ItemID}
	p.Ranges = rangeLinks(q)
	p.AllHref = statsHref("/app/stats", q.Range, "")

	if q.ItemID != "" && len(sum.Items) > 0 {
		p.ItemTitle = sum.Items[0].Title
	}

	p.Rated = "-"
	switch {
	case sum.CompletionRate != nil:
		p.Rated = stats.Percent(*sum.CompletionRate)
		p.RatedSub = fmt.Sprintf("%d of %d completed", sum.Totals.Completed, sum.Totals.Settled)
		if rest := sum.Totals.Others(); rest != "" {
			p.RatedSub += "; " + rest
		}
	case sum.Totals.Chains == 0:
		p.RatedSub = "nothing was scheduled"
	default:
		p.RatedSub = "nothing has settled yet (" + sum.Totals.Unsettled() + ")"
	}
	p.Lag, p.LagSub = "-", "no completed reminder was sent first"
	if sum.MedianLagMinutes != nil {
		p.Lag = stats.MinutesPhrase(*sum.MedianLagMinutes)
		p.LagSub = fmt.Sprintf("from reminder to done, over %d", sum.LagSamples)
	}

	p.Rate = rateChart(sum, ts)
	p.Heat = heatView(hm)

	for _, it := range sum.Items {
		row := ItemRow{ItemStats: it, Streak: stats.StreakPhrase(it.CurrentStreak, it.LongestStreak)}
		var parts []string
		if it.CompletionRate != nil {
			parts = append(parts, stats.Percent(*it.CompletionRate)+" done")
		}
		if it.MedianLagMinutes != nil {
			parts = append(parts, "median "+stats.MinutesPhrase(*it.MedianLagMinutes))
		}
		if len(it.RecentSkipNotes) > 0 {
			parts = append(parts, "skipped: "+strings.Join(it.RecentSkipNotes, "; "))
		}
		if len(parts) == 0 {
			parts = append(parts, "nothing settled in this window")
		}
		row.Meta = strings.Join(parts, " · ")
		row.Href = statsHref("/app/stats", q.Range, it.ItemID)
		p.Items = append(p.Items, row)
	}

	for _, g := range sum.Goals {
		series, err := svc.GoalProgress(ctx, g.GoalID)
		if err != nil {
			return StatsPage{}, fmt.Errorf("web: goal %s: %w", g.GoalID, err)
		}
		p.Goals = append(p.Goals, goalView(g, series))
	}
	return p, nil
}

func rateChart(sum stats.Summary, ts stats.Timeseries) RateChart {
	rated := 0
	for _, b := range ts.Buckets {
		if b.CompletionRate != nil {
			rated++
		}
	}
	c := RateChart{Label: "Completion rate over time. " + stats.RangePhrase(sum.Range)}
	switch {
	case sum.Totals.Settled < MinSettled:
		c.Why = fmt.Sprintf("Not enough data yet: %d of the %d settled reminders this needs. A line through fewer than that would swing on a single answer.",
			sum.Totals.Settled, MinSettled)
	case rated < MinRateBuckets:
		c.Why = fmt.Sprintf("Not enough data yet: %d of the %d %ss with a rate this needs.", rated, MinRateBuckets, ts.Bucket)
	default:
		c.Ready, c.JSON = true, mustJSON(ts)
	}
	return c
}

func heatView(hm stats.Heatmap) HeatView {
	h := HeatView{Map: hm}
	h.Hours = []int{0, 6, 12, 18} // header labels, each spanning six columns
	if hm.Total < MinCompletions {
		h.Why = fmt.Sprintf("Not enough data yet: %d of the %d completions this needs. With fewer, the grid is mostly one or two lit cells.",
			hm.Total, MinCompletions)
		return h
	}
	h.Ready = true
	return h
}

func goalView(row stats.GoalStats, s stats.GoalSeries) GoalView {
	v := GoalView{Row: row, Series: s, JSON: mustJSON(s), Headline: stats.GoalPhrase(row), Velocity: s.Kind == stats.GoalKindItemLinked}
	have := 0
	for _, pt := range s.Points {
		if pt.Value != nil {
			have++
		}
	}
	switch {
	case have >= MinGoalPoints:
		v.Ready = true
	case s.Kind == stats.GoalKindFreestanding:
		v.Why = fmt.Sprintf("Not enough data yet: a trail needs at least %d progress updates, and there are %d.", MinGoalPoints, have)
	default:
		v.Why = fmt.Sprintf("Not enough data yet: the line needs at least %d days of the period, and %d have passed.", MinGoalPoints, have)
	}
	return v
}

func mustJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return "null"
	}
	return string(b)
}

func rangeLinks(q stats.Query) []RangeLink {
	labels := map[stats.Range]string{
		stats.RangeWeek: "7 days", stats.RangeMonth: "30 days", stats.RangeQuarter: "90 days", stats.RangeAll: "All",
	}
	out := make([]RangeLink, 0, len(stats.Ranges))
	for _, r := range stats.Ranges {
		out = append(out, RangeLink{
			Label: labels[r], Current: r == q.Range,
			Href:     statsHref("/app/stats", r, q.ItemID),
			BodyHref: statsHref("/app/stats/body", r, q.ItemID),
		})
	}
	return out
}

// statsHref builds a stats URL. The default range and no item are left out, so
// the bare address stays the canonical one.
func statsHref(base string, r stats.Range, item string) string {
	v := url.Values{}
	if r != stats.DefaultRange {
		v.Set("range", string(r))
	}
	if item != "" {
		v.Set("item", item)
	}
	if len(v) == 0 {
		return base
	}
	return base + "?" + v.Encode()
}

// statsQuery reads range= and item= through stats.NewQuery, the validation the
// API routes and get_stats use.
func statsQuery(vals url.Values) (stats.Query, error) {
	q, ve := stats.NewQuery(vals.Get("range"), vals.Get("item"), "")
	if ve != nil {
		return stats.Query{}, ve
	}
	return q, nil
}

// isValidation reports whether err is the caller's mistake.
func isValidation(err error) bool {
	var ve *domain.ValidationError
	return errors.As(err, &ve)
}

// statsBodyOf is the fragment URL for a page URL: same query, /body route.
func statsBodyOf(href string) string {
	return "/app/stats/body" + strings.TrimPrefix(href, "/app/stats")
}

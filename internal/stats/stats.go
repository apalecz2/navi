// Package stats is the one place a statistic is computed (V5, V6, O4).
//
// GET /api/stats/summary, /timeseries, /heatmap and the agent's get_stats tool
// all call Service and render what comes back. They format; they do not count.
// That is enforced by what each of them can reach rather than by care: the
// handlers hold a three-method interface whose results are the typed values
// below and are serialised as they are, the tool puts the same value in its
// Result for reply.go to phrase, and neither has a store method that returns a
// chain. If a number on the dashboard and a number in the chat can differ, one
// of them computed something, and this layout is what makes that a visible
// diff instead of a latent bug.
//
// Everything reads the chains view through store.StatsChains, never the
// occurrences table, so a snooze chain counts once (R7, D-011). Goal rows come
// from store.ListGoalProgress, the read the agent's context block and the
// morning briefing already use; there is no second goal query.
//
// # Definitions
//
// These are the spec's gap. 04-data-model.md names a chain's three outcomes and
// stops; what follows is the rest, and docs/04-data-model.md carries the same
// text.
//
// A chain is classified once, by Classify, into exactly one of five classes:
//
//	completed  any link completed (D-011). However late, and however many snoozes.
//	skipped    not completed, and the last link is skipped. A deliberate "not
//	           today" (R2), usually with a reason.
//	missed     not completed, and the last link is missed: snooze-cap exhaustion,
//	           or a check-in asked and the grace window closed on silence (K6).
//	awaiting   not resolved, but a check-in has asked about it. Still answerable.
//	open       not resolved and nobody has asked yet: pending, or notified and
//	           ignored so far.
//
// Completion rate is completed / (completed + missed): the chains that have
// settled as a success or a failure. Skipped is excluded from both sides. R2
// says a skip is distinct from a miss and "does not damage a streak the way a
// miss does", and a rate that counted it as a denominator row would punish
// exactly the honest "I was away" the skip exists to record; counting it as a
// success would reward saying "skip" over doing the thing. Awaiting and open
// are excluded because they are neither yet: a chain due at 21:00 and unresolved
// at 21:05 is a fact about the clock. A window with nothing settled has no rate,
// which is null and never 0 - the same rule /healthz follows for a field with no
// value.
//
// Streaks are per item, over that item's chains in scheduled order, up to now,
// and are lifetime figures independent of the range asked for (a "longest streak
// this week" is a number nobody wants). A completed chain extends the current
// run by one. A missed chain ends it. Nothing else touches it:
//
//   - a skipped chain neither breaks the run nor adds to it. It is not a failure
//     (R2), and it is not a success, so it must not silently extend the streak
//     either; the run waits across it.
//   - an awaiting or open chain does the same. The most recent chain still inside
//     its grace window is the case this exists for: "walk done?" asked at 21:00,
//     unanswered at 21:05, leaves the streak where it was until the answer or the
//     grace window decides. K6 is the same rule seen from the other side - only
//     asked-and-got-nothing is a miss, so only that ends a run.
//   - an old chain that is still open is treated the same way, which is
//     deliberate: nothing marks a chain missed on a clock alone, so neither may
//     this.
//
// V7 (a weekday item's streak is not broken by weekends) needs no code: a chain
// exists only for a day the schedule produced.
//
// Median lag is the time from a chain's first notification to its completion,
// over chains that were completed AND actually notified AND completed no earlier
// than notified. A silent item has no notified_at, so there is nothing to
// measure; a row resolved before it was ever sent was never claimed (R3), so
// neither is lag. A snoozed chain measures from the first ask (the root's
// notified_at), not the last snooze, which is what makes a chain pushed three
// times read as slow - D-011's stated cost, and the reason lag is tracked at
// all. Minutes, to one decimal; null when there are no samples.
//
// # Windows
//
// week, month and quarter are trailing windows of 7, 30 and 90 local calendar
// days ending today, in the device zone (schedule.Zones.Local(), the clock the
// day view and the check-in use): from local midnight N-1 days ago up to now. A
// chain is in the window when its root was scheduled inside it; chains due later
// today that have not happened are not in it. all is everything up to now.
//
// This is not the Monday calendar week goals use, and the difference is
// deliberate. A goal is a commitment about a named period, so its period is a
// calendar one. A statistic is a trailing measurement, and a calendar week would
// read as one data point on Monday morning and reset to empty every Monday; a
// calendar month would do the same on the 1st. Rolling windows also match the
// 30-day rate 06-agent-spec.md's context block already names.
//
// Timeseries buckets are local dates (day) or local Mondays (week, matching
// internal/materializer's periods and the goals). A chain is filed under the
// local date of its root's scheduled_at - "how did what was due that day go" -
// which is not the calendar's rule (it files a chain under its live link). The
// heatmap is the one place a different time is used: it buckets by when the
// chain was completed, in local weekday and hour, because "when do I actually
// do things" is what it answers.
//
// # Archived items
//
// Archived items stay in every aggregate and in "all" and per-item results,
// flagged archived. History is immutable (invariant 2); deleting a reminder
// does not un-happen it, and a completion rate that silently shrank when an old
// item was retired would be a statistic that depends on housekeeping. Active
// items are always listed in a summary even with no chains in the window (their
// streaks are lifetime); an archived item is listed only if it has chains in the
// window.
package stats

import (
	"strings"

	"github.com/aidenpaleczny/navi/internal/domain"
)

// Range names a trailing window. See the package doc's Windows section.
type Range string

const (
	RangeWeek    Range = "week"
	RangeMonth   Range = "month"
	RangeQuarter Range = "quarter"
	RangeAll     Range = "all"

	// DefaultRange is 06-agent-spec.md's "default month".
	DefaultRange = RangeMonth
)

// Ranges is the accepted set, in the order the spec lists them.
var Ranges = []Range{RangeWeek, RangeMonth, RangeQuarter, RangeAll}

// days is each trailing window's length in local calendar days; all has none.
func (r Range) days() int {
	switch r {
	case RangeWeek:
		return 7
	case RangeMonth:
		return 30
	case RangeQuarter:
		return 90
	}
	return 0
}

// Bucket is a timeseries granularity.
type Bucket string

const (
	BucketDay  Bucket = "day"
	BucketWeek Bucket = "week"

	DefaultBucket = BucketDay
)

// Query is what a caller asks. Build it with NewQuery, which is the one place
// the three entry points' parameters are validated.
type Query struct {
	Range  Range
	ItemID string // empty means every item
	Bucket Bucket // timeseries only
}

// NewQuery validates and defaults the parameters shared by the HTTP handlers and
// the get_stats tool, so the two cannot reject different things. Empty strings
// take their defaults. Every failure is a *domain.ValidationError naming the
// rule and the value, the shape the API envelope and the escalation ladder both
// consume.
//
// An item id is checked only for shape here; whether it names an item is the
// Service's question, because it needs the store.
func NewQuery(rangeS, itemID, bucketS string) (Query, *domain.ValidationError) {
	q := Query{Range: DefaultRange, Bucket: DefaultBucket, ItemID: itemID}

	if rangeS != "" {
		q.Range = Range(rangeS)
		ok := false
		for _, r := range Ranges {
			ok = ok || r == q.Range
		}
		if !ok {
			names := make([]string, len(Ranges))
			for i, r := range Ranges {
				names[i] = string(r)
			}
			return Query{}, domain.Invalid("stats_range", "range",
				"range %q is not one of %s", rangeS, strings.Join(names, ", "))
		}
	}

	if bucketS != "" {
		q.Bucket = Bucket(bucketS)
		if q.Bucket != BucketDay && q.Bucket != BucketWeek {
			return Query{}, domain.Invalid("stats_bucket", "bucket",
				"bucket %q is not one of day, week", bucketS)
		}
	}

	if itemID != "" && !domain.ValidID(itemID) {
		return Query{}, domain.Invalid("stats_item_id", "item_id",
			"item_id %q is not an item id", itemID)
	}
	return q, nil
}

// Class is a chain's single classification. See the package doc.
type Class string

const (
	ClassCompleted Class = "completed"
	ClassSkipped   Class = "skipped"
	ClassMissed    Class = "missed"
	ClassAwaiting  Class = "awaiting"
	ClassOpen      Class = "open"
)

// Totals counts chains by class. Settled is Completed + Missed, the rate's
// denominator, carried so a client never re-derives it.
type Totals struct {
	Chains    int `json:"chains"`
	Completed int `json:"completed"`
	Skipped   int `json:"skipped"`
	Missed    int `json:"missed"`
	Awaiting  int `json:"awaiting"`
	Open      int `json:"open"`
	Settled   int `json:"settled"`
}

// ItemStats is one item's row in a summary.
type ItemStats struct {
	ItemID   string `json:"item_id"`
	Title    string `json:"title"`
	Archived bool   `json:"archived"`

	// Totals, CompletionRate and the lag are over the window. The streaks are
	// lifetime.
	Totals           Totals   `json:"totals"`
	CompletionRate   *float64 `json:"completion_rate"`
	CurrentStreak    int      `json:"current_streak"`
	LongestStreak    int      `json:"longest_streak"`
	MedianLagMinutes *float64 `json:"median_lag_minutes"`
	LagSamples       int      `json:"lag_samples"`

	// RecentSkipNotes are the reasons on skipped chains in the window, newest
	// first, at most three, so "on vacation" travels with the number it explains.
	RecentSkipNotes []string `json:"recent_skip_notes"`
}

// GoalStats is one goal's row in a summary, taken from store.GoalProgress as it
// is. An item-linked goal reports Completed against Target and a Velocity; a
// freestanding one reports ProgressPct. The fields of the other kind are null.
type GoalStats struct {
	GoalID      string  `json:"goal_id"`
	Title       string  `json:"title"`
	Status      string  `json:"status"`
	PeriodKind  string  `json:"period_kind"`
	PeriodStart string  `json:"period_start"`
	PeriodEnd   string  `json:"period_end"`
	ItemID      *string `json:"item_id"`

	Completed *int     `json:"completed"`
	Target    *int     `json:"target"`
	Velocity  *float64 `json:"velocity_per_week"`
	Met       bool     `json:"met"`

	ProgressPct *int    `json:"progress_pct"`
	Note        *string `json:"note"`
}

// Summary is GET /api/stats/summary and the get_stats result.
type Summary struct {
	Range    string `json:"range"`
	Timezone string `json:"timezone"`

	// From and To are local dates, inclusive. From is null for range=all when
	// there is nothing to start from.
	From *string `json:"from"`
	To   string  `json:"to"`

	ItemID *string `json:"item_id"`

	Totals           Totals   `json:"totals"`
	CompletionRate   *float64 `json:"completion_rate"`
	MedianLagMinutes *float64 `json:"median_lag_minutes"`
	LagSamples       int      `json:"lag_samples"`

	Items []ItemStats `json:"items"`
	Goals []GoalStats `json:"goals"`
}

// TimeBucket is one timeseries point. Start is the bucket's first local date: the
// day itself, or the Monday of the week. The first bucket of a trailing window
// can be partial.
type TimeBucket struct {
	Start          string   `json:"start"`
	Totals         Totals   `json:"totals"`
	CompletionRate *float64 `json:"completion_rate"`
}

// Timeseries is GET /api/stats/timeseries.
type Timeseries struct {
	Range    string       `json:"range"`
	Bucket   string       `json:"bucket"`
	Timezone string       `json:"timezone"`
	From     *string      `json:"from"`
	To       string       `json:"to"`
	ItemID   *string      `json:"item_id"`
	Buckets  []TimeBucket `json:"buckets"`
}

// Heatmap is GET /api/stats/heatmap. Cells[d][h] counts chains completed on
// local weekday d (0 = Monday ... 6 = Sunday) in local hour h.
type Heatmap struct {
	Range    string     `json:"range"`
	Timezone string     `json:"timezone"`
	From     *string    `json:"from"`
	To       string     `json:"to"`
	ItemID   *string    `json:"item_id"`
	Weekdays []string   `json:"weekdays"`
	Total    int        `json:"total"`
	Max      int        `json:"max"`
	Cells    [7][24]int `json:"cells"`
}

// Weekdays labels Heatmap.Cells' first index.
var weekdayLabels = []string{"mon", "tue", "wed", "thu", "fri", "sat", "sun"}

func strPtr(s string) *string { return &s }

func (q Query) itemPtr() *string {
	if q.ItemID == "" {
		return nil
	}
	return strPtr(q.ItemID)
}

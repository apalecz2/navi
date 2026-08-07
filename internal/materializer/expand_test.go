// This is the single carve-out from S2 (D-020), and Q-14 in
// docs/10-open-questions.md records why it is the only one.
//
// Every other component in this system fails visibly: a stalled loop shows on
// the dashboard, a bad tool call is caught by the validator, a model outage
// sends the plain title. A materializer that places an occurrence an hour off
// twice a year fails invisibly and rarely, which is the one shape
// hand-verification is structurally bad at — you cannot notice in March that
// November will be wrong.
//
// So: three unverified behaviours, plus the two worked examples the schedule
// spec writes out. Weekly expansion across a DST boundary, BYDAY with a negative
// index, and the fold handling this package adds on top of the stdlib. Not a
// suite, not a framework, no coverage target, and explicitly not a precedent for
// testing anything else here.
package materializer

import (
	"encoding/json"
	"io"
	"log/slog"
	"math/rand/v2"
	"testing"
	"time"

	"github.com/aidenpaleczny/navi/internal/domain"
	"github.com/aidenpaleczny/navi/internal/schedule"
)

// localLayout carries the offset, because the offset is the entire point: a
// reminder that stays at 09:00 across a transition is one whose UTC instant
// moved by an hour, and a test that compared instants would assert the bug.
const localLayout = "2006-01-02 15:04 -0700"

// TestExpandAcrossDST walks recurrence rules over the two US transitions in
// 2026: forward on 8 March, back on 1 November.
func TestExpandAcrossDST(t *testing.T) {
	ny := location(t, "America/New_York")

	cases := []struct {
		name     string
		schedule string
		from     time.Time
		days     int
		want     []string
	}{
		{
			// Q-14's first unverified behaviour. rrule-go is expanding across
			// the March transition here, though only over dates: the offsets in
			// the expected values come from schedule.Instant.
			name:     "weekly across spring forward",
			schedule: `{"kind":"fixed","rrule":"FREQ=WEEKLY;BYDAY=SU","at":"09:00"}`,
			from:     time.Date(2026, 3, 1, 0, 0, 0, 0, ny),
			days:     30,
			want: []string{
				"2026-03-01 09:00 -0500",
				"2026-03-08 09:00 -0400",
				"2026-03-15 09:00 -0400",
				"2026-03-22 09:00 -0400",
				"2026-03-29 09:00 -0400",
			},
		},
		{
			// The session's own acceptance criterion: same wall clock on both
			// sides, which means a different instant on each.
			name:     "daily across spring forward",
			schedule: `{"kind":"fixed","rrule":"FREQ=DAILY","at":"09:00"}`,
			from:     time.Date(2026, 3, 6, 0, 0, 0, 0, ny),
			days:     5,
			want: []string{
				"2026-03-06 09:00 -0500",
				"2026-03-07 09:00 -0500",
				"2026-03-08 09:00 -0400",
				"2026-03-09 09:00 -0400",
				"2026-03-10 09:00 -0400",
			},
		},
		{
			name:     "daily across fall back",
			schedule: `{"kind":"fixed","rrule":"FREQ=DAILY","at":"09:00"}`,
			from:     time.Date(2026, 10, 30, 0, 0, 0, 0, ny),
			days:     4,
			want: []string{
				"2026-10-30 09:00 -0400",
				"2026-10-31 09:00 -0400",
				"2026-11-01 09:00 -0500",
				"2026-11-02 09:00 -0500",
			},
		},
		{
			// A fixed time that lands in the gap on exactly one day of the run.
			// 02:30 does not exist on 8 March, so that day fires at 03:00 and
			// every other day is untouched.
			name:     "daily through the gap",
			schedule: `{"kind":"fixed","rrule":"FREQ=DAILY","at":"02:30"}`,
			from:     time.Date(2026, 3, 6, 0, 0, 0, 0, ny),
			days:     4,
			want: []string{
				"2026-03-06 02:30 -0500",
				"2026-03-07 02:30 -0500",
				"2026-03-08 03:00 -0400",
				"2026-03-09 02:30 -0400",
			},
		},
		{
			// And one that lands in the fold. 01:30 happens twice on 1 November;
			// the earlier of the two is -0400.
			name:     "daily through the fold",
			schedule: `{"kind":"fixed","rrule":"FREQ=DAILY","at":"01:30"}`,
			from:     time.Date(2026, 10, 30, 0, 0, 0, 0, ny),
			days:     4,
			want: []string{
				"2026-10-30 01:30 -0400",
				"2026-10-31 01:30 -0400",
				"2026-11-01 01:30 -0400",
				"2026-11-02 01:30 -0500",
			},
		},
		{
			// Q-14's second unverified behaviour: the last Sunday of the month,
			// across a March whose last Sunday is its fifth and an April whose
			// is its fourth.
			name:     "monthly on a negative BYDAY index",
			schedule: `{"kind":"fixed","rrule":"FREQ=MONTHLY;BYDAY=-1SU","at":"10:00"}`,
			from:     time.Date(2026, 3, 1, 0, 0, 0, 0, ny),
			days:     60,
			want: []string{
				"2026-03-29 10:00 -0400",
				"2026-04-26 10:00 -0400",
			},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := expandLocal(t, c.schedule, ny, c.from, c.days)
			if len(got) != len(c.want) {
				t.Fatalf("got %d occurrences %v, want %d %v", len(got), got, len(c.want), c.want)
			}
			for i := range got {
				if got[i] != c.want[i] {
					t.Errorf("occurrence %d: got %s, want %s", i, got[i], c.want[i])
				}
			}
		})
	}
}

// TestInstantDST is the two worked examples from
// docs/05-schedule-spec.md#dst, plus the controls that keep them honest: the
// same clock an hour either side, where there is nothing to decide.
func TestInstantDST(t *testing.T) {
	ny := location(t, "America/New_York")

	cases := []struct {
		name  string
		local schedule.LocalDateTime
		want  string
		fold  schedule.Fold
	}{
		{
			// "Nonexistent local time (02:30 on a spring-forward day): shift
			// forward to the first valid time, 03:00."
			//
			// Not 03:30. time.Date resolves this one to 01:30 -0500 — an hour
			// before what was asked for, on the far side of the transition — so
			// the answer here is written rather than inherited.
			name:  "spring forward gap",
			local: schedule.LocalDateTime{Year: 2026, Month: time.March, Day: 8, Hour: 2, Minute: 30},
			want:  "2026-03-08 03:00 -0400",
			fold:  schedule.FoldGap,
		},
		{
			// "Ambiguous local time (01:30 on a fall-back day): take the first
			// occurrence, the pre-transition one." -0400, not -0500.
			name:  "fall back fold",
			local: schedule.LocalDateTime{Year: 2026, Month: time.November, Day: 1, Hour: 1, Minute: 30},
			want:  "2026-11-01 01:30 -0400",
			fold:  schedule.FoldAmbiguous,
		},
		{
			name:  "first instant of the fold",
			local: schedule.LocalDateTime{Year: 2026, Month: time.November, Day: 1, Hour: 1, Minute: 0},
			want:  "2026-11-01 01:00 -0400",
			fold:  schedule.FoldAmbiguous,
		},
		{
			name:  "first valid time after the gap",
			local: schedule.LocalDateTime{Year: 2026, Month: time.March, Day: 8, Hour: 3, Minute: 0},
			want:  "2026-03-08 03:00 -0400",
			fold:  schedule.FoldNone,
		},
		{
			name:  "an ordinary morning",
			local: schedule.LocalDateTime{Year: 2026, Month: time.June, Day: 1, Hour: 9, Minute: 0},
			want:  "2026-06-01 09:00 -0400",
			fold:  schedule.FoldNone,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			at, fold := schedule.Instant(c.local, ny)
			if got := at.In(ny).Format(localLayout); got != c.want {
				t.Errorf("got %s, want %s", got, c.want)
			}
			if fold != c.fold {
				t.Errorf("got fold %s, want %s", fold, c.fold)
			}
		})
	}
}

// expandLocal runs one schedule through the expansion and renders what came out
// in the item's own zone.
func expandLocal(t *testing.T, raw string, loc *time.Location, from time.Time, days int) []string {
	t.Helper()

	s, err := schedule.Parse(json.RawMessage(raw))
	if err != nil {
		t.Fatalf("parse schedule: %v", err)
	}

	rq := request{
		item: domain.Item{
			ID:     "01TESTTESTTESTTESTTESTTEST",
			Kind:   domain.KindReminder,
			TZ:     loc.String(),
			TZMode: domain.TZModeFixed,
			Active: true,
			// The RRULE anchor. Expansion is deterministic only because DTSTART
			// comes from here rather than from the clock.
			CreatedAt: from,
		},
		schedule: s,
		generate: true,
		loc:      loc,
		run: run{
			now:   from,
			to:    from.AddDate(0, 0, days),
			zones: schedule.Zones{Fallback: loc},
		},
		// Seeded, so a windowed or fuzzy case added later places the same times
		// on every run. Production seeds from rand.Uint64 instead; see
		// Materializer.newRand.
		rnd: rand.New(rand.NewPCG(1, 2)),
		log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	slots, err := rq.expand(nil)
	if err != nil {
		t.Fatalf("expand: %v", err)
	}

	out := make([]string, 0, len(slots))
	for _, s := range slots {
		out = append(out, s.at.In(loc).Format(localLayout))
	}
	return out
}

func location(t *testing.T, name string) *time.Location {
	t.Helper()

	loc, err := time.LoadLocation(name)
	if err != nil {
		t.Fatalf("load %s: %v", name, err)
	}
	return loc
}

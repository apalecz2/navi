package stats

import (
	"context"
	"fmt"
	"time"

	"github.com/aidenpaleczny/navi/internal/schedule"
	"github.com/aidenpaleczny/navi/internal/store"
)

// ItemSignals is what one item's history says, as the copywriter's context blob
// reads it (06-agent-spec.md, Context blob).
//
// It is computed here and not in the copywriter for the reason the package doc
// gives: the reminder that says "five in a row" and the stats page that says
// "5 in a row" have to be the same number, and they are because both come out of
// streaks(), Classify and medianLag over the same chains. Nothing in this file
// counts anything those functions do not.
type ItemSignals struct {
	// Resolved is the chains that reached an outcome: completed, skipped or
	// missed. It is G9's input (Q-7: "resolved occurrences per item"), and it is
	// chains rather than rows, so a snoozed-three-times reminder is one.
	Resolved int

	CurrentStreak int
	LongestStreak int

	// Last7 is the item's last seven resolved chains, newest first, as their
	// Class names. Awaiting and open chains have no outcome yet and are not in it.
	Last7 []string

	// CompletionRate30d is the trailing-30-day rate, null when nothing settled.
	CompletionRate30d *float64

	// DaysSinceLastCompletion is whole local days; null when never completed.
	DaysSinceLastCompletion *int

	MedianLagMinutes *float64

	// ConsecutiveMisses is the run of missed chains ending at the newest outcome.
	// Skipped, awaiting and open chains are neutral and are walked past, the same
	// rule streaks() applies from the other side; a completion ends the run.
	ConsecutiveMisses int

	// SnoozesToday is the snoozes spent on chains that were due today.
	SnoozesToday int

	// RecentSkipNotes are the reasons on chains skipped in the last 14 local days,
	// newest first, at most three.
	RecentSkipNotes []string
}

// skipNoteDays is how far back a skip's reason is still "recent". Two weeks is
// also the dormancy horizon, which is deliberate: a skip with a reason inside
// that window is a person with a stated reason, not a schedule gone dead.
const skipNoteDays = 14

// ItemSignals reads one item's chains up to now and summarises them.
func (s *Service) ItemSignals(ctx context.Context, itemID string) (ItemSignals, error) {
	zones, err := schedule.LoadZones(ctx, s.r, s.defaultTZ)
	if err != nil {
		return ItemSignals{}, fmt.Errorf("stats: item signals: resolve zones: %w", err)
	}
	loc := zones.Local()
	now := s.now()

	history, err := s.r.StatsChains(ctx, store.StatsFilter{ItemID: itemID, To: now.Truncate(time.Second).Add(time.Second)})
	if err != nil {
		return ItemSignals{}, err
	}

	var out ItemSignals
	t := totalsOf(history)
	out.Resolved = t.Completed + t.Skipped + t.Missed
	out.CurrentStreak, out.LongestStreak = streaks(history)
	out.MedianLagMinutes, _ = medianLag(history)

	month := resolve(RangeMonth, now, loc)
	local := now.In(loc)
	skipFrom := time.Date(local.Year(), local.Month(), local.Day()-(skipNoteDays-1), 0, 0, 0, 0, loc)
	today := dayStart(now, loc)

	var inMonth, recent []store.StatsChain
	var lastCompleted *time.Time
	for _, c := range history {
		if month.contains(c) {
			inMonth = append(inMonth, c)
		}
		if !c.ScheduledAt.Before(skipFrom) {
			recent = append(recent, c)
		}
		if !c.ScheduledAt.Before(today) {
			out.SnoozesToday += c.SnoozeCount
		}
		// The latest completion instant, not the latest completed chain: a chain
		// snoozed across midnight can complete after a later-scheduled one.
		if c.Completed && c.CompletedAt != nil && (lastCompleted == nil || c.CompletedAt.After(*lastCompleted)) {
			lastCompleted = c.CompletedAt
		}
	}
	out.CompletionRate30d = totalsOf(inMonth).rate()
	out.RecentSkipNotes = recentSkipNotes(recent, 3)
	if lastCompleted != nil {
		d := daysBetween(*lastCompleted, now, loc)
		out.DaysSinceLastCompletion = &d
	}

	// Newest first from here: the last-seven list and the miss run both read the
	// recent end of the history.
	out.Last7 = []string{}
	missRun, runOpen := 0, true
	for i := len(history) - 1; i >= 0; i-- {
		class := Classify(history[i])
		switch class {
		case ClassCompleted, ClassSkipped, ClassMissed:
			if len(out.Last7) < 7 {
				out.Last7 = append(out.Last7, string(class))
			}
		}
		if runOpen {
			switch class {
			case ClassMissed:
				missRun++
			case ClassCompleted:
				runOpen = false
			}
		}
	}
	out.ConsecutiveMisses = missRun
	return out, nil
}

// daysBetween is whole local calendar days from a to b, built from dates so a DST
// night does not make a day 23 or 25 hours long.
func daysBetween(a, b time.Time, loc *time.Location) int {
	a, b = a.In(loc), b.In(loc)
	da := time.Date(a.Year(), a.Month(), a.Day(), 0, 0, 0, 0, time.UTC)
	db := time.Date(b.Year(), b.Month(), b.Day(), 0, 0, 0, 0, time.UTC)
	d := int(db.Sub(da).Hours() / 24)
	if d < 0 {
		return 0
	}
	return d
}

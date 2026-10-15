package stats

import (
	"context"
	"time"

	"github.com/aidenpaleczny/navi/internal/store"
)

// The proactive triggers' definitions (06-agent-spec.md, Proactive behaviour).
// They live here and not in internal/proactive for the reason the package doc
// gives for every other number: the page that says "5 in a row" and the message
// that says "that's five misses" have to be counting the same chains. The loop
// that sends the message and the copywriter's tone ladder both call these, and
// neither holds a threshold of its own.
const (
	// MissRun is the trigger's "5 consecutive misses". Consecutive means what
	// ItemSignals.ConsecutiveMisses already means: missed chains since the last
	// completion, walking past skipped, awaiting and open chains, which are neutral.
	// A skip does not rescue a run and does not extend one.
	MissRun = 5

	// SnoozeRun is the trigger's "3 snoozes on one occurrence", before the item's
	// own snooze_cap lowers it. See SnoozeThreshold.
	SnoozeRun = 3

	// DormantDays is "14 days dormant". The days rule alone would call a monthly
	// reminder dormant half of every month, so Dormant carries two guards.
	DormantDays = 14

	// DormantMinMisses is the first guard: a quiet fortnight only counts when the
	// item actually came due and went unanswered at least twice in it.
	DormantMinMisses = 2

	// SnoozeLookback is how far back a snoozed chain is still worth a proposal. Past
	// two days the moment has gone, and a message about a Tuesday on a Friday reads
	// as the system keeping score.
	SnoozeLookback = 48 * time.Hour
)

// MissRunReached reports whether a consecutive-miss count is a trigger.
func MissRunReached(consecutiveMisses int) bool { return consecutiveMisses >= MissRun }

// IsDormant is the fourteen-day rule: no completion in DormantDays days, at least
// DormantMinMisses misses since, and no skip with a stated reason in that time.
//
// The third guard is the person on a stated break: a skip with a note is a reason,
// not a schedule gone dead, so it is also why recent skip notes never reach a
// message. daysSinceLastCompletion is nil when the item has never been completed,
// which is not dormancy under this rule - five misses in a row is the trigger for
// an item that never took.
//
// It does not include the five-miss run. Callers that want "dormant by either
// route" combine it with MissRunReached; the proactive loop deliberately does not,
// so that the two triggers are mutually exclusive and an item is asked once.
func IsDormant(consecutiveMisses int, daysSinceLastCompletion *int, recentSkipNotes int) bool {
	return daysSinceLastCompletion != nil && *daysSinceLastCompletion >= DormantDays &&
		consecutiveMisses >= DormantMinMisses && recentSkipNotes == 0
}

// MissRunReached and Dormant are the same two predicates over a computed signal.
func (s ItemSignals) MissRunReached() bool { return MissRunReached(s.ConsecutiveMisses) }

// Dormant is IsDormant over a computed signal.
func (s ItemSignals) Dormant() bool {
	return IsDormant(s.ConsecutiveMisses, s.DaysSinceLastCompletion, len(s.RecentSkipNotes))
}

// SnoozeThreshold is how many snoozes on one chain make the trigger for an item
// with the given snooze_cap, and zero when the item can never trigger it.
//
// The cap is enforced at request time: a snooze asked of a link already at depth
// cap resolves the chain missed (domain.CheckSnoozeCap). With the default cap of
// three, "three snoozes" is therefore the state one request short of that - the
// last moment a proposal can still save the chain from being assigned a miss - and
// not the same event as the exhaustion. With a smaller cap the chain would reach
// depth cap and then be missed without ever seeing three, so the threshold is the
// smaller of the two. A cap of zero or less never snoozes at all.
func SnoozeThreshold(snoozeCap int) int {
	if snoozeCap < 1 {
		return 0
	}
	if snoozeCap < SnoozeRun {
		return snoozeCap
	}
	return SnoozeRun
}

// SnoozedChains returns the chains rooted in the last SnoozeLookback that were
// snoozed at least once, oldest first. Whether a chain reaches its item's
// threshold is the caller's, because the threshold depends on the item; this is
// the one read of the chains view the snooze trigger makes.
func (s *Service) SnoozedChains(ctx context.Context) ([]store.StatsChain, error) {
	now := s.now()
	all, err := s.r.StatsChains(ctx, store.StatsFilter{
		From: now.Add(-SnoozeLookback),
		To:   now.Truncate(time.Second).Add(time.Second),
	})
	if err != nil {
		return nil, err
	}
	out := all[:0:0]
	for _, c := range all {
		if c.SnoozeCount >= 1 {
			out = append(out, c)
		}
	}
	return out, nil
}

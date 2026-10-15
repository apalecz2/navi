package copywriter

import "github.com/aidenpaleczny/navi/internal/stats"

// Strategy is which approach a message takes. Code chooses it from the context
// blob and the model only writes the sentence: triggers are deterministic and
// responses are generative (invariant 3). The interesting version of this feature
// is varied strategy, not varied phrasing (06-agent-spec.md, Tone ladder).
type Strategy string

const (
	StrategySnoozeChild   Strategy = "snooze_child"
	StrategyDormancy      Strategy = "dormancy"
	StrategyShrinkAsk     Strategy = "shrink_the_ask"
	StrategyProtectStreak Strategy = "protect_streak"
	StrategyLightAck      Strategy = "light_acknowledgement"
	StrategyNeutral       Strategy = "neutral"
)

// The ladder's thresholds, from the spec's table.
const (
	// StreakProtect: "current_streak >= 5: name the number, frame it as something
	// to protect".
	StreakProtect = 5

	// MissesShrink: "2 to 4 consecutive misses: shrink the ask".
	MissesShrink = 2

	// MissesDormant and DormantDays are internal/stats' numbers, not this
	// package's: the proactive loop asks the reschedule-shrink-or-drop question on
	// exactly these conditions, and two copies of a threshold are how a dormant
	// item ends up asked twice, or never.
	MissesDormant = stats.MissRun
	DormantDays   = stats.DormantDays
)

// Choose is the tone ladder. Order is precedence.
//
//  1. A snooze child is a second attempt, so it is always the light one, whatever
//     the history says. A dormancy question is asked on a first attempt; asking it
//     again on the snooze of that question would be the nag the branch exists to
//     avoid.
//  2. Dormancy: five or more consecutive misses, or fourteen days with no
//     completion. The predicates are internal/stats' (MissRunReached, IsDormant),
//     which also carry the days rule's two guards the spec's table does not state:
//     at least two misses in the run, because a monthly reminder is fourteen days
//     from its last completion for half of every month and is not dormant; and off
//     when a skip with a stated reason is recent, because that is a person away,
//     not a schedule gone dead - which is also why recent skip notes never reach a
//     message at all.
//
//     This branch no longer asks anything. The question "reschedule, shrink or
//     drop?" is the proactive loop's (internal/proactive), sent once per episode
//     with a concrete alternative and a context_ref the reply is recognised by. A
//     reminder that asked it too would put the same question to the person twice,
//     once without a record of having been asked. What is left here is the spec's
//     "stop nudging": the plainest reminder there is.
//  3. Two to four misses: shrink the ask.
//  4. A streak of five or more: name it, as something to protect. One to four:
//     acknowledge it lightly.
//  5. Anything else, including a single recent miss, is neutral. The miss is not
//     mentioned; that is in the instruction below and in the hard rules.
func Choose(b Blob) Strategy {
	switch {
	case b.IsSnoozeChild:
		return StrategySnoozeChild
	case stats.MissRunReached(b.ConsecutiveMisses),
		stats.IsDormant(b.ConsecutiveMisses, b.DaysSinceLastCompletion, len(b.RecentSkipNotes)):
		return StrategyDormancy
	case b.ConsecutiveMisses >= MissesShrink:
		return StrategyShrinkAsk
	case b.CurrentStreak >= StreakProtect:
		return StrategyProtectStreak
	case b.CurrentStreak >= 1:
		return StrategyLightAck
	}
	return StrategyNeutral
}

// Instruction is the guidance a strategy puts in the prompt, exported so
// cmd/naviseed can hold what the dormancy branch says against the proactive
// loop's ownership of its question.
func (s Strategy) Instruction() string { return s.instruction() }

// instruction is the strategy as the sentence of guidance that goes into the
// prompt. The ladder's choice is code; this is only how it is put to the model.
func (s Strategy) instruction() string {
	switch s {
	case StrategySnoozeChild:
		return "This is a second attempt: the user already snoozed this reminder once. Say so lightly, in fewer words than a first reminder would use, with no pressure at all."
	case StrategyDormancy:
		return "Do not nudge. This reminder has gone quiet, so write the plainest, shortest reminder you can: name the reminder and nothing else of weight. No encouragement, no number, no question, and no mention of the schedule or of earlier reminders - another message deals with that."
	case StrategyShrinkAsk:
		return "Shrink the ask. Offer the smallest version of this reminder - for example just five minutes of it. Do not mention that earlier ones were missed."
	case StrategyProtectStreak:
		return "The user is on a run. State the current_streak number exactly as given, as digits, and frame it as something worth keeping going."
	case StrategyLightAck:
		return "The user has done this recently. Acknowledge that in a few words and keep the message short. Do not state a number unless it is current_streak."
	}
	return "Write a plain, neutral reminder. If there has been a recent miss, do not mention it."
}

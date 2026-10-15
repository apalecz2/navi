package stats

// The empty-state thresholds: how much data a figure needs before it is shown as
// more than a count. A chart under them is replaced by a sentence that says what
// it is waiting for; the weekly digest drops its rate and its observation under
// MinSettled for the same reason. A week of history drawn as a three-point line
// reads as broken rather than sparse, and a rate over a handful of chains swings
// ten points on one answer.
//
// They were the statistics page's alone (internal/web) until the digest needed
// the same number. Two readers of one threshold is the situation this package
// exists for, so the values live here and internal/web re-exports them. They are
// presentation policy, not statistics: nothing in this package compares against
// them, and each reader compares them with a count the Service returned.
const (
	// MinSettled is how many settled chains (completed or missed) a window needs
	// before a completion-rate line is drawn, or a rate is put in a digest. Below
	// it one chain moves the rate by ten points or more.
	MinSettled = 10

	// MinRateBuckets is how many buckets must have a rate (null buckets are gaps)
	// for the line to be a line.
	MinRateBuckets = 3

	// MinCompletions is how many completions the time-of-day grid needs.
	MinCompletions = 10

	// MinGoalPoints is how many points a goal's trail needs.
	MinGoalPoints = 2
)

package digest

import (
	"fmt"
	"strings"

	"github.com/aidenpaleczny/navi/internal/stats"
)

// Form is how much a week earns.
type Form string

const (
	// FormFull is a week with enough settled chains to read: the figures, the
	// rate, and one observation from the model when it produces a usable one.
	FormFull Form = "full"

	// FormShort is a week with something in it but too little to read a pattern
	// from: the counts and a sentence saying so, and no model call. Honest about
	// being thin rather than dressed up with a percentage.
	FormShort Form = "short"

	// FormNone is a week in which nothing was scheduled at all - a new install,
	// or everything paused. There is nothing to say and nothing is sent.
	FormNone Form = "none"
)

// maxItems bounds the per-item lines. A digest of thirty reminders is a wall of
// text nobody reads; the rest are acknowledged in one line with no number in it.
const maxItems = 10

// closing is the last line of every digest. A digest is a message the person may
// answer ("drop the walk, then"), and the line says so without asking anything
// specific. It contains no number.
const closing = "Anything you want to change for next week?"

// shortNote is the sentence a thin week carries instead of a percentage. No
// number in it, so it cannot disagree with the figures it follows.
const shortNote = "That is too little to call a pattern, so there is no observation this week."

// Figures is the week's numbers, worded. Facts is the one text both halves of the
// digest are made from: it is what the template sends, what the model is shown,
// and what the number check compares the model's reply against. There is no other
// source of a figure in this package.
type Figures struct {
	Form  Form
	Facts string
}

// Settle chooses a form from the week's Summary and words it.
//
// The thin-week rule is stats.MinSettled, the threshold the statistics page uses
// to decide whether a completion-rate line has enough behind it: fewer settled
// chains than that and the page says "not enough data yet" instead of drawing
// one, and the digest does the same instead of printing a percentage. No chains
// at all in the window is the other edge, and it sends nothing.
func Settle(sum stats.Summary) Figures {
	switch {
	case sum.Totals.Chains == 0:
		return Figures{Form: FormNone}
	case sum.Totals.Settled < stats.MinSettled:
		return Figures{Form: FormShort, Facts: stats.RangePhrase(sum.Range) + ": " + sum.RateDetail() + "."}
	}
	return Figures{Form: FormFull, Facts: fullFacts(sum)}
}

// fullFacts is every figure of a full digest, in the phrases the statistics page
// and get_stats use. Nothing here formats a number itself except through them.
func fullFacts(sum stats.Summary) string {
	var b strings.Builder

	headline := stats.RangePhrase(sum.Range) + ": "
	if sum.CompletionRate != nil {
		headline += stats.Percent(*sum.CompletionRate) + " done"
	}
	fmt.Fprintf(&b, "%s (%s).\n", headline, sum.RateDetail())
	if sum.MedianLagMinutes != nil {
		fmt.Fprintf(&b, "Median time from reminder to done: %s.\n", stats.MinutesPhrase(*sum.MedianLagMinutes))
	}

	listed, more := 0, 0
	for _, it := range sum.Items {
		if it.Totals.Chains == 0 {
			continue // listed on the page for its lifetime streak; not part of this week
		}
		if listed == maxItems {
			more++
			continue
		}
		listed++
		fmt.Fprintf(&b, "%s: %s · %s\n", it.Title, it.Meta(), stats.StreakPhrase(it.CurrentStreak, it.LongestStreak))
	}
	if more > 0 {
		// No count: "and N more" would be a figure get_stats never says, and the
		// digest says no figure the statistics do not.
		b.WriteString("More reminders than fit here are not listed.\n")
	}

	for i, g := range sum.Goals {
		if i == 0 {
			b.WriteString("Goals:\n")
		}
		fmt.Fprintf(&b, "%s: %s (%s)\n", g.Title, stats.GoalPhrase(g), g.Status)
	}
	return strings.TrimRight(b.String(), "\n")
}

// Message is the text that is sent: the figures, then the observation if there is
// one, then the closing line. With observation empty it is the template.
func (f Figures) Message(observation string) string {
	var b strings.Builder
	b.WriteString(f.Facts)
	b.WriteString("\n\n")
	switch {
	case f.Form == FormShort:
		b.WriteString(shortNote)
		b.WriteString("\n\n")
	case observation != "":
		b.WriteString(observation)
		b.WriteString("\n\n")
	}
	b.WriteString(closing)
	return b.String()
}

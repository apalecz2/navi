package schedule

import (
	"fmt"
	"strings"

	"github.com/aidenpaleczny/navi/internal/defaults"
)

// Inference is one field the caller did not supply and the table did.
//
// It is a return value rather than a log line because A5 needs it: the agent
// confirms every write in plain language and states which parameters it
// guessed, and D-015 makes that statement the whole reason the system is
// allowed to never ask a clarifying question. A default applied silently is a
// wrong schedule the user has no reason to look at.
type Inference struct {
	// Field is the dotted path, matching the field a ValidationError would
	// name.
	Field string

	// Value is the filled value, rendered the way a person reads it.
	Value string

	// Source is where it came from, as a path into defaults.yaml. It is what
	// makes "why is my window 9 to 9" answerable without reading the code.
	Source string
}

// String renders an inference for a confirmation or a log line.
func (i Inference) String() string {
	return fmt.Sprintf("%s = %s (from %s)", i.Field, i.Value, i.Source)
}

// Describe renders a list of inferences as one clause the agent can put in a
// sentence. Empty when nothing was inferred, so the caller can skip the clause
// entirely rather than saying "I assumed nothing".
func Describe(inferences []Inference) string {
	if len(inferences) == 0 {
		return ""
	}
	parts := make([]string, len(inferences))
	for i, inf := range inferences {
		parts[i] = strings.TrimPrefix(inf.Field, "schedule.") + " " + inf.Value
	}
	return strings.Join(parts, ", ")
}

// Resolve fills an under-specified schedule from the vocabulary table and
// reports what it filled.
//
// It is a separate pass from Parse, deliberately. Reading a stored schedule
// must return the column and not a version of it improved on the way past, or
// the materializer and the calendar disagree with what a person edited. And the
// inference list only means anything on the write path, which is the only path
// that calls this.
//
// It fills structure, not intent. count, period, rrule and at are never
// invented: the frequency block of the table maps a phrase to those and the
// model supplies them, so an absent one is a rejection naming the field rather
// than a guess. What is filled is the placement detail a person does not say
// out loud — the window, the minimum gap, the days that are allowed.
func Resolve(s Schedule, table *defaults.Table) (Schedule, []Inference, error) {
	if table == nil {
		return s, nil, fmt.Errorf("schedule: resolve: no defaults table")
	}

	var inferences []Inference

	if s.Kind.UsesWindow() && len(s.Window) == 0 {
		w := table.DefaultWindow()
		s.Window = w.Strings()
		inferences = append(inferences, Inference{
			Field:  "schedule.window",
			Value:  w.String(),
			Source: "windows.default",
		})
	}

	if s.Kind == KindFuzzy {
		if len(s.DaysAllowed) == 0 {
			days := table.DaysAllowed()
			s.DaysAllowed = days
			inferences = append(inferences, Inference{
				Field:  "schedule.days_allowed",
				Value:  strings.Join(days, ","),
				Source: "defaults.days_allowed",
			})
		}

		// The gap is keyed by period, so a schedule that never named its period
		// gets nothing here and fails the required-field check instead. That is
		// the right order: "period is required" is a better message than a gap
		// error about a period the caller never gave.
		if s.MinGapHours == nil && s.Period != nil {
			if gap, ok := table.MinGap(string(*s.Period)); ok {
				g := gap
				s.MinGapHours = &g
				inferences = append(inferences, Inference{
					Field:  "schedule.min_gap_hours",
					Value:  fmt.Sprintf("%dh", gap),
					Source: "min_gap_hours." + string(*s.Period),
				})
			}
		}
	}

	return s, inferences, nil
}

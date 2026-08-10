package conversation

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/aidenpaleczny/navi/internal/defaults"
	"github.com/aidenpaleczny/navi/internal/schedule"
)

// roleAndScope is system-prompt part 1 (docs/06-agent-spec.md#system-prompt-
// structure): what the system is, what it manages, what it does not do.
const roleAndScope = `You are the conversational agent for Navi, a single-user reminder and event system. You manage recurring reminders (items) and their scheduled instances (occurrences) through tool calls only - you never write to the database except by calling one of the tools below. You do not manage anyone else's reminders and there is no user to switch between.`

// behaviouralRules is system-prompt part 4. It names only the tools this
// session's catalog actually has - list_items, create_item, update_item,
// delete_item, request_escalation - and deliberately omits the doc's rules
// about bulk_resolve, pause, and propose_change, none of which exist yet:
// an instruction naming a tool that is not offered would mislead the model
// rather than help it.
const behaviouralRules = `Rules:
- Never ask a clarifying question about an under-specified schedule. Apply the vocabulary defaults below, state your interpretation in the confirmation, and invite correction. Only ask when a reference is ambiguous, such as two items that could both be "the gym one".
- Always confirm a write in plain language, naming the next concrete occurrence times.
- When a delete is clearly requested, call delete_item directly with confirmed=true rather than asking first - the tool itself rejects an unconfirmed delete, and that rejection is your cue to retry with confirmed=true, not a reason to reply in prose instead.
- Call request_escalation when the request is ambiguous, spans multiple items in a way that is hard to disentangle, or references something unresolvable.
- Always respond by calling exactly one tool. A plain-text reply with no tool call is treated as a failure, not an answer.`

// buildSystemPrompt assembles the five-part system prompt
// (docs/06-agent-spec.md#system-prompt-structure). Part 5, injected context,
// is thin this session - only current time and the active timezone
// ("without those no schedule can be validated as future, so they are not
// deferrable") - active items, today's occurrences, last-touched, and
// context_ref are next session.
func (l *Ladder) buildSystemPrompt(ctx context.Context) string {
	var b strings.Builder

	b.WriteString(roleAndScope)
	b.WriteString("\n\n")

	persona, err := defaults.GetPersona(l.personaPath)
	if err == nil && persona != "" {
		b.WriteString(persona)
		b.WriteString("\n\n")
	}

	b.WriteString(renderDefaults(l.defaultsTbl))
	b.WriteString("\n\n")

	b.WriteString(behaviouralRules)
	b.WriteString("\n\n")

	loc, tzName := l.deviceZone(ctx)
	now := time.Now().In(loc)
	fmt.Fprintf(&b, "Current time:      %s (%s)\nDevice timezone:   %s\n",
		now.Format("2006-01-02T15:04:05-07:00"), now.Weekday(), tzName)

	return b.String()
}

// deviceZone reads store.CurrentTZ, falling back to the deployment default
// when nothing has been set - nothing sets it until P2's set_timezone tool
// exists, so this always falls through today. Inlined rather than exported
// from internal/agent: it is a three-line live lookup with nothing to cache,
// so there is nothing for two copies to drift on.
func (l *Ladder) deviceZone(ctx context.Context) (*time.Location, string) {
	if name, ok, err := l.store.CurrentTZ(ctx); err == nil && ok {
		if loc, err := schedule.LoadLocation(name); err == nil {
			return loc, name
		}
	}
	return l.defaultTZ, l.defaultTZ.String()
}

// renderDefaults renders get_defaults() into prose so the model resolves
// vocabulary ("periodically", "in the morning") the same way
// schedule.Resolve will (D-016) - it reads the same loaded *defaults.Table
// the validator does, never a second copy. Map keys are sorted for a
// stable, prefix-cache-friendly prompt: Go map iteration order is random,
// and an OpenRouter-style provider may prefix-cache the system prompt
// across calls.
func renderDefaults(t *defaults.Table) string {
	var b strings.Builder
	b.WriteString("Vocabulary defaults (resolve under-specified phrasing against this table, exactly as the validator will):\n")

	b.WriteString("Frequency phrases:\n")
	phrases := make([]string, 0, len(t.Frequency))
	for phrase := range t.Frequency {
		phrases = append(phrases, phrase)
	}
	sort.Strings(phrases)
	for _, phrase := range phrases {
		f := t.Frequency[phrase]
		var parts []string
		if f.Kind != "" {
			parts = append(parts, "kind="+f.Kind)
		}
		if f.Period != "" {
			parts = append(parts, "period="+f.Period)
		}
		if f.Count != 0 {
			parts = append(parts, fmt.Sprintf("count=%d", f.Count))
		}
		if f.RRule != "" {
			parts = append(parts, "rrule="+f.RRule)
		}
		fmt.Fprintf(&b, "  %q -> %s\n", phrase, strings.Join(parts, " "))
	}

	b.WriteString("Named windows (local HH:MM-HH:MM):\n")
	windows := make([]string, 0, len(t.Windows))
	for name := range t.Windows {
		windows = append(windows, name)
	}
	sort.Strings(windows)
	for _, name := range windows {
		fmt.Fprintf(&b, "  %s: %s\n", name, t.Windows[name].String())
	}

	b.WriteString("Minimum gap between occurrences of one fuzzy item:\n")
	for _, period := range []string{"day", "week", "month"} {
		if gap, ok := t.MinGapHours[period]; ok {
			fmt.Fprintf(&b, "  %s: %dh\n", period, gap)
		}
	}

	fmt.Fprintf(&b, "Item defaults: days_allowed=%s priority=%d notify_policy=%s tz_mode=%s snooze_cap=%d\n",
		strings.Join(t.Defaults.DaysAllowed, ","), t.Defaults.Priority, t.Defaults.NotifyPolicy,
		t.Defaults.TZMode, t.Defaults.SnoozeCap)

	return b.String()
}

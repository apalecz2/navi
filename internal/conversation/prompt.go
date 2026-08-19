package conversation

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/aidenpaleczny/navi/internal/defaults"
	"github.com/aidenpaleczny/navi/internal/domain"
	"github.com/aidenpaleczny/navi/internal/schedule"
)

// roleAndScope is system-prompt part 1 (docs/06-agent-spec.md#system-prompt-
// structure): what the system is, what it manages, what it does not do.
const roleAndScope = `You are the conversational agent for Navi, a single-user reminder and event system. You manage recurring reminders (items) and their scheduled instances (occurrences) through tool calls only - you never write to the database except by calling one of the tools below. You do not manage anyone else's reminders and there is no user to switch between.`

// behaviouralRules is system-prompt part 4. It names only the tools the
// catalog actually has - list_items, create_item, update_item, delete_item,
// bulk_resolve, request_escalation - and deliberately omits the doc's rules
// about pause and propose_change, neither of which exists yet: an instruction
// naming a tool that is not offered would mislead the model rather than help
// it.
//
// bulk_resolve's rule is 06-agent-spec's own, verbatim in intent: prefer it for
// any message containing a completion, including exactly one. The tool is
// atomic over a list, so there is never a reason to reach for something else,
// and the ids it needs are already in the Today's occurrences block below.
const behaviouralRules = `Rules:
- Never ask a clarifying question about an under-specified schedule. Apply the vocabulary defaults below, state your interpretation in the confirmation, and invite correction. Only ask when a reference is ambiguous, such as two items that could both be "the gym one".
- Use the Active items, Today's occurrences, and Last touched blocks below to resolve references ("it", "the gym one", "make it more like five times") before considering that a reference is ambiguous. Only escalate when more than one item still fits after checking them.
- Always confirm a write in plain language, naming the next concrete occurrence times.
- When a delete is clearly requested, call delete_item directly with confirmed=true rather than asking first - the tool itself rejects an unconfirmed delete, and that rejection is your cue to retry with confirmed=true, not a reason to reply in prose instead.
- Prefer bulk_resolve for any message reporting that something was done, skipped, or missed - including a single one, and including something already done earlier in the day. It takes a list and writes all of it in one transaction, so one call covers "did everything except the walk". Take the occurrence ids from the Today's occurrences block; never guess one.
- Call request_escalation when the request is ambiguous, spans multiple items in a way that is hard to disentangle, or references something unresolvable.
- Always respond by calling exactly one tool. A plain-text reply with no tool call is treated as a failure, not an answer.`

// buildSystemPrompt assembles the five-part system prompt
// (docs/06-agent-spec.md#system-prompt-structure). Part 5, injected context,
// is the full block this session: current time, device timezone, global
// pause, active items, today's occurrences, and last touched. context_ref
// stays absent - nothing writes it before P3's reconciliation check-ins
// exist.
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
	fmt.Fprintf(&b, "Current time:      %s (%s)\nDevice timezone:   %s\nGlobal pause:      %s\n\n",
		now.Format("2006-01-02T15:04:05-07:00"), now.Weekday(), tzName, l.renderPause(ctx, loc))

	b.WriteString(l.renderActiveItems(ctx))
	b.WriteString("\n")
	b.WriteString(l.renderTodaysOccurrences(ctx, loc))
	b.WriteString("\n")
	b.WriteString(l.renderLastTouched(ctx))

	return b.String()
}

// renderPause is the context block's "Global pause" line - "none", or the
// local date vacation mode (I6) runs until. A store error degrades to "none"
// rather than failing the whole turn over a line the model can act
// correctly without.
func (l *Ladder) renderPause(ctx context.Context, loc *time.Location) string {
	until, paused, err := l.store.GlobalPauseUntil(ctx)
	if err != nil || !paused || !until.After(time.Now()) {
		return "none"
	}
	return "until " + until.In(loc).Format(domain.DateLayout)
}

// renderActiveItems is the context block's "Active items" list: id, quoted
// title, notify_policy, and the schedule's compact summary
// (schedule.Schedule.String(), already built to be exactly this string) - "so
// the model reasons over enough to disambiguate 'the gym one' without it
// becoming a paragraph" (the session brief's own words for this block).
func (l *Ladder) renderActiveItems(ctx context.Context) string {
	items, err := l.store.ListActiveItems(ctx)
	if err != nil || len(items) == 0 {
		return "Active items: none\n"
	}
	var b strings.Builder
	b.WriteString("Active items:\n")
	tw := tabwriter.NewWriter(&b, 0, 2, 2, ' ', 0)
	for _, it := range items {
		sched, _ := schedule.Parse(it.Schedule)
		fmt.Fprintf(tw, "  %s\t%q\t%s\t%s\n", it.ID, it.Title, it.NotifyPolicy, sched.String())
	}
	tw.Flush()
	return b.String()
}

// renderTodaysOccurrences is the context block's "Today's occurrences" list -
// what makes "everything except the walk" resolvable, since active item
// definitions alone don't say what's outstanding today. loc is the device
// timezone already resolved for the rest of the prompt, so "today" here and
// "Current time" above it never disagree.
func (l *Ladder) renderTodaysOccurrences(ctx context.Context, loc *time.Location) string {
	occs, err := l.store.TodaysOccurrences(ctx, loc)
	if err != nil || len(occs) == 0 {
		return "Today's occurrences: none\n"
	}
	var b strings.Builder
	b.WriteString("Today's occurrences:\n")
	tw := tabwriter.NewWriter(&b, 0, 2, 2, ' ', 0)
	for _, o := range occs {
		status := string(o.Status)
		if o.ResolvedAt != nil {
			source := "?"
			if o.ResolutionSource != nil {
				source = string(*o.ResolutionSource)
			}
			status = fmt.Sprintf("%s  (%s, %s)", status, source, o.ResolvedAt.In(loc).Format("15:04"))
		}
		fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\n", o.ID, o.StartsAt.In(loc).Format("15:04"), o.ItemTitle, status)
	}
	tw.Flush()
	return b.String()
}

// renderLastTouched is the context block's "Last touched" line (A9) - the
// referent for "make it more like five times" without re-naming the item.
// Omitted entirely when nothing has been touched yet or the item no longer
// resolves, the same "absent rather than a plausible placeholder" convention
// Context ref follows.
func (l *Ladder) renderLastTouched(ctx context.Context) string {
	id, ok, err := l.store.LastTouchedItemID(ctx)
	if err != nil || !ok {
		return ""
	}
	item, err := l.store.GetItem(ctx, id)
	if err != nil {
		return ""
	}
	return fmt.Sprintf("Last touched: %s (%q)\n", id, item.Title)
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

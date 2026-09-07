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
	"github.com/aidenpaleczny/navi/internal/store"
)

// roleAndScope is system-prompt part 1 (docs/06-agent-spec.md#system-prompt-
// structure): what the system is, what it manages, what it does not do.
const roleAndScope = `You are the conversational agent for Navi, a single-user reminder and event system. You manage recurring reminders (items) and their scheduled instances (occurrences) through tool calls only - you never write to the database except by calling one of the tools below. You do not manage anyone else's reminders and there is no user to switch between.`

// behaviouralRules is system-prompt part 4. It names only the tools the
// catalog actually has - list_items, create_item, update_item, delete_item,
// bulk_resolve, pause, request_escalation - and deliberately omits the doc's
// rule about propose_change, which does not exist yet: an instruction naming a
// tool that is not offered would mislead the model rather than help it. pause
// joined the catalog and this list in the same commit, which is the discipline
// that keeps the two honest.
//
// pause's rule is 06-agent-spec's "prefer pause over multiple skips when the
// user indicates absence". It is worth stating rather than leaving to the tool
// description because the wrong behaviour is plausible: "I'm away until Monday"
// reads as a report about eighteen occurrences, and resolving it that way
// produces eighteen skips in the completion record where the truth is one
// absence.
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
- Record a skip, not a miss, whenever the user says why something did not happen, and put their own words in the note: "did everything except the walk, I was away" is a completed set plus one skipped walk with note "I was away". Never choose missed. A miss is not something a person reports - it is what the system concludes when nobody said anything at all.
- A "Context ref" block below means the last thing you sent was an end-of-day check-in and those occurrences are still waiting on an answer. The user's message may be that answer. If it reports what was or was not done, call bulk_resolve for exactly the occurrences it names and say nothing about the ones it left out - an item the user did not mention is not a skip, and it is not your job to ask again. If the message is a new request instead, treat it as one: a check-in does not oblige the user to answer it before saying anything else. Ids for these may come from the Context ref block or from Today's occurrences; after midnight only the Context ref block still lists them.
- Prefer pause over a run of skips when the user says they are away or unavailable for a stretch of time. "I'm away until Monday" is one pause with scope=global, not a skip for every occurrence in between. Use scope=item when only one reminder is affected. Pausing with no until resumes normal operation.
- Call request_escalation when the request is ambiguous, spans multiple items in a way that is hard to disentangle, or references something unresolvable.
- Always respond by calling exactly one tool. A plain-text reply with no tool call is treated as a failure, not an answer.`

// buildSystemPrompt assembles the five-part system prompt
// (docs/06-agent-spec.md#system-prompt-structure). Part 5, injected context, is
// the full block: current time, device timezone, global pause, active items,
// today's occurrences, last touched, and context ref.
//
// Context ref is what makes a reply to the check-in resolvable, and it is
// deliberately the whole of the mechanism. There is no classifier deciding
// whether a message is an answer, and no separate route for one: every inbound
// turn runs through this same prompt and this same catalog. Two things do the
// work. The check-in is already in cross-turn history - the reconciler writes
// it in persistAssistantProse's shape - so the model sees its own question
// immediately above the user's reply. And this block names which occurrences
// are still waiting, so "everything except the walk" has a set to resolve
// against.
//
// Recognition therefore costs nothing when the message is not a reply. A new
// request arriving after a check-in reaches create_item exactly as it would
// have, because nothing about the routing changed and behaviouralRules says in
// so many words that a check-in does not bind what the user says next.
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
	b.WriteString(l.renderActiveGoals(ctx, loc))
	b.WriteString("\n")
	b.WriteString(l.renderLastTouched(ctx))
	b.WriteString(l.renderContextRef(ctx, loc))

	return b.String()
}

// renderContextRef is the context block's "Context ref" section: the check-in
// currently awaiting an answer, and the occurrences it is still waiting on.
//
// Omitted entirely when nothing is awaiting, which is the same "absent rather
// than a plausible placeholder" convention renderLastTouched follows - and the
// convention this block is named in. An empty Context ref line would tell the
// model it is answering a question nobody asked, which is the one failure the
// whole design is arranged to avoid.
//
// Titles are listed beside the ids and are not decoration. A 21:00 check-in
// answered at 00:15 is waiting on rows from yesterday, which Today's
// occurrences no longer contains, and behaviouralRules forbids guessing an id -
// so past midnight this block is the only place the title-to-id mapping exists.
//
// "Still awaiting" is Deadline in the future: the same rows the grace pass will
// mark missed once it is not, read from the same store method. That is what
// makes "the agent can still resolve it" and "the reconciler has not concluded
// yet" one fact rather than two clocks.
func (l *Ladder) renderContextRef(ctx context.Context, loc *time.Location) string {
	awaiting, err := l.store.ListAwaitingReconciliation(ctx, loc)
	if err != nil {
		return "" // degrade to no block, matching renderPause
	}

	now := time.Now()
	live := make([]store.Awaiting, 0, len(awaiting))
	for _, a := range awaiting {
		if a.Deadline.After(now) {
			live = append(live, a)
		}
	}
	if len(live) == 0 {
		return ""
	}

	ref, ok, err := l.store.LatestContextRef(ctx, store.ContextRefReconcile)
	if err != nil || !ok {
		// Rows are awaiting an answer but no check-in row names them, which
		// means a conversations write was lost after RecordCheckIn marked them.
		// Naming the date these rows were actually asked on is better than
		// dropping the block: the ids are what the model needs, and the ref is
		// how it knows they belong together.
		ref = "reconcile:" + live[0].ReconciledAt.In(loc).Format(domain.DateLayout)
	}

	var b strings.Builder
	fmt.Fprintf(&b, "Context ref:  %s\n", ref)
	tw := tabwriter.NewWriter(&b, 0, 2, 2, ' ', 0)
	for i, a := range live {
		label := "  awaiting:"
		if i > 0 {
			label = "  "
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\n", label, a.ID, a.ItemTitle)
	}
	tw.Flush()
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

// renderActiveGoals is the context block's "Active goals" list (P3.5,
// docs/06-agent-spec.md#context-injection): id, quoted title, shape, and the
// current progress. It reads the widened ListGoalProgress rather than a second
// "what's active" query, and the progress it prints is computed fresh in that
// call - a count over chains for an item-linked goal, the newest goal_updates
// row for a freestanding one. The briefing composer reads the same block next
// session, so a goal named in the morning message and one the agent discusses
// mid-conversation show identical numbers.
func (l *Ladder) renderActiveGoals(ctx context.Context, loc *time.Location) string {
	goals, err := l.store.ListGoalProgress(ctx, store.GoalFilterActive, loc)
	if err != nil || len(goals) == 0 {
		return "Active goals: none\n"
	}
	var b strings.Builder
	b.WriteString("Active goals:\n")
	tw := tabwriter.NewWriter(&b, 0, 2, 2, ' ', 0)
	for _, p := range goals {
		kind := "freestanding"
		progress := "no updates yet"
		switch {
		case p.ItemLinked:
			kind = "item-linked"
			progress = fmt.Sprintf("%d/%d this period", p.Completed, p.Target)
		case p.HasUpdate:
			updated := "updated " + p.UpdatedAt.In(loc).Format(domain.DateLayout)
			switch {
			case p.LatestPct != nil && p.LatestNote != nil && *p.LatestNote != "":
				progress = fmt.Sprintf("%d%%, %q, %s", *p.LatestPct, *p.LatestNote, updated)
			case p.LatestPct != nil:
				progress = fmt.Sprintf("%d%%, %s", *p.LatestPct, updated)
			case p.LatestNote != nil:
				progress = fmt.Sprintf("%q, %s", *p.LatestNote, updated)
			}
		}
		fmt.Fprintf(tw, "  %s\t%q\t%s\t%s\n", p.Goal.ID, p.Goal.Title, kind, progress)
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

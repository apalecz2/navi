package briefing

import (
	"context"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/aidenpaleczny/navi/internal/defaults"
	"github.com/aidenpaleczny/navi/internal/domain"
	"github.com/aidenpaleczny/navi/internal/model"
	"github.com/aidenpaleczny/navi/internal/store"
)

// composeBudget bounds the whole tier walk, far under the tiers' own configured
// timeouts, for the reason reconciler.composeBudget spells out: supervisor
// ticks are serial, so an unbudgeted walk would hold this loop for minutes and
// stale /healthz for a loop that is working fine. The composer is the component
// with the good failure mode, so it is the one that gives up first.
const composeBudget = 20 * time.Second

// maxComposedRunes is the ceiling on a composed briefing. A model that returns
// an essay has misunderstood the task badly enough that the template is better
// prose, so this is a fallback trigger, not a truncation point.
const maxComposedRunes = 600

// Context is what both the template and the model composer are handed: the same
// picture the agent's system prompt assembles, gathered once so a fallback
// mid-window names the same things in the same order.
type Context struct {
	// Local is now in the person's zone — the date and weekday the briefing
	// opens with.
	Local time.Time

	Items []itemView
	Occs  []occView
	Goals []store.GoalProgress
}

type itemView struct {
	Title  string
	Silent bool
}

type occView struct {
	Time   string // local HH:MM
	Title  string
	Silent bool
	Done   bool
}

// buildContext gathers the briefing's view of the day. It is deliberately the
// same three reads renderActiveItems / renderTodaysOccurrences / renderActiveGoals
// make in internal/conversation, not a second path.
func (b *Briefing) buildContext(ctx context.Context, local time.Time) (Context, error) {
	items, err := b.store.ListActiveItems(ctx)
	if err != nil {
		return Context{}, fmt.Errorf("briefing: list active items: %w", err)
	}
	occs, err := b.store.TodaysOccurrences(ctx, local.Location())
	if err != nil {
		return Context{}, fmt.Errorf("briefing: today's occurrences: %w", err)
	}
	goals, err := b.store.ListGoalProgress(ctx, store.GoalFilterActive, local.Location())
	if err != nil {
		return Context{}, fmt.Errorf("briefing: list goal progress: %w", err)
	}

	silentItem := make(map[string]bool, len(items))
	blob := Context{Local: local, Goals: goals}
	for _, it := range items {
		silent := it.NotifyPolicy == domain.NotifySilent
		silentItem[it.ID] = silent
		blob.Items = append(blob.Items, itemView{Title: it.Title, Silent: silent})
	}
	for _, o := range occs {
		blob.Occs = append(blob.Occs, occView{
			Time:   o.StartsAt.In(local.Location()).Format("15:04"),
			Title:  o.ItemTitle,
			Silent: silentItem[o.ItemID],
			Done:   o.ResolvedAt != nil,
		})
	}
	return blob, nil
}

// Composer turns the day's context into the briefing's prose.
//
// Nil is an ordinary configuration, not a degraded one: the model stack exists
// only when a chat transport does, and a deployment that cannot receive a reply
// has no use for a prettier briefing. composeBriefing templates in that case
// without treating it as a failure.
type Composer interface {
	Compose(ctx context.Context, blob Context) (string, error)
}

// ModelComposer is the model half of the briefing.
type ModelComposer struct {
	client      *model.Client
	routing     *model.Routing
	personaPath string
}

// NewModelComposer returns a composer for task briefing. routing is read for
// its tier count only; the client owns everything else about the call.
func NewModelComposer(client *model.Client, routing *model.Routing, personaPath string) *ModelComposer {
	return &ModelComposer{client: client, routing: routing, personaPath: personaPath}
}

// composeSystemPrompt is 06-agent-spec's Briefing composer section as
// instructions. The persona goes in front of it (systemPrompt), so a briefing
// sounds like the same app the conversation does.
const composeSystemPrompt = `You are writing one short morning briefing for a single-user reminder app. You are telling the user what their day looks like and asking what else it should include.

Rules:
- State what today looks like: the reminders due today, which ones are silent so they will not ping, and where each active goal stands for its period. Use the titles and numbers given, in the order given. Do not invent anything and do not drop anything.
- Call out explicitly anything that is not the normal routine - a goal that is behind pace for its period is the main one you can see from the data given. Do not manufacture novelty that is not there.
- End with a real question about what else today should include. It expects an answer; it is not rhetorical.
- Two to four sentences. Never guilt, never scold, never comment on things the user did not do. This is information and an invitation, not an assessment.
- Reply with the message text only. No greeting line, no sign-off, no quotation marks, no formatting or bullet points.`

// Compose asks the model for the briefing text, walking tiers until one answers
// usably. It returns an error rather than a fallback string — the fallback is
// composeBriefing's to apply, so "the model failed" and "there is no model"
// take one path.
func (c *ModelComposer) Compose(ctx context.Context, blob Context) (string, error) {
	messages := []model.Message{
		{Role: model.RoleSystem, Content: c.systemPrompt()},
		{Role: model.RoleUser, Content: renderContextForModel(blob)},
	}

	ctx, cancel := context.WithTimeout(ctx, composeBudget)
	defer cancel()

	tiers := c.routing.TierCount(model.TaskBriefing)
	var lastErr error
	for tier := 1; tier <= tiers; tier++ {
		var esc *model.Escalation
		if lastErr != nil {
			esc = &model.Escalation{Reason: lastErr.Error()}
		}

		res, err := c.client.Complete(ctx, model.Request{
			Task:       model.TaskBriefing,
			Tier:       tier,
			Messages:   messages,
			Escalation: esc,
		})
		if err != nil {
			lastErr = err
			continue
		}
		text, err := acceptComposed(res.Message.Content)
		if err != nil {
			lastErr = err
			continue
		}
		return text, nil
	}

	if lastErr == nil {
		// TierCount returned zero: task briefing is missing from
		// config/model.yaml. Unreachable when the file ships with the block,
		// but it keeps the loop from falling out with ("", nil).
		return "", fmt.Errorf("briefing: compose: no tiers configured for task %s", model.TaskBriefing)
	}
	return "", fmt.Errorf("briefing: compose: %w", lastErr)
}

// composeBriefing is the one place that decides what a pass stages or sends.
//
// The template is computed first and unconditionally, so it is the value the
// function starts from rather than an error path it falls into — D-009's
// degrade-to-boring rule as control flow. allowModel is false at (or past) the
// send, where invariant 1 forbids a model call.
func (b *Briefing) composeBriefing(ctx context.Context, blob Context, allowModel bool) (text string, fellBack bool) {
	template := composeTemplate(blob)
	if !allowModel || b.composer == nil {
		return template, true
	}

	composed, err := b.composer.Compose(ctx, blob)
	if err != nil {
		b.log.Warn("briefing composition fell back to the template", "err", err)
		return template, true
	}
	return composed, false
}

// systemPrompt is the composer's instructions with the persona in front. A
// persona that cannot be read is skipped rather than fatal, matching
// reconciler.ModelComposer.systemPrompt.
func (c *ModelComposer) systemPrompt() string {
	persona, err := defaults.GetPersona(c.personaPath)
	if err != nil || persona == "" {
		return composeSystemPrompt
	}
	return persona + "\n\n" + composeSystemPrompt
}

// renderContextForModel is the day's context as the plain text the model reads.
// It reuses composeTemplate's own rendering of the lists so the model and the
// fallback are describing exactly the same facts.
func renderContextForModel(blob Context) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Today: %s\n\n", blob.Local.Format("Monday, 2 January 2006"))
	b.WriteString(renderOccLines(blob.Occs))
	b.WriteString("\n")
	b.WriteString(renderGoalLines(blob.Goals))
	return b.String()
}

// acceptComposed is the guard between a completion and a staged message. Both
// failures return an error so the caller falls back rather than repairs.
func acceptComposed(content string) (string, error) {
	text := strings.TrimSpace(content)
	if text == "" {
		return "", fmt.Errorf("composed briefing was empty")
	}
	if n := utf8.RuneCountInString(text); n > maxComposedRunes {
		return "", fmt.Errorf("composed briefing was %d runes, over the %d limit", n, maxComposedRunes)
	}
	return text, nil
}

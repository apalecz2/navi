package proactive

import (
	"context"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/aidenpaleczny/navi/internal/defaults"
	"github.com/aidenpaleczny/navi/internal/domain"
	"github.com/aidenpaleczny/navi/internal/model"
)

// composeBudget bounds the whole tier walk, far under the tiers' own configured
// timeouts, for the reason reconciler.composeBudget spells out: ticks are serial,
// so an unbudgeted walk holds the loop. The composer is the component with the
// good failure mode, so it is the one that gives up first.
const composeBudget = 20 * time.Second

// maxComposedRunes is the ceiling on a composed proposal. One reminder, one
// offer: anything longer has misunderstood the task and the template is better
// prose, so this is a fallback trigger and not a truncation point.
const maxComposedRunes = 400

// Candidate is what a message is written from: which trigger fired, for which
// item, and the alternative code has already chosen. Nothing in it is something
// the model decides.
type Candidate struct {
	Key     string
	Trigger string
	Item    domain.Item
	Alt     Alternative
}

// reason is the trigger as a plain clause for the model. There are no numbers in
// it on purpose: a count of misses is an invitation to mention it, and G8 says an
// unprompted message never guilts.
func (c Candidate) reason() string {
	switch c.Trigger {
	case domain.TriggerSnoozes:
		return "it was snoozed several times in a row"
	case domain.TriggerDormant:
		return "it has been quiet for about two weeks"
	}
	return "it has not been happening for a while"
}

// Composer turns a candidate into the message's prose. Nil is an ordinary
// configuration, not a degraded one - the model stack exists only when a chat
// transport does, and a proposal nobody can reply to is not worth sending at all
// (the loop is not given a notifier either in that case) - and composeProposal
// templates without treating it as a failure.
type Composer interface {
	Compose(ctx context.Context, c Candidate) (string, error)
}

// composeTemplate is the plain message, and the value composeProposal starts from.
// It states the alternative verbatim, offers leaving things as they are, and says
// nothing that counts or compares.
func composeTemplate(c Candidate) string {
	title := fmt.Sprintf("%q", c.Item.Title)
	switch c.Trigger {
	case domain.TriggerSnoozes:
		return fmt.Sprintf("%s keeps getting pushed back. Want me to %s? Or leave it as it is.", title, c.Alt.Summary)
	case domain.TriggerDormant:
		return fmt.Sprintf("%s has gone quiet. Want me to %s? Or leave it as it is.", title, c.Alt.Summary)
	}
	return fmt.Sprintf("%s hasn't been happening lately. Want me to %s? Or leave it as it is.", title, c.Alt.Summary)
}

// composeProposal is the one place that decides what gets sent. The template is
// computed first and unconditionally, so it is where the function starts and not
// an error path it falls into: D-009's degrade-to-boring as control flow, the
// shape composeCheckIn and composeBriefing already have. fellBack reports that the
// text is the template, whatever the reason.
func composeProposal(ctx context.Context, composer Composer, c Candidate) (text string, fellBack bool, err error) {
	template := composeTemplate(c)
	if composer == nil {
		return template, true, nil
	}
	composed, err := composer.Compose(ctx, c)
	if err != nil {
		return template, true, err
	}
	return composed, false, nil
}

// Accept is the guard between a completion and a send. The message has to state
// the alternative, because the alternative is what "yes" will mean: a message that
// offered something else would have the person agreeing to a change they were
// never shown. It also has to name the item, which with up to three proposals a
// day is the only thing telling them apart. Every failure returns an error so the
// caller falls back rather than repairs.
func Accept(content string, c Candidate) (string, error) {
	text := strings.TrimSpace(content)
	if text == "" {
		return "", fmt.Errorf("composed proposal was empty")
	}
	if n := utf8.RuneCountInString(text); n > maxComposedRunes {
		return "", fmt.Errorf("composed proposal was %d runes, over the %d limit", n, maxComposedRunes)
	}
	low := strings.ToLower(text)
	if !strings.Contains(low, strings.ToLower(c.Item.Title)) {
		return "", fmt.Errorf("composed proposal does not name %q", c.Item.Title)
	}
	for _, group := range c.Alt.Mentions {
		found := false
		for _, spelling := range strings.Split(group, "|") {
			if strings.Contains(low, strings.ToLower(spelling)) {
				found = true
				break
			}
		}
		if !found {
			return "", fmt.Errorf("composed proposal does not state the alternative (%q)", c.Alt.Summary)
		}
	}
	return text, nil
}

// ModelComposer is the model half of the proposal.
type ModelComposer struct {
	client      *model.Client
	routing     *model.Routing
	personaPath string
}

// NewModelComposer returns a composer for task proposal. routing is read for its
// tier count only; the client owns everything else about the call.
func NewModelComposer(client *model.Client, routing *model.Routing, personaPath string) *ModelComposer {
	return &ModelComposer{client: client, routing: routing, personaPath: personaPath}
}

// composeSystemPrompt is the proposal as instructions, with the persona in front
// (systemPrompt) so it sounds like the same app the conversation does. G8 is
// stated here in full and not left to the persona, which is editable: the rule
// about guilt has to survive a persona edited to be blunt.
const composeSystemPrompt = `You are writing one short message for a single-user reminder app, proposing a change to ONE reminder that is not working. The app has already decided what to propose; you are only wording it.

Rules:
- Name the reminder, exactly as given.
- State the proposal exactly as given, in your own words but with every detail of it: the same times, the same days, the same numbers.
- Make clear it is a suggestion and that leaving the reminder as it is stays an option. A plain yes will be taken as agreement, so ask in a way a yes answers.
- One or two sentences. Never guilt, never scold, never say how many times something was missed or snoozed, never use the word "failed" or "missed".
- Reply with the message text only. No greeting, no sign-off, no quotation marks around the whole message, no formatting.`

func (c *ModelComposer) systemPrompt() string {
	persona, err := defaults.GetPersona(c.personaPath)
	if err != nil || persona == "" {
		return composeSystemPrompt
	}
	return persona + "\n\n" + composeSystemPrompt
}

// Compose asks the model for the message, walking tiers until one answers usably.
// It returns an error rather than a fallback: the fallback is composeProposal's to
// apply, so "the model failed" and "there is no model" take one path.
func (c *ModelComposer) Compose(ctx context.Context, cand Candidate) (string, error) {
	messages := []model.Message{
		{Role: model.RoleSystem, Content: c.systemPrompt()},
		{Role: model.RoleUser, Content: fmt.Sprintf("Reminder: %q\nWhy it is being raised: %s\nProposal: %s\n",
			cand.Item.Title, cand.reason(), cand.Alt.Summary)},
	}

	ctx, cancel := context.WithTimeout(ctx, composeBudget)
	defer cancel()

	tiers := c.routing.TierCount(model.TaskProposal)
	var lastErr error
	for tier := 1; tier <= tiers; tier++ {
		var esc *model.Escalation
		if lastErr != nil {
			esc = &model.Escalation{Reason: lastErr.Error()}
		}
		res, err := c.client.Complete(ctx, model.Request{
			Task: model.TaskProposal, Tier: tier, Messages: messages, Escalation: esc,
		})
		if err != nil {
			lastErr = err
			continue
		}
		text, err := Accept(res.Message.Content, cand)
		if err != nil {
			lastErr = err
			continue
		}
		return text, nil
	}

	if lastErr == nil {
		// TierCount returned zero: task proposal is missing from config/model.yaml,
		// as it is in a config directory mounted before this task existed.
		return "", fmt.Errorf("proactive: compose: no tiers configured for task %s", model.TaskProposal)
	}
	return "", fmt.Errorf("proactive: compose: %w", lastErr)
}

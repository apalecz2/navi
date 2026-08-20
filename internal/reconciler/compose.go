package reconciler

import (
	"context"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/aidenpaleczny/navi/internal/defaults"
	"github.com/aidenpaleczny/navi/internal/model"
	"github.com/aidenpaleczny/navi/internal/store"
)

// composeBudget bounds the whole tier walk, and it is deliberately far shorter
// than the tiers' own timeouts (120s and 130s in config/model.yaml).
//
// Supervisor ticks are serial and the interval sleep happens after the body, so
// an unbudgeted two-tier walk would hold this loop for over four minutes: the
// check-in would be that late, ExpireGrace would not run in the meantime, and
// /healthz would report a stale last_tick for a loop that is working fine. The
// composer is the one component here with a good failure mode, so it is the one
// that should give up first. 06-agent-spec's latency budget for this component
// is "seconds".
const composeBudget = 20 * time.Second

// maxComposedRunes is the ceiling on a composed check-in. A model that returns
// an essay has misunderstood the task badly enough that the template is better
// prose, so this is a fallback trigger rather than a truncation point -
// truncating would ship the first 400 runes of something wrong.
const maxComposedRunes = 400

// Composer turns the outstanding set into the one question a pass asks.
//
// Nil is an ordinary configuration rather than a degraded one: the model stack
// exists only when a chat transport does, and a deployment that cannot receive
// a reply has no use for a prettier question. Reconcile templates in that case
// without treating it as a failure.
type Composer interface {
	Compose(ctx context.Context, outstanding []store.Unreconciled) (string, error)
}

// ModelComposer is the model half of D-009's check-in.
type ModelComposer struct {
	client      *model.Client
	routing     *model.Routing
	personaPath string
}

// NewModelComposer returns a composer for task reconcile. routing is read for
// its tier count only; the client owns everything else about the call.
func NewModelComposer(client *model.Client, routing *model.Routing, personaPath string) *ModelComposer {
	return &ModelComposer{client: client, routing: routing, personaPath: personaPath}
}

// composeSystemPrompt is 06-agent-spec's Reconciler composer section as
// instructions. The three rules are its three rules.
//
// "Never accusatory" is stated here rather than left to persona.md because this
// is the first place it can go wrong in a way the template could not. A
// template cannot develop a tone; a model asked to write about things a person
// did not do can, and the failure is invisible until it has already been sent.
// G8's hard rule is repeated rather than assumed for the same reason the
// vocabulary table is injected rather than trusted to be known.
const composeSystemPrompt = `You are writing one short end-of-day check-in message for a single-user reminder app. The user has a few things from today that nobody has recorded an outcome for, and you are asking about them.

Rules:
- One message covering all of them. Never one message per item, and never a list with a line of commentary each.
- It is a question, not an audit. You are asking what happened, not reporting a failure or noting a pattern. Never guilt, never scold, never imply the user has let anything slip - "haven't heard about" is the register, not "you missed".
- Name the items by the titles given, in the order given. Do not invent items, do not drop any, and do not add advice, encouragement, or a summary of how the day went.
- Two or three sentences at most. End by asking which of them got done.
- Reply with the message text only. No greeting, no sign-off, no quotation marks around it, no formatting.`

// Compose asks the model for the check-in text, walking tiers until one answers
// usably.
//
// It returns an error rather than a fallback string. The fallback is
// Reconcile's to apply, so that "the model failed" and "there is no model" take
// the same path and there is one place that decides what gets sent.
//
// The model is given titles and nothing else - no occurrence ids, no statuses,
// no times. That is the property worth protecting: the set of rows that gets a
// reconciled_at is built in Reconcile from the same slice this reads, so
// whatever the model writes, it cannot widen or narrow what was actually asked
// about. A composed message naming an item that was never gathered would be a
// question the grace window then does not answer for.
func (c *ModelComposer) Compose(ctx context.Context, outstanding []store.Unreconciled) (string, error) {
	titles := dedupeTitles(outstanding)
	if len(titles) == 0 {
		return "", fmt.Errorf("reconciler: compose: nothing outstanding")
	}

	messages := []model.Message{
		{Role: model.RoleSystem, Content: c.systemPrompt()},
		{Role: model.RoleUser, Content: "Outstanding today:\n- " + strings.Join(titles, "\n- ")},
	}

	ctx, cancel := context.WithTimeout(ctx, composeBudget)
	defer cancel()

	tiers := c.routing.TierCount(model.TaskReconcile)
	var lastErr error
	for tier := 1; tier <= tiers; tier++ {
		var esc *model.Escalation
		if lastErr != nil {
			esc = &model.Escalation{Reason: lastErr.Error()}
		}

		// No Tools. This is prose, and offering a catalog to a call whose only
		// valid answer is a sentence invites a tool call nobody will execute.
		//
		// No same-tier retry either, unlike the conversation ladder. The ladder
		// retries because its alternative is telling the user it could not
		// understand them; here the alternative is a good template, so spending
		// a second call and another slice of the budget to avoid it is the
		// wrong trade.
		res, err := c.client.Complete(ctx, model.Request{
			Task:       model.TaskReconcile,
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
		// TierCount returned zero, which means the task is missing from
		// config/model.yaml. Routing.Validate rejects that at boot, so this is
		// unreachable rather than merely unlikely - it exists so the loop
		// cannot fall out returning ("", nil) and send an empty check-in.
		return "", fmt.Errorf("reconciler: compose: no tiers configured for task %s", model.TaskReconcile)
	}
	return "", fmt.Errorf("reconciler: compose check-in: %w", lastErr)
}

// composeCheckIn is the one place that decides what a pass actually sends.
//
// The template is computed first and unconditionally, so it is the value the
// function starts from rather than an error path it falls into. That is D-009's
// fallback expressed as control flow: there is no arrangement of failures that
// reaches the send with nothing to send.
//
// A nil composer counts as a fallback too. It is not a failure - it is the
// ordinary shape of a deployment with no chat transport - but the number worth
// having is how often the user got the plain version, and that is the same
// number either way.
func (r *Reconciler) composeCheckIn(ctx context.Context, outstanding []store.Unreconciled) string {
	text := composeTemplate(outstanding)
	if r.composer == nil {
		r.metrics.IncCheckInFallback()
		return text
	}

	composed, err := r.composer.Compose(ctx, outstanding)
	if err != nil {
		// Warn rather than return: a template going out is a working system
		// degrading as designed, not a tick that failed. The model's own error
		// is already an llm_calls row and a navi_llm_calls_total sample, so
		// this line is about which text was chosen, not about what broke.
		r.log.Warn("check-in composition fell back to the template", "err", err)
		r.metrics.IncCheckInFallback()
		return text
	}
	return composed
}

// systemPrompt is the composer's instructions with the persona in front of
// them, so a check-in sounds like the same app the conversation does.
//
// A persona that cannot be read is skipped rather than fatal, matching
// buildSystemPrompt: the rules below already carry the one constraint that
// matters most here, and losing the voice is better than losing the message.
func (c *ModelComposer) systemPrompt() string {
	persona, err := defaults.GetPersona(c.personaPath)
	if err != nil || persona == "" {
		return composeSystemPrompt
	}
	return persona + "\n\n" + composeSystemPrompt
}

// acceptComposed is the guard between a completion and a sent message.
//
// Both failures return an error so the caller falls back rather than repairs.
// An empty completion is a call that succeeded and said nothing, which
// model.Complete reports as success because adequacy is the caller's judgement
// to make; this is that judgement.
func acceptComposed(content string) (string, error) {
	text := strings.TrimSpace(content)
	if text == "" {
		return "", fmt.Errorf("composed check-in was empty")
	}
	if n := utf8.RuneCountInString(text); n > maxComposedRunes {
		return "", fmt.Errorf("composed check-in was %d runes, over the %d limit", n, maxComposedRunes)
	}
	return text, nil
}

// dedupeTitles is composeTemplate's own first step, kept identical on purpose:
// the model and the template are given exactly the same list in exactly the
// same order, so a fallback mid-evening does not change which items get named
// or what order they come in.
func dedupeTitles(outstanding []store.Unreconciled) []string {
	seen := make(map[string]struct{}, len(outstanding))
	titles := make([]string, 0, len(outstanding))
	for _, o := range outstanding {
		if _, ok := seen[o.ItemID]; ok {
			continue
		}
		seen[o.ItemID] = struct{}{}
		titles = append(titles, o.ItemTitle)
	}
	return titles
}

package digest

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/aidenpaleczny/navi/internal/defaults"
	"github.com/aidenpaleczny/navi/internal/model"
)

// composeBudget bounds the whole tier walk. Longer than the other composers'
// 20 seconds because this task runs with thinking on and its tiers are configured
// at 145 and 160 seconds; the compose phase starts 30 minutes ahead, so a walk
// that uses all of it still finishes long before the send. Only this loop's tick
// is held for it, once a week.
const composeBudget = 4 * time.Minute

// maxObservationRunes is the ceiling on the one thing the model writes. An
// essay has misunderstood the task, so this is a fallback trigger, not a
// truncation point.
const maxObservationRunes = 400

// Composer writes the observation: one short passage about the week's figures,
// and nothing else. It does not write the figures and is not trusted with them -
// the loop checks every number in what it returns (CheckNumbers).
//
// Nil is an ordinary configuration: the model stack exists only when a chat
// transport does, and a digest without an observation is the template.
type Composer interface {
	Observe(ctx context.Context, facts string) (string, error)
}

// ModelComposer is the model half of the digest.
type ModelComposer struct {
	client      *model.Client
	routing     *model.Routing
	personaPath string
}

// NewModelComposer returns a composer for task digest.
func NewModelComposer(client *model.Client, routing *model.Routing, personaPath string) *ModelComposer {
	return &ModelComposer{client: client, routing: routing, personaPath: personaPath}
}

// hardRules follow the persona and are not in persona.md, on purpose and for the
// copywriter's reason: G8 is a rule of the system and not a taste, and a persona
// edited to be punitive must not be able to make the weekly summary so.
const hardRules = `Hard rules. They override everything above.
You are given the figures for the user's last week. Write ONE observation about them: something the figures show that the user might not have noticed, such as an item that went well, a streak, a time of day, or how the week compares with the goals. Pick the one thing that is most worth saying.
- Two or three sentences, no more.
- Never guilt, never scold. Do not imply the user has let anything slip, failed, or disappointed you. A missed or skipped reminder is a fact, not a failing; mention one only if it explains something good or neutral.
- Use only numbers that appear in the figures, copied exactly and written as digits. Do not round, convert, estimate, add, subtract, or compute anything: no "about", no "roughly", no percentages that are not given, no "half", no "twice". If you cannot say it without a number that is not given, say it without a number.
- Do not repeat the figures back. They are sent to the user alongside your observation.
- Do not ask a question and do not give advice.
- Reply with the observation text only. No greeting, no label, no quotation marks, no formatting.`

// systemPrompt is the persona with the hard rules after it; a persona that cannot
// be read is skipped, matching the other composers.
func (c *ModelComposer) systemPrompt() string {
	persona, err := defaults.GetPersona(c.personaPath)
	if err != nil || strings.TrimSpace(persona) == "" {
		return hardRules
	}
	return persona + "\n\n" + hardRules
}

// Observe walks the tiers until one answers usably. A transport error or an
// unusable completion (empty, too long) moves to the next tier like every other
// composer; the number check is not here, because a model that invented a number
// is not retried (see composeMessage).
func (c *ModelComposer) Observe(ctx context.Context, facts string) (string, error) {
	messages := []model.Message{
		{Role: model.RoleSystem, Content: c.systemPrompt()},
		{Role: model.RoleUser, Content: "The figures for the week:\n\n" + facts},
	}

	ctx, cancel := context.WithTimeout(ctx, composeBudget)
	defer cancel()

	tiers := c.routing.TierCount(model.TaskDigest)
	var lastErr error
	for tier := 1; tier <= tiers; tier++ {
		var esc *model.Escalation
		if lastErr != nil {
			esc = &model.Escalation{Reason: lastErr.Error()}
		}
		res, err := c.client.Complete(ctx, model.Request{
			Task: model.TaskDigest, Tier: tier, Messages: messages, Escalation: esc,
		})
		if err != nil {
			lastErr = err
			continue
		}
		text, err := acceptObservation(res.Message.Content)
		if err != nil {
			lastErr = err
			continue
		}
		return text, nil
	}
	if lastErr == nil {
		return "", fmt.Errorf("digest: compose: no tiers configured for task %s", model.TaskDigest)
	}
	return "", fmt.Errorf("digest: compose: %w", lastErr)
}

// acceptObservation is the guard between a completion and the number check.
func acceptObservation(content string) (string, error) {
	text := strings.Trim(strings.TrimSpace(content), "\"")
	text = strings.TrimSpace(text)
	if text == "" {
		return "", errors.New("observation was empty")
	}
	if n := utf8.RuneCountInString(text); n > maxObservationRunes {
		return "", fmt.Errorf("observation was %d runes, over the %d limit", n, maxObservationRunes)
	}
	return text, nil
}

// Outcome is how a composed message came to be what it is.
type Outcome struct {
	// Fallback reports that the message is the template although a full digest
	// could have carried an observation.
	Fallback bool

	// Rejected reports that a completion arrived and the number check refused it.
	// It implies Fallback.
	Rejected bool
}

// composeMessage is the one place that decides what a full digest says. The
// template is computed first and unconditionally, so it is the value the function
// starts from rather than an error path it falls into (D-009), and an observation
// replaces it only if it passes both guards.
//
// allowModel is false at the send, where invariant 1 forbids a model call. A
// short digest never calls the model: it has no observation to write.
func composeMessage(ctx context.Context, c Composer, f Figures, allowModel bool, warn func(msg string, args ...any)) (string, Outcome) {
	template := f.Message("")
	if f.Form != FormFull {
		return template, Outcome{}
	}
	if !allowModel || c == nil {
		return template, Outcome{Fallback: true}
	}

	obs, err := c.Observe(ctx, f.Facts)
	if err != nil {
		warn("digest composition fell back to the template", "err", err)
		return template, Outcome{Fallback: true}
	}
	if err := CheckNumbers(f.Facts, obs); err != nil {
		// Refused, not repaired, and not retried on another tier: a model that
		// restated a figure once is as likely to again, and the template is the
		// right answer here. Counted by the caller.
		warn("digest observation refused by the number check", "err", err, "observation", obs)
		return template, Outcome{Fallback: true, Rejected: true}
	}
	return f.Message(obs), Outcome{}
}

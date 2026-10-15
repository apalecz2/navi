package copywriter

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/aidenpaleczny/navi/internal/defaults"
	"github.com/aidenpaleczny/navi/internal/model"
)

// callBudget bounds one model call, far under the tier's own configured timeout,
// for the reason reconciler.composeBudget gives: the copywriter's whole failure
// story is giving up in time for the scheduler to send the title anyway.
const callBudget = 20 * time.Second

// MaxMessageRunes is persona.md's hard length limit (06-agent-spec.md: "under 120
// characters"), enforced in code as well as asked for in the prompt. A message
// over it is rejected, not truncated: the first 120 characters of something that
// ignored the limit is not a shorter version of what was meant.
const MaxMessageRunes = 120

// Request is one generation: the occurrence it is for, the context, and the
// strategy code already chose.
type Request struct {
	OccurrenceID string
	Blob         Blob
	Strategy     Strategy
}

// Output is an accepted message and the model that wrote it.
type Output struct {
	Text  string
	Model string
}

// Generator turns a request into text. Nil is an ordinary configuration - no
// model client - and means every reminder fires as its title.
type Generator interface {
	Generate(ctx context.Context, req Request) (Output, error)
}

// ModelGenerator is the model half of the copywriter: task copywriter, tier one
// only. There is no tier two because escalating a task whose failure mode is
// already acceptable spends money to avoid an outcome that is fine.
type ModelGenerator struct {
	client      *model.Client
	routing     *model.Routing
	personaPath string
}

// NewModelGenerator returns a generator. The persona is read through
// defaults.GetPersona on every call, which is the same function the agent's
// system prompt and both composers call: there is no copy of it here, so an
// edit to persona.md reaches the next generation with no restart.
func NewModelGenerator(client *model.Client, routing *model.Routing, personaPath string) *ModelGenerator {
	return &ModelGenerator{client: client, routing: routing, personaPath: personaPath}
}

// hardRules sit after the persona and are not in persona.md, on purpose. G8 is a
// rule of the system and not a taste: a persona file edited to "be tough on me"
// must not be able to turn the feature punitive, and a file with the rule deleted
// must not either. Placed last so that where the two disagree these win.
const hardRules = `Hard rules. They override everything above.
- Never guilt, never scold. Do not imply the user has let anything slip, failed, or disappointed you.
- Never mention a missed or skipped reminder, how long it has been since the user last did this, or anything they did not do. Never refer to a skip note or its reason.
- One or two short sentences, under 120 characters in total.
- If you state a number, use exactly the number given in the context, as digits.
- recent_messages are what this reminder said before. Do not reuse their wording, their opening, or their angle.
- Reply with the notification text only. No quotation marks, no label, no formatting.`

// systemPrompt is the persona with the hard rules after it. A persona that cannot
// be read is skipped rather than fatal, matching the other three readers.
func (g *ModelGenerator) systemPrompt() string {
	persona, err := defaults.GetPersona(g.personaPath)
	if err != nil || strings.TrimSpace(persona) == "" {
		return hardRules
	}
	return persona + "\n\n" + hardRules
}

// userPrompt is the strategy the code chose and the context it was chosen from.
func userPrompt(req Request) (string, error) {
	blob, err := json.MarshalIndent(req.Blob, "", "  ")
	if err != nil {
		return "", fmt.Errorf("copywriter: encode context: %w", err)
	}
	var b strings.Builder
	b.WriteString("Write the notification text for one reminder.\n\nApproach: ")
	b.WriteString(req.Strategy.instruction())
	b.WriteString("\n\nContext:\n")
	b.Write(blob)
	return b.String(), nil
}

// Generate makes one call. One attempt is one call: the retry budget (G4) is the
// loop's, counted on the row, so that a crash or a timeout still costs an attempt.
func (g *ModelGenerator) Generate(ctx context.Context, req Request) (Output, error) {
	if g.routing.TierCount(model.TaskCopywriter) == 0 {
		return Output{}, fmt.Errorf("copywriter: no tiers configured for task %s", model.TaskCopywriter)
	}
	user, err := userPrompt(req)
	if err != nil {
		return Output{}, err
	}

	ctx, cancel := context.WithTimeout(ctx, callBudget)
	defer cancel()

	occ := req.OccurrenceID
	res, err := g.client.Complete(ctx, model.Request{
		Task: model.TaskCopywriter,
		Tier: 1,
		Messages: []model.Message{
			{Role: model.RoleSystem, Content: g.systemPrompt()},
			{Role: model.RoleUser, Content: user},
		},
		OccurrenceID: &occ,
	})
	if err != nil {
		return Output{}, fmt.Errorf("copywriter: generate: %w", err)
	}
	text, err := accept(res.Message.Content, req.Blob.RecentMessages)
	if err != nil {
		return Output{}, err
	}
	return Output{Text: text, Model: res.Model}, nil
}

// accept is the guard between a completion and a stored message. model.Complete
// reports prose-with-no-content as a success because adequacy is the caller's
// judgement; this is where it is made. Every refusal is an error, so the attempt
// is spent and the row keeps what it has.
func accept(content string, recent []string) (string, error) {
	text := strings.Join(strings.Fields(content), " ")
	text = strings.Trim(text, "\"'`“”")
	text = strings.TrimSpace(text)
	if text == "" {
		return "", fmt.Errorf("copywriter: generated message was empty")
	}
	if n := utf8.RuneCountInString(text); n > MaxMessageRunes {
		return "", fmt.Errorf("copywriter: generated message was %d characters, over the %d limit", n, MaxMessageRunes)
	}
	for _, r := range recent {
		if strings.EqualFold(strings.TrimSpace(r), text) {
			return "", fmt.Errorf("copywriter: generated message repeats a recent one")
		}
	}
	return text, nil
}

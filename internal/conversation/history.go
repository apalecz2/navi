package conversation

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/aidenpaleczny/navi/internal/domain"
	"github.com/aidenpaleczny/navi/internal/model"
	"github.com/aidenpaleczny/navi/internal/transport"
)

// historyLimit is A10's "roughly the last 20 messages" - a design fact, not a
// deployment knob, on the same footing as the loop intervals CLAUDE.md
// reserves for package constants.
const historyLimit = 20

// seedHistory loads the cross-turn history Handle prepends between the
// system prompt and the current turn's user message: up to historyLimit
// prior conversations rows, oldest first, translated back into wire
// messages.
//
// This is deliberately not the same thing as the escalation ladder's own
// in-turn retries. Those are built fresh in messages on every Handle call and
// never persisted as their own conversations rows (finish is the only write
// point), so they never count against historyLimit and never appear in a
// later turn's seeded history - only a turn's terminal outcome does.
//
// One wrinkle: in production the webhook already persists the inbound
// message (for Telegram-redelivery dedup, internal/transport/telegram/
// webhook.go) before Intake ever calls Handle, so the freshly loaded rows
// can include the current turn's own copy as the newest entry. A completed
// turn always ends in an assistant row (Handle's finish), so the only way
// the newest row can be role-user with content matching in.Text is that it
// is this turn's own just-persisted copy - never an earlier turn's. Drop it
// here; Handle appends in.Text itself right after.
func (l *Ladder) seedHistory(ctx context.Context, in transport.IncomingMessage) ([]model.Message, error) {
	rows, err := l.store.ListRecentConversations(ctx, historyLimit+1)
	if err != nil {
		return nil, fmt.Errorf("conversation: load history: %w", err)
	}
	if len(rows) > 0 && rows[0].Role == domain.RoleUser && rows[0].Content == in.Text {
		rows = rows[1:]
	}
	if len(rows) > historyLimit {
		rows = rows[:historyLimit]
	}

	// The trim above can land inside a tool-call pair: persistToolRung
	// always writes the assistant tool-call row before its tool-result row,
	// so the tool row is the newer of the two, and if the cut falls between
	// them the surviving oldest row is a tool-result message with no
	// assistant call anywhere in the window - the pair's older half was cut,
	// not just uninspected. toolCallNames can't invent a name for a row
	// whose pair isn't in rows at all, and neither can Gemini's compat shim,
	// which 400s on a nameless function_response. Only the oldest kept row
	// can ever be orphaned this way (every pair fully inside the window
	// keeps both members, since they're adjacent), so one check suffices.
	if len(rows) > 0 && rows[len(rows)-1].Role == domain.RoleTool {
		rows = rows[:len(rows)-1]
	}

	toolNames, err := toolCallNames(rows)
	if err != nil {
		return nil, fmt.Errorf("conversation: load history: %w", err)
	}

	// rows is newest first; the wire format wants oldest first.
	msgs := make([]model.Message, 0, len(rows))
	for i := len(rows) - 1; i >= 0; i-- {
		msg, err := toModelMessage(rows[i], toolNames)
		if err != nil {
			return nil, fmt.Errorf("conversation: load history: %w", err)
		}
		msgs = append(msgs, msg)
	}
	return msgs, nil
}

// toolCallNames maps every ToolCallID appearing in rows back to the function
// name it invoked, read out of the assistant tool-call row that carries it.
// persistToolRung always writes that row immediately before the matching
// tool-result row (ladder.go's "pair"), but this scans the whole window
// rather than assuming adjacency, since a Gemini-bound tool-role message
// needs its name (model.Message.Name) and the tool-result row itself never
// stores one.
func toolCallNames(rows []domain.Conversation) (map[string]string, error) {
	names := make(map[string]string)
	for _, c := range rows {
		if c.Role != domain.RoleAssistant || c.ToolCalls == nil || *c.ToolCalls == "" {
			continue
		}
		var calls []model.ToolCall
		if err := json.Unmarshal([]byte(*c.ToolCalls), &calls); err != nil {
			return nil, fmt.Errorf("decode stored tool_calls: %w", err)
		}
		for _, tc := range calls {
			names[tc.ID] = tc.Name
		}
	}
	return names, nil
}

// toModelMessage translates one persisted conversations row back into the
// shape model.Client.Complete sends. The inverse of ladder.go's
// persistAssistantProse and persistToolRung: a plain assistant or user row
// round-trips through Content alone, and an assistant tool-call row's
// ToolCalls JSON (written as []model.ToolCall with exactly one entry) is
// decoded back rather than replayed as text. toolNames is toolCallNames'
// output, looked up here rather than recomputed per row.
func toModelMessage(c domain.Conversation, toolNames map[string]string) (model.Message, error) {
	switch c.Role {
	case domain.RoleUser:
		return model.Message{Role: model.RoleUser, Content: c.Content}, nil
	case domain.RoleTool:
		var toolCallID string
		if c.ToolCallID != nil {
			toolCallID = *c.ToolCallID
		}
		return model.Message{
			Role: model.RoleTool, Content: c.Content, ToolCallID: toolCallID,
			Name: toolNames[toolCallID],
		}, nil
	case domain.RoleAssistant:
		msg := model.Message{Role: model.RoleAssistant, Content: c.Content}
		if c.ToolCalls != nil && *c.ToolCalls != "" {
			var calls []model.ToolCall
			if err := json.Unmarshal([]byte(*c.ToolCalls), &calls); err != nil {
				return model.Message{}, fmt.Errorf("decode stored tool_calls: %w", err)
			}
			msg.ToolCalls = calls
		}
		return msg, nil
	default:
		return model.Message{}, fmt.Errorf("unknown conversation role %q in history", c.Role)
	}
}

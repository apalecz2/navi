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

	// rows is newest first; the wire format wants oldest first.
	msgs := make([]model.Message, 0, len(rows))
	for i := len(rows) - 1; i >= 0; i-- {
		msg, err := toModelMessage(rows[i])
		if err != nil {
			return nil, fmt.Errorf("conversation: load history: %w", err)
		}
		msgs = append(msgs, msg)
	}
	return msgs, nil
}

// toModelMessage translates one persisted conversations row back into the
// shape model.Client.Complete sends. The inverse of ladder.go's
// persistAssistantProse and persistToolRung: a plain assistant or user row
// round-trips through Content alone, and an assistant tool-call row's
// ToolCalls JSON (written as []model.ToolCall with exactly one entry) is
// decoded back rather than replayed as text.
func toModelMessage(c domain.Conversation) (model.Message, error) {
	switch c.Role {
	case domain.RoleUser:
		return model.Message{Role: model.RoleUser, Content: c.Content}, nil
	case domain.RoleTool:
		var toolCallID string
		if c.ToolCallID != nil {
			toolCallID = *c.ToolCallID
		}
		return model.Message{Role: model.RoleTool, Content: c.Content, ToolCallID: toolCallID}, nil
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

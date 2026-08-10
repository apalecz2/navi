package conversation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/aidenpaleczny/navi/internal/agent"
	"github.com/aidenpaleczny/navi/internal/domain"
	"github.com/aidenpaleczny/navi/internal/model"
	"github.com/aidenpaleczny/navi/internal/transport"
)

// toolCallInstruction is the synthetic corrective message appended after a
// prose-only reply. It is never persisted to conversations - it is
// ladder-internal machinery to keep the wire transcript valid, not
// something the human or the assistant actually said.
const toolCallInstruction = "You must call exactly one of the available tools to fulfill this request, or call request_escalation if it cannot be done."

// Handle runs one inbound message through the escalation ladder end to end
// (docs/06-agent-spec.md#escalation-ladder). It returns a non-nil error
// only for genuine infrastructure failure - a conversations write or the
// reply Send failing - never for a model or tool outcome: every ladder
// exhaustion is fully handled internally and ends in a sent, persisted
// reply (an execution confirmation, an apology, or a rephrase request).
//
// Tier bookkeeping: for each of routing.TierCount(TaskCRUD) tiers, one
// attempt then, unless the failure was request_escalation or a
// non-retryable *model.Error, one same-tier retry before advancing. A
// validation-shaped failure (bad tool args, unknown tool, no tool call)
// always retries once per tier it visits, producing exactly TierCount*2
// model.Complete calls on total exhaustion - 4 for crud's committed 2-tier
// config, matching the session's acceptance bar. A model.Complete()-level
// failure retries only when its ErrorKind reports Retryable(), and
// terminates immediately when it reports !Escalatable() or no tier remains.
func (l *Ladder) Handle(ctx context.Context, in transport.IncomingMessage) error {
	history, err := l.seedHistory(ctx, in)
	if err != nil {
		return err
	}
	messages := make([]model.Message, 0, len(history)+2)
	messages = append(messages, model.Message{Role: model.RoleSystem, Content: l.buildSystemPrompt(ctx)})
	messages = append(messages, history...)
	messages = append(messages, model.Message{Role: model.RoleUser, Content: in.Text})

	catalog := l.tools.Catalog()
	tierCount := l.routing.TierCount(model.TaskCRUD)

	var lastReason string
	lastWasModelError := false

tierLoop:
	for tier := 1; tier <= tierCount; tier++ {
		retryLeft := 1
		for {
			var esc *model.Escalation
			if lastReason != "" {
				esc = &model.Escalation{Reason: lastReason}
			}

			result, err := l.client.Complete(ctx, model.Request{
				Task:       model.TaskCRUD,
				Tier:       tier,
				Messages:   messages,
				Tools:      catalog,
				Escalation: esc,
			})

			if err != nil {
				lastWasModelError = true
				var mErr *model.Error
				errors.As(err, &mErr)
				lastReason = err.Error()

				if mErr != nil && mErr.Kind.Retryable() && retryLeft > 0 {
					retryLeft--
					continue
				}
				if mErr != nil && !mErr.Kind.Escalatable() {
					break tierLoop // a caller bug (bad task/tier) - no tier fixes it
				}
				break // escalate to the next tier
			}
			lastWasModelError = false

			if len(result.Message.ToolCalls) == 0 {
				if perr := l.persistAssistantProse(ctx, result.Message.Content); perr != nil {
					return perr
				}
				messages = append(messages,
					model.Message{Role: model.RoleAssistant, Content: result.Message.Content},
					model.Message{Role: model.RoleUser, Content: toolCallInstruction},
				)
				lastReason = "no tool call returned"
				if retryLeft > 0 {
					retryLeft--
					continue
				}
				break
			}

			tc := result.Message.ToolCalls[0]
			toolResult, callErr := l.tools.Call(ctx, tc.Name, tc.Arguments)

			var escReq *agent.EscalationRequested
			if errors.As(callErr, &escReq) {
				reason := escReq.Error()
				if perr := l.persistToolRung(ctx, tc, reason); perr != nil {
					return perr
				}
				messages = append(messages,
					model.Message{Role: model.RoleAssistant, ToolCalls: []model.ToolCall{tc}},
					model.Message{Role: model.RoleTool, ToolCallID: tc.ID, Content: reason},
				)
				lastReason = escReq.Reason
				break // to the next tier's first attempt, no retry spent (L4)
			}
			if callErr != nil {
				if perr := l.persistToolRung(ctx, tc, callErr.Error()); perr != nil {
					return perr
				}
				messages = append(messages,
					model.Message{Role: model.RoleAssistant, ToolCalls: []model.ToolCall{tc}},
					model.Message{Role: model.RoleTool, ToolCallID: tc.ID, Content: callErr.Error()},
				)
				lastReason = callErr.Error()
				if retryLeft > 0 {
					retryLeft--
					continue
				}
				break
			}

			// Success.
			if perr := l.persistToolRung(ctx, tc, summarizeResult(toolResult)); perr != nil {
				return perr
			}
			return l.finish(ctx, buildConfirmation(tc.Name, toolResult))
		}
	}

	reply := buildRephrase()
	if lastWasModelError {
		reply = buildApology()
	}
	return l.finish(ctx, reply)
}

// finish persists the terminal assistant row and sends it. It is the single
// exit point for every outcome - success, apology, and rephrase alike -
// which is what makes "one turn produces user, assistant, and tool rows"
// true regardless of which branch the ladder took.
func (l *Ladder) finish(ctx context.Context, reply string) error {
	if err := l.persistAssistantProse(ctx, reply); err != nil {
		return err
	}
	if _, err := l.sender.Send(ctx, transport.Outbound{Body: reply}); err != nil {
		return fmt.Errorf("conversation: send reply: %w", err)
	}
	return nil
}

// persistAssistantProse writes one assistant row carrying real prose and no
// tool call - used for a no-tool-call rung's actual reply and for the
// terminal reply of every outcome.
func (l *Ladder) persistAssistantProse(ctx context.Context, content string) error {
	if _, _, err := l.store.CreateConversation(ctx, domain.NewConversation{
		Role: domain.RoleAssistant, Content: content,
	}); err != nil {
		return fmt.Errorf("conversation: persist assistant row: %w", err)
	}
	return nil
}

// persistToolRung writes the pair of rows one tool-call rung produces: the
// assistant's decision to call tc, and the tool's answer (a validation
// error, an escalation notice, or a success summary).
func (l *Ladder) persistToolRung(ctx context.Context, tc model.ToolCall, resultText string) error {
	tcJSON, err := json.Marshal([]model.ToolCall{tc})
	if err != nil {
		return fmt.Errorf("conversation: marshal tool call: %w", err)
	}
	tcStr := string(tcJSON)

	if _, _, err := l.store.CreateConversation(ctx, domain.NewConversation{
		Role: domain.RoleAssistant, ToolCalls: &tcStr,
	}); err != nil {
		return fmt.Errorf("conversation: persist assistant tool-call row: %w", err)
	}

	toolCallID := tc.ID
	if _, _, err := l.store.CreateConversation(ctx, domain.NewConversation{
		Role: domain.RoleTool, Content: resultText, ToolCallID: &toolCallID,
	}); err != nil {
		return fmt.Errorf("conversation: persist tool row: %w", err)
	}
	return nil
}

// summarizeResult is the tool row's content on a successful call - a
// compact JSON view of what the write actually did, for the audit trail
// rather than for the model (there is no further model call this turn).
func summarizeResult(res agent.Result) string {
	data, err := json.Marshal(res)
	if err != nil {
		return "ok"
	}
	return string(data)
}

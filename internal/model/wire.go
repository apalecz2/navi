package model

import "encoding/json"

// The types below are the OpenAI-compatible chat-completions wire format —
// OpenRouter's own shape, and the shape D-021 asks this client to speak
// without an SDK. They are private and JSON-tagged; the public Message,
// Tool, ToolCall and Result types (model.go) never carry a wire tag, so
// reshaping a provider quirk here never touches the public API.

type chatRequest struct {
	Model    string        `json:"model"`
	Messages []chatMessage `json:"messages"`
	Tools    []chatTool    `json:"tools,omitempty"`

	// Reasoning is OpenRouter's unified switch for thinking mode. Best
	// effort: omitted entirely when the task's routing entry doesn't ask for
	// it, and nothing yet reads it back out of the response.
	Reasoning *chatReasoning `json:"reasoning,omitempty"`
}

type chatReasoning struct {
	Enabled bool `json:"enabled"`
}

type chatMessage struct {
	Role       string         `json:"role"`
	Content    string         `json:"content,omitempty"`
	ToolCalls  []chatToolCall `json:"tool_calls,omitempty"`
	ToolCallID string         `json:"tool_call_id,omitempty"`

	// Name is set on a tool-role message - see Message.Name for why it
	// exists.
	Name string `json:"name,omitempty"`
}

type chatTool struct {
	Type     string       `json:"type"`
	Function chatFunction `json:"function"`
}

type chatFunction struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

type chatToolCall struct {
	ID       string           `json:"id"`
	Type     string           `json:"type"`
	Function chatFunctionCall `json:"function"`

	// ExtraContent is Google's own extension point on a tool call
	// (extra_content.google.thought_signature) — opaque to this file,
	// carried through unmodified. See ToolCall.Extra for why it exists.
	ExtraContent json.RawMessage `json:"extra_content,omitempty"`
}

type chatFunctionCall struct {
	Name string `json:"name"`

	// Arguments is a JSON-encoded string in the wire format, not an embedded
	// object — the outer decode leaves it already unescaped, so casting it
	// straight to json.RawMessage (fromWireMessage) is the whole conversion.
	Arguments string `json:"arguments"`
}

type chatResponse struct {
	Model   string       `json:"model"`
	Choices []chatChoice `json:"choices"`
	Usage   *chatUsage   `json:"usage"`
	Error   *chatAPIErr  `json:"error"`
}

type chatChoice struct {
	Index        int         `json:"index"`
	Message      chatMessage `json:"message"`
	FinishReason string      `json:"finish_reason"`
}

type chatUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
}

// chatAPIErr is how OpenRouter (and most OpenAI-compatible providers) shape
// a failure that still arrives as a 200 body, or as the body of a non-2xx
// response.
type chatAPIErr struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	Code    any    `json:"code"`
}

// toWireRequest builds the request body for one attempt.
func toWireRequest(model string, thinking bool, messages []Message, tools []Tool) chatRequest {
	wire := make([]chatMessage, len(messages))
	for i, m := range messages {
		wire[i] = toWireMessage(m)
	}
	req := chatRequest{
		Model:    model,
		Messages: normalizeToolTurns(wire),
	}
	if len(tools) > 0 {
		req.Tools = make([]chatTool, len(tools))
		for i, t := range tools {
			req.Tools[i] = chatTool{
				Type: "function",
				Function: chatFunction{
					Name:        t.Name,
					Description: t.Description,
					Parameters:  t.Parameters,
				},
			}
		}
	}
	if thinking {
		req.Reasoning = &chatReasoning{Enabled: true}
	}
	return req
}

// toolTurnFiller is the connective tissue normalizeToolTurns inserts. It
// only ever fills a structural gap Gemini's compat shim won't tolerate - a
// gap this codebase leaves on purpose, since the ladder's own in-turn
// nudges (ladder.go's toolCallInstruction) are never persisted and a
// history replay months later has nothing truer to put here.
const toolTurnFiller = "Continue."

// normalizeToolTurns enforces the one structural rule on top of the wire
// format that Gemini's OpenAI-compat shim adds and OpenRouter's real
// OpenAI-compatible backends don't: a message carrying a function call
// (tool_calls) must immediately follow a user turn or a function-response
// (tool) turn. Nothing upstream guarantees that across a turn boundary -
// the ladder persists every rung's real content (persistAssistantProse,
// persistToolRung) but deliberately never persists the synthetic nudge that
// connects a bare-prose rung to the tool-call rung that follows it
// (ladder.go's toolCallInstruction), so a later turn's replayed history can
// legally contain two adjacent assistant-role rows where the second is a
// tool call. Rather than persisting ladder scaffolding to patch that over,
// this patches the wire request at the one place a provider quirk belongs
// (see the package doc): it only ever inserts a filler user turn, never
// drops or reorders anything, so a sequence that was already legal is
// untouched.
func normalizeToolTurns(msgs []chatMessage) []chatMessage {
	out := make([]chatMessage, 0, len(msgs))
	for _, m := range msgs {
		if len(m.ToolCalls) > 0 {
			prevOK := len(out) > 0 &&
				(out[len(out)-1].Role == string(RoleUser) || out[len(out)-1].Role == string(RoleTool))
			if !prevOK {
				out = append(out, chatMessage{Role: string(RoleUser), Content: toolTurnFiller})
			}
		}
		out = append(out, m)
	}
	return out
}

func toWireMessage(m Message) chatMessage {
	wm := chatMessage{
		Role:       string(m.Role),
		Content:    m.Content,
		ToolCallID: m.ToolCallID,
		Name:       m.Name,
	}
	if len(m.ToolCalls) > 0 {
		wm.ToolCalls = make([]chatToolCall, len(m.ToolCalls))
		for i, tc := range m.ToolCalls {
			wm.ToolCalls[i] = chatToolCall{
				ID:   tc.ID,
				Type: "function",
				Function: chatFunctionCall{
					Name:      tc.Name,
					Arguments: string(tc.Arguments),
				},
				ExtraContent: tc.Extra,
			}
		}
	}
	return wm
}

// fromWireMessage converts the assistant message of a chosen completion back
// to the public shape.
func fromWireMessage(wm chatMessage) Message {
	m := Message{
		Role:       Role(wm.Role),
		Content:    wm.Content,
		ToolCallID: wm.ToolCallID,
	}
	if len(wm.ToolCalls) > 0 {
		m.ToolCalls = make([]ToolCall, len(wm.ToolCalls))
		for i, tc := range wm.ToolCalls {
			m.ToolCalls[i] = ToolCall{
				ID:        tc.ID,
				Name:      tc.Function.Name,
				Arguments: json.RawMessage(tc.Function.Arguments),
				Extra:     tc.ExtraContent,
			}
		}
	}
	return m
}

// empty reports whether a decoded chat message carries nothing usable — the
// condition KindEmpty exists for.
func (m chatMessage) empty() bool {
	return m.Content == "" && len(m.ToolCalls) == 0
}

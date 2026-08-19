// Package telegram is the first notification transport that reaches a real
// device, and, since session 8 (P1), the first to receive from one. Send
// implements the outbound half of transport.Transport against the Telegram
// Bot API's sendMessage endpoint; Inbound (webhook.go) is the inbound half,
// verified against a shared secret and filtered by the sender allowlist
// (D8), recording plain messages to conversations.
//
// Transport.Receive has no caller anywhere in this codebase and stays
// unimplemented rather than wired to a live channel: the durable seam for
// whatever consumes inbound messages next (the agent, P1) is the
// conversations table Inbound writes to, not an in-memory channel with no
// restart durability and nothing draining it yet. See ErrReceiveNotImplemented.
//
// Since session 15 (P2) it renders inline keyboards and handles the taps.
// Capabilities declares supports_actions true, Send attaches a keyboard to any
// message carrying both a SubjectID and Actions, and the callback-query branch
// of the webhook turns a tap into the same store call the HTTP endpoint makes.
// Sessions 6 through 14 declared the flag false on purpose — a button whose tap
// goes nowhere is worse than no button — and this is the session it was waiting
// for.
//
// No SDK: the Bot API is plain JSON over HTTPS, so net/http and
// encoding/json are the whole client, on the same reasoning the stack
// decisions give the model client (D-021..D-023). Every method goes through
// call, which is the one place a request is built, sent, and checked for the
// API's own ok flag.
package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"unicode/utf16"

	"github.com/aidenpaleczny/navi/internal/transport"
)

// Name is this adapter's identity in NOTIFY_TRANSPORT and in the log.
const Name = "telegram"

const apiBase = "https://api.telegram.org"

// maxBodyLength is Telegram's own ceiling on sendMessage text, in UTF-16 code
// units. Declared honestly in Capabilities (in runes, per that field's
// contract) and re-enforced here in the adapter's own unit right before the
// call, because a rune count and a UTF-16 code-unit count only agree for text
// with no astral characters.
const maxBodyLength = 4096

// ErrReceiveNotImplemented is returned by Receive. Inbound messages arrive
// through the webhook (Inbound, webhook.go) and are persisted directly to
// conversations, not streamed through this method — see the package doc for
// why a channel isn't the seam here.
var ErrReceiveNotImplemented = errors.New("telegram: receive is not implemented; inbound messages are recorded via the webhook, not streamed")

// Transport sends to one bot, one chat. There is no user table (S1), so both
// are fixed at construction rather than resolved per message.
type Transport struct {
	botToken string
	chatID   string
	apiBase  string
	client   *http.Client
}

// Option adjusts a Transport at construction. There is exactly one, and it
// exists for the same reason the materializer's newRand field does: a part that
// is expensive to get wrong needs somewhere to be exercised without a network.
type Option func(*Transport)

// WithAPIBase points the adapter at something other than Telegram.
//
// Its only caller is cmd/naviseed, which stands an httptest server in for the
// Bot API so that the order of answerCallbackQuery and editMessageText, and the
// absence of a sendMessage beside them, are assertions rather than assumptions.
// Nothing in cmd/navi passes it.
func WithAPIBase(base string) Option {
	return func(t *Transport) { t.apiBase = base }
}

// New returns a Telegram transport. botToken and chatID are both required by
// the caller (internal/config validates this); neither is checked against
// the API here — a bad token is discovered on the first Send, the same way a
// network failure would be.
func New(botToken, chatID string, opts ...Option) *Transport {
	t := &Transport{
		botToken: botToken,
		chatID:   chatID,
		apiBase:  apiBase,
		client:   &http.Client{},
	}
	for _, opt := range opts {
		opt(t)
	}
	return t
}

// ChatID is the chat this adapter delivers to, which is also the chat every
// message it could be asked to edit lives in.
func (t *Transport) ChatID() string { return t.chatID }

// Name identifies the adapter. Nothing branches on it (D-007); it is here for
// the interface, the log line, and the startup warning.
func (t *Transport) Name() string { return Name }

// Capabilities answers honestly for what this adapter does today, not what
// the platform could eventually do.
func (t *Transport) Capabilities() transport.Capabilities {
	return transport.Capabilities{
		// True since session 15: Send renders an inline keyboard and the
		// webhook's callback branch receives the tap. It was false for nine
		// sessions because the handler did not exist, and flipping it is a
		// change inside this adapter — nothing in internal/scheduler moved,
		// which is the only test D-007 gets before a second adapter exists.
		SupportsActions: true,

		// Nothing sets this true until T10, which may never be built. Telegram
		// renders its buttons inside the chat message, not on the lock screen,
		// and D-006 exists to find out whether that difference matters.
		SupportsNativeNotificationActions: false,

		// This session sends unformatted text: no parse_mode. Flips whenever
		// a session starts actually sending Markdown or HTML.
		SupportsRichText: false,

		// editMessageText, which is what lets a tap fold its outcome into the
		// message it came from rather than push a second one after it (N6).
		SupportsMessageEditing: true,

		MaxBodyLength: maxBodyLength,
	}
}

// sendMessageRequest is the subset of Telegram's sendMessage parameters this
// adapter uses: a plain body, no formatting, and a keyboard when there is
// something to tap.
type sendMessageRequest struct {
	ChatID              string          `json:"chat_id"`
	Text                string          `json:"text"`
	DisableNotification bool            `json:"disable_notification,omitempty"`
	ReplyMarkup         *inlineKeyboard `json:"reply_markup,omitempty"`
}

// editMessageTextRequest rewrites a message already in the chat (N6).
//
// ReplyMarkup omitted means the keyboard is dropped, which is what a resolved
// reminder wants: the buttons described something that has now happened. A
// caller that wants the keyboard replaced rather than removed passes one.
type editMessageTextRequest struct {
	ChatID      string          `json:"chat_id"`
	MessageID   int             `json:"message_id"`
	Text        string          `json:"text"`
	ReplyMarkup *inlineKeyboard `json:"reply_markup,omitempty"`
}

// editMessageReplyMarkupRequest swaps the keyboard and leaves the text alone.
// It is how the Snooze button opens R9's four presets without claiming anything
// about the occurrence has changed — because nothing has.
type editMessageReplyMarkupRequest struct {
	ChatID      string          `json:"chat_id"`
	MessageID   int             `json:"message_id"`
	ReplyMarkup *inlineKeyboard `json:"reply_markup,omitempty"`
}

// answerCallbackQueryRequest clears the client's loading spinner and shows the
// outcome as a toast. Telegram requires this within a few seconds of the tap
// regardless of what else happens, so it is sent before the edit.
type answerCallbackQueryRequest struct {
	CallbackQueryID string `json:"callback_query_id"`
	Text            string `json:"text,omitempty"`
	ShowAlert       bool   `json:"show_alert,omitempty"`
}

// apiResponse is the envelope every Bot API method returns. Result is left as
// raw JSON so call can be one function over all four methods; only sendMessage
// looks at it.
type apiResponse struct {
	OK          bool            `json:"ok"`
	Description string          `json:"description,omitempty"`
	ErrorCode   int             `json:"error_code,omitempty"`
	Result      json.RawMessage `json:"result,omitempty"`
}

// messageResult is the part of a sent message this adapter reads back.
type messageResult struct {
	MessageID int `json:"message_id"`
}

// call performs one Bot API method: marshal, post, read, decode, and check the
// API's own ok flag, which is where a Telegram failure actually reports itself —
// a rejected request is frequently an HTTP 200 carrying ok:false.
//
// It is one function rather than four copies because there are four methods now.
// result may be nil for a method whose answer is only success or failure.
func (t *Transport) call(ctx context.Context, method string, payload, result any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("telegram: encode %s: %w", method, err)
	}

	url := fmt.Sprintf("%s/bot%s/%s", t.apiBase, t.botToken, method)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("telegram: build %s request: %w", method, err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := t.client.Do(req)
	if err != nil {
		return fmt.Errorf("telegram: %s: %w", method, err)
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("telegram: read %s response: %w", method, err)
	}

	var out apiResponse
	if err := json.Unmarshal(data, &out); err != nil {
		return fmt.Errorf("telegram: decode %s response (status %d): %w", method, resp.StatusCode, err)
	}
	if !out.OK {
		return fmt.Errorf("telegram: %s: api error %d: %s", method, out.ErrorCode, out.Description)
	}

	if result != nil {
		if len(out.Result) == 0 {
			return fmt.Errorf("telegram: %s: response carried no result", method)
		}
		if err := json.Unmarshal(out.Result, result); err != nil {
			return fmt.Errorf("telegram: decode %s result: %w", method, err)
		}
	}
	return nil
}

// Send makes exactly one attempt, bound entirely by ctx. There is no retry
// loop here: session 5's scheduler already handles a failed send by
// releasing the claim for the next tick, so retrying inside the adapter
// would only hide a slow failure behind a slower one. There is also no
// preflight token check at construction — a bad token surfaces here, on the
// first real send, and is retried exactly like an unreachable host.
func (t *Transport) Send(ctx context.Context, msg transport.Outbound) (string, error) {
	var result messageResult
	err := t.call(ctx, "sendMessage", sendMessageRequest{
		ChatID:              t.chatID,
		Text:                truncateUTF16(msg.Body, maxBodyLength),
		DisableNotification: silent(msg.Priority),
		ReplyMarkup:         keyboardFor(msg),
	}, &result)
	if err != nil {
		return "", err
	}
	return strconv.Itoa(result.MessageID), nil
}

// AnswerCallbackQuery clears the spinner on a tapped button and shows text as a
// toast. It is step four of the adapter's obligations in
// docs/07-api-spec.md#notification-button-taps and runs on every callback,
// including the ones that resolved nothing.
func (t *Transport) AnswerCallbackQuery(ctx context.Context, callbackID, text string, alert bool) error {
	return t.call(ctx, "answerCallbackQuery", answerCallbackQueryRequest{
		CallbackQueryID: callbackID,
		Text:            truncateUTF16(text, maxToastLength),
		ShowAlert:       alert,
	}, nil)
}

// EditMessageText rewrites a message in place, dropping its keyboard unless one
// is supplied. Step five, and the whole of N6 on a transport that can edit.
func (t *Transport) EditMessageText(ctx context.Context, messageID int, text string, kb *inlineKeyboard) error {
	return t.call(ctx, "editMessageText", editMessageTextRequest{
		ChatID:      t.chatID,
		MessageID:   messageID,
		Text:        truncateUTF16(text, maxBodyLength),
		ReplyMarkup: kb,
	}, nil)
}

// EditMessageReplyMarkup swaps a message's keyboard without touching its text.
func (t *Transport) EditMessageReplyMarkup(ctx context.Context, messageID int, kb *inlineKeyboard) error {
	return t.call(ctx, "editMessageReplyMarkup", editMessageReplyMarkupRequest{
		ChatID:      t.chatID,
		MessageID:   messageID,
		ReplyMarkup: kb,
	}, nil)
}

// Receive is not implemented. Returning an error rather than an inert
// channel means a caller finds out immediately, instead of getting an
// inbound half that looks wired and never delivers anything. See the package
// doc and ErrReceiveNotImplemented.
func (t *Transport) Receive(ctx context.Context) (<-chan transport.IncomingMessage, error) {
	return nil, ErrReceiveNotImplemented
}

// silent maps items.priority (1 quietest, 5 loudest, default 3) onto the one
// signal Telegram offers besides normal delivery (N5). Below the default is
// silent; the default and anything louder, including the zero-value
// PriorityUnspecified, is normal.
func silent(p transport.Priority) bool {
	return p == 1 || p == 2
}

// truncateUTF16 cuts s to at most max UTF-16 code units, which is the unit
// Telegram's own limit is denominated in. The scheduler has already truncated
// to Capabilities.MaxBodyLength in runes (transport.Truncate); this is the
// backstop for the gap between a rune count and a UTF-16 code-unit count,
// which only appears for text containing astral characters.
func truncateUTF16(s string, max int) string {
	units := utf16.Encode([]rune(s))
	if len(units) <= max {
		return s
	}
	return string(utf16.Decode(units[:max]))
}

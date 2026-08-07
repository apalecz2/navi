// Package telegram is the first notification transport that reaches a real
// device. It implements the outbound half of transport.Transport against the
// Telegram Bot API's sendMessage endpoint.
//
// Outbound only, at P0. Receive has no consumer until P1 wires conversation,
// so it fails loudly rather than returning a channel nothing ever writes to
// — a premature caller finds out immediately instead of getting an inbound
// half that looks connected and silently never fires. Inline keyboards are
// not rendered here either: Capabilities declares supports_actions false
// because the callback path (P2, /webhook/telegram) does not exist yet, and
// a button whose tap goes nowhere is worse than no button. Both flip in the
// sessions that build their other halves, not here.
//
// No SDK: the Bot API is plain JSON over HTTPS, so net/http and
// encoding/json are the whole client, on the same reasoning the stack
// decisions give the model client (D-021..D-023).
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

// ErrReceiveNotImplemented is returned by Receive until P1 wires conversation.
var ErrReceiveNotImplemented = errors.New("telegram: receive not implemented until P1")

// Transport sends to one bot, one chat. There is no user table (S1), so both
// are fixed at construction rather than resolved per message.
type Transport struct {
	botToken string
	chatID   string
	client   *http.Client
}

// New returns a Telegram transport. botToken and chatID are both required by
// the caller (internal/config validates this); neither is checked against
// the API here — a bad token is discovered on the first Send, the same way a
// network failure would be.
func New(botToken, chatID string) *Transport {
	return &Transport{botToken: botToken, chatID: chatID, client: &http.Client{}}
}

// Name identifies the adapter. Nothing branches on it (D-007); it is here for
// the interface, the log line, and the startup warning.
func (t *Transport) Name() string { return Name }

// Capabilities answers honestly for what this adapter does today, not what
// the platform could eventually do.
func (t *Transport) Capabilities() transport.Capabilities {
	return transport.Capabilities{
		// P2's callback handler doesn't exist yet (/webhook/telegram is not
		// built), so inline keyboards go unrendered here regardless of this
		// flag. Flips to true in the same commit that adds the handler.
		SupportsActions: false,

		// Nothing sets this true until T10, which may never be built.
		SupportsNativeNotificationActions: false,

		// This session sends unformatted text: no parse_mode. Flips whenever
		// a session starts actually sending Markdown or HTML.
		SupportsRichText: false,

		MaxBodyLength: maxBodyLength,
	}
}

// sendMessageRequest is the subset of Telegram's sendMessage parameters this
// adapter uses: a plain body, no formatting, no reply markup.
type sendMessageRequest struct {
	ChatID              string `json:"chat_id"`
	Text                string `json:"text"`
	DisableNotification bool   `json:"disable_notification,omitempty"`
}

type sendMessageResponse struct {
	OK          bool   `json:"ok"`
	Description string `json:"description,omitempty"`
	ErrorCode   int    `json:"error_code,omitempty"`
	Result      *struct {
		MessageID int `json:"message_id"`
	} `json:"result,omitempty"`
}

// Send makes exactly one attempt, bound entirely by ctx. There is no retry
// loop here: session 5's scheduler already handles a failed send by
// releasing the claim for the next tick, so retrying inside the adapter
// would only hide a slow failure behind a slower one. There is also no
// preflight token check at construction — a bad token surfaces here, on the
// first real send, and is retried exactly like an unreachable host.
func (t *Transport) Send(ctx context.Context, msg transport.Outbound) (string, error) {
	body := truncateUTF16(msg.Body, maxBodyLength)

	reqBody, err := json.Marshal(sendMessageRequest{
		ChatID:              t.chatID,
		Text:                body,
		DisableNotification: silent(msg.Priority),
	})
	if err != nil {
		return "", fmt.Errorf("telegram: encode send message: %w", err)
	}

	url := fmt.Sprintf("%s/bot%s/sendMessage", apiBase, t.botToken)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(reqBody))
	if err != nil {
		return "", fmt.Errorf("telegram: build send request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := t.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("telegram: send message: %w", err)
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("telegram: read send response: %w", err)
	}

	var out sendMessageResponse
	if err := json.Unmarshal(data, &out); err != nil {
		return "", fmt.Errorf("telegram: decode send response (status %d): %w", resp.StatusCode, err)
	}
	if !out.OK || out.Result == nil {
		return "", fmt.Errorf("telegram: send message: api error %d: %s", out.ErrorCode, out.Description)
	}

	return strconv.Itoa(out.Result.MessageID), nil
}

// Receive has no consumer until P1 wires conversation. Returning an error
// rather than an inert channel means a caller that reaches for it too early
// finds out immediately, instead of getting an inbound half that looks wired
// and never delivers anything.
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

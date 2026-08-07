package telegram

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/aidenpaleczny/navi/internal/domain"
)

// maxUpdateBytes bounds the body a webhook request is allowed to carry.
// Generous for a text update; it exists to keep a misbehaving or malicious
// sender from handing this handler an unbounded read.
const maxUpdateBytes = 1 << 20 // 1 MiB

// ConversationStore is the one method this handler needs from the
// repository — named locally rather than taking *store.Store, on the same
// discipline internal/httpapi's own Store interface uses.
type ConversationStore interface {
	CreateConversation(ctx context.Context, n domain.NewConversation) (domain.Conversation, bool, error)
}

// Metrics is this handler's slice of the registry.
type Metrics interface {
	IncInboundAccepted(transport string)
	IncInboundDropped(reason string)
}

// Inbound is the webhook half of this adapter: verified against a shared
// secret, then filtered by the sender allowlist (D8). It shares no state
// with the outbound Transport — a botToken and an http.Client versus a
// secret, an allowlist, a store, and a metrics sink — so it is its own type
// rather than a widened one.
//
// It implements http.Handler directly, on the same precedent as
// metrics.Handler(): the package that owns a wire format hands httpapi a
// ready handler rather than httpapi importing this package's internals.
type Inbound struct {
	secret          string
	allowedSenderID string
	store           ConversationStore
	metrics         Metrics
	log             *slog.Logger
}

// NewInbound returns the webhook handler for CHAT_TRANSPORT=telegram.
func NewInbound(secret, allowedSenderID string, store ConversationStore, m Metrics, log *slog.Logger) *Inbound {
	return &Inbound{secret: secret, allowedSenderID: allowedSenderID, store: store, metrics: m, log: log}
}

// update is the subset of Telegram's Update this handler decodes: enough to
// route correctly without acting on anything but a plain message this
// session. from() reads whichever of message or callback_query is present,
// so an allowlisted user's button tap (no buttons exist until P2, but the
// Update shape does not know that) is recognised as allowlisted rather than
// miscounted as a stranger.
type update struct {
	UpdateID      int64            `json:"update_id"`
	Message       *tgMessage       `json:"message"`
	CallbackQuery *tgCallbackQuery `json:"callback_query"`
}

type tgMessage struct {
	From *tgUser `json:"from"`
	Text string  `json:"text"`
}

type tgCallbackQuery struct {
	From *tgUser `json:"from"`
}

type tgUser struct {
	ID int64 `json:"id"`
}

func (u update) from() *tgUser {
	switch {
	case u.Message != nil:
		return u.Message.From
	case u.CallbackQuery != nil:
		return u.CallbackQuery.From
	default:
		return nil
	}
}

// ServeHTTP verifies the secret, decodes the envelope, checks the allowlist,
// and — for a plain message from the allowlisted sender — records it.
//
// Order: secret first, so a wrong or missing header never causes the body to
// be read at all; then decode, because the sender id lives inside the JSON
// and cannot be checked before it is parsed; then the allowlist; then the
// write. A non-message update from the allowlisted sender (an edited
// message, a channel post, a callback query) is acknowledged and ignored —
// neither accepted nor dropped — since P2 is what gives any of those
// somewhere to go.
func (h *Inbound) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !h.secretValid(r.Header.Get("X-Telegram-Bot-Api-Secret-Token")) {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxUpdateBytes))
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	var upd update
	if err := json.Unmarshal(body, &upd); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	sender := upd.from()
	if sender == nil || strconv.FormatInt(sender.ID, 10) != h.allowedSenderID {
		// D8: dropped silently. No reply, no error to the sender, and nothing
		// here names who it was or what it said — the counter is the only
		// trace.
		h.metrics.IncInboundDropped("allowlist")
		w.WriteHeader(http.StatusOK)
		return
	}

	if upd.Message == nil {
		w.WriteHeader(http.StatusOK)
		return
	}

	transportName := Name
	externalID := strconv.FormatInt(upd.UpdateID, 10)
	_, inserted, err := h.store.CreateConversation(r.Context(), domain.NewConversation{
		Role:       domain.RoleUser,
		Content:    upd.Message.Text,
		Transport:  &transportName,
		ExternalID: &externalID,
	})
	if err != nil {
		h.log.Error("webhook: create conversation", "err", err)
		// 500 rather than swallowing the error: Telegram retries a non-2xx,
		// and the dedup check makes that retry safe, so there is no reason to
		// pretend a write failure succeeded.
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	if inserted {
		h.metrics.IncInboundAccepted(Name)
	}
	w.WriteHeader(http.StatusOK)
}

// secretValid compares in constant time so response timing cannot be used to
// guess the secret one byte at a time. An empty configured secret or an
// empty header is always invalid — CHAT_TRANSPORT=telegram requires
// TELEGRAM_WEBHOOK_SECRET (internal/config), so an empty h.secret here would
// mean this handler was constructed wrong, not that the check should pass.
func (h *Inbound) secretValid(got string) bool {
	if h.secret == "" || got == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(got), []byte(h.secret)) == 1
}

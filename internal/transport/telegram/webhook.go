package telegram

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/aidenpaleczny/navi/internal/domain"
	"github.com/aidenpaleczny/navi/internal/store"
	"github.com/aidenpaleczny/navi/internal/transport"
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

// Dispatcher hands an accepted inbound message to whatever processes it
// next — internal/conversation.Intake, since P1. Optional: nil means
// nothing is wired to consume messages, the same as before this session.
// Enqueue must not block: the webhook has to return 2xx promptly, well
// before a model call could finish, so it reports whether the message was
// accepted rather than waiting for room.
type Dispatcher interface {
	Enqueue(msg transport.IncomingMessage) bool
}

// Resolver is what a button tap needs from the repository: the same two
// methods internal/httpapi calls, reached the same way.
//
// It is deliberately not a narrower "resolve this for me" helper. D-014 bought
// one state machine across three surfaces, and the way that stays true is that
// the third surface calls the same two store methods rather than something
// written for it — every transition rule stays inside domain.Transition, which
// both of these already ask.
type Resolver interface {
	ResolveOccurrence(ctx context.Context, id string, to domain.Status, note *string,
		source domain.ResolutionSource, now time.Time) (store.Resolution, error)

	SnoozeOccurrence(ctx context.Context, id string, source domain.ResolutionSource,
		now time.Time, at func(domain.Item, domain.Occurrence) (time.Time, error)) (store.Snooze, error)

	CurrentTZ(ctx context.Context) (string, bool, error)
}

// Metrics is this handler's slice of the registry.
type Metrics interface {
	IncInboundAccepted(transport string)
	IncInboundDropped(reason string)
	IncTransition(from, to, source string)
}

// Inbound is the webhook half of this adapter: verified against a shared
// secret, then filtered by the sender allowlist (D8).
//
// It holds the outbound Transport rather than duplicating a bot token and an
// http.Client, because since session 15 it answers taps: a callback query is
// replied to on the same bot it was sent from, and the message it came from is
// edited in place (N6). It is still its own type — the outbound half has no
// secret, no allowlist and no store — but it is no longer stateless with
// respect to the API.
//
// It implements http.Handler directly, on the same precedent as
// metrics.Handler(): the package that owns a wire format hands httpapi a
// ready handler rather than httpapi importing this package's internals.
type Inbound struct {
	secret          string
	allowedSenderID string
	store           ConversationStore
	resolver        Resolver
	api             *Transport
	dispatcher      Dispatcher
	metrics         Metrics
	defaultTZ       *time.Location
	log             *slog.Logger
}

// NewInbound returns the webhook handler for CHAT_TRANSPORT=telegram.
//
// dispatcher may be nil, in which case an accepted message is recorded but
// nothing is ever notified to process it — the state naviseed's
// webhook-only checks still exercise.
//
// resolver and api may both be nil, in which case a callback query is
// acknowledged and ignored exactly as it was before this session. They are not
// separately optional: a tap that resolves without answering leaves a spinner
// on the user's screen, and one that answers without resolving lies.
func NewInbound(
	secret, allowedSenderID string,
	store ConversationStore,
	resolver Resolver,
	api *Transport,
	dispatcher Dispatcher,
	m Metrics,
	defaultTZ *time.Location,
	log *slog.Logger,
) *Inbound {
	return &Inbound{
		secret:          secret,
		allowedSenderID: allowedSenderID,
		store:           store,
		resolver:        resolver,
		api:             api,
		dispatcher:      dispatcher,
		metrics:         m,
		defaultTZ:       defaultTZ,
		log:             log,
	}
}

// update is the subset of Telegram's Update this handler decodes: enough to
// route a plain message and a button tap and nothing else. from() reads
// whichever of message or callback_query is present, so both kinds go through
// one allowlist check before they diverge.
type update struct {
	UpdateID      int64            `json:"update_id"`
	Message       *tgMessage       `json:"message"`
	CallbackQuery *tgCallbackQuery `json:"callback_query"`
}

// tgMessage carries MessageID and Chat because a callback query arrives with
// the message its button was attached to, and editing that message in place
// (N6) needs both. For an inbound plain message they are decoded and unused.
type tgMessage struct {
	MessageID int     `json:"message_id"`
	Chat      *tgChat `json:"chat"`
	From      *tgUser `json:"from"`
	Text      string  `json:"text"`
}

type tgChat struct {
	ID int64 `json:"id"`
}

// tgCallbackQuery is one button tap. ID is what answerCallbackQuery clears the
// spinner with, Data is the payload callback.go framed, and Message is where
// the outcome gets folded back in.
type tgCallbackQuery struct {
	ID      string     `json:"id"`
	From    *tgUser    `json:"from"`
	Data    string     `json:"data"`
	Message *tgMessage `json:"message"`
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
// and then routes on what kind of update arrived.
//
// Order: secret first, so a wrong or missing header never causes the body to
// be read at all; then decode, because the sender id lives inside the JSON
// and cannot be checked before it is parsed; then the allowlist; then the
// work. That is steps one and two of
// docs/07-api-spec.md#notification-button-taps, and it is the same order for
// both kinds of update — which is the point, because it is why a tap needs no
// authentication of its own and Q6's signed action tokens stay unbuilt.
//
// The two kinds diverge immediately afterwards. A message normalizes into an
// IncomingMessage and is enqueued for the agent. A callback query never reaches
// the agent at all: it decodes to an occurrence and an action and goes straight
// to the resolve path, because a tap is an instruction that has already been
// unambiguously expressed. Any other update from the allowlisted sender (an
// edited message, a channel post) is still acknowledged and ignored.
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

	if upd.CallbackQuery != nil {
		h.handleCallback(r.Context(), upd.CallbackQuery)

		// Always 200, whatever the tap did. A non-2xx makes Telegram redeliver
		// the update, and a redelivered tap is another call into the state
		// machine — which is safe, but pointless for a payload that will fail
		// to decode every time. No conversations row and no update_id dedup
		// either: 07-api-spec is explicit that no deduplication lives in the
		// adapter, because a double tap is the state machine's second
		// idempotency row and nothing else. The agent still sees the outcome,
		// because context injection already reports today's occurrences with
		// their status and resolution.
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

		// Only a genuinely new message is handed off — never a Telegram
		// redelivery that hit the dedup path, which would otherwise
		// process the same request twice. Enqueue is non-blocking, so a
		// full queue never delays this response; the message is already
		// durably recorded above regardless of whether it fits.
		if h.dispatcher != nil {
			msg := transport.IncomingMessage{
				SenderID:   strconv.FormatInt(sender.ID, 10),
				Text:       upd.Message.Text,
				Transport:  Name,
				ExternalID: externalID,
				ReceivedAt: time.Now(),
			}
			if !h.dispatcher.Enqueue(msg) {
				h.metrics.IncInboundDropped("queue_full")
			}
		}
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

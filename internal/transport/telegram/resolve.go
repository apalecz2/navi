package telegram

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/aidenpaleczny/navi/internal/domain"
	"github.com/aidenpaleczny/navi/internal/schedule"
	"github.com/aidenpaleczny/navi/internal/store"
	"github.com/aidenpaleczny/navi/internal/transport"
)

// apiTimeout bounds the two API calls a tap makes after its write has already
// committed.
const apiTimeout = 10 * time.Second

// callbackReply is what a tap turned into: a toast, and optionally a rewrite of
// the message the button was attached to.
//
// Every field here is read off what the store returned — the outcome, the
// resulting status, the state machine's own message, the chain. None of it is
// re-derived, which is what keeps this from becoming a second copy of the
// transition rules living beside the real one. It is the same four values
// internal/httpapi renders its response envelope from.
type callbackReply struct {
	toast string

	// alert shows the toast as a dialog the user has to dismiss rather than a
	// banner that fades. Reserved for outcomes worth stopping at: the snooze cap.
	alert bool

	// suffix is appended to the original message text. Empty means the message
	// is left exactly as it was, which is right whenever nothing was resolved.
	suffix string
}

// handleCallback turns one button tap into a resolution, in the order
// docs/07-api-spec.md#notification-button-taps lists. The secret and the
// allowlist have already been checked by ServeHTTP, which is steps one and two.
//
// Nothing here reports an error upward. The caller answers Telegram with 200
// regardless (see ServeHTTP), so a failure is a log line and a toast, which is
// the only place a user could see it anyway.
func (h *Inbound) handleCallback(ctx context.Context, q *tgCallbackQuery) {
	if h.resolver == nil || h.api == nil {
		// Nothing is wired to resolve, so a tap is acknowledged and ignored the
		// way every non-message update was before this session.
		h.log.Debug("webhook: callback with no resolver wired")
		return
	}

	cb, ok := decodeCallback(q.Data)
	if !ok {
		// Step three never happens: no store call, no edit, and the keyboard is
		// left alone because nothing was resolved. The counter is the only trace
		// of the payload, which is deliberate — it is attacker-controlled text.
		h.metrics.IncInboundDropped("callback_decode")
		h.answer(ctx, q, callbackReply{toast: "This button is no longer valid."})
		return
	}

	if !actionResolves(cb.Action) {
		h.swapKeyboard(ctx, q, cb)
		return
	}

	reply := h.resolveCallback(ctx, cb)

	// Step four before step five: the spinner is cleared first, because
	// Telegram wants an answer within seconds and the edit is the slower of the
	// two. A 409 is answered here just like anything else, with the state
	// machine's own account of the current state as the toast.
	h.answer(ctx, q, reply)

	if reply.suffix != "" {
		h.fold(ctx, q, reply.suffix)
	}
}

// resolveCallback performs the write and reports what to say about it.
func (h *Inbound) resolveCallback(ctx context.Context, cb callback) callbackReply {
	switch cb.Action {
	case actionComplete:
		return h.resolve(ctx, cb.OccurrenceID, domain.StatusCompleted)
	case actionSkip:
		return h.resolve(ctx, cb.OccurrenceID, domain.StatusSkipped)
	case actionSnooze:
		return h.snooze(ctx, cb.OccurrenceID, schedule.Delta(cb.Arg))
	}

	// Unreachable: handleCallback checks actionResolves first, and
	// decodeCallback rejects anything outside the set.
	return callbackReply{toast: "This button is no longer valid."}
}

// resolve is Done and Skip. It is the same store method
// POST /api/occurrences/{id}/resolve calls, with the same arguments but a
// different source — which is the whole of what "one state machine, many
// surfaces" means in code (D-014, invariant 4).
func (h *Inbound) resolve(ctx context.Context, id string, to domain.Status) callbackReply {
	res, err := h.resolver.ResolveOccurrence(ctx, id, to, nil, domain.ResolvedByNotification, time.Now())
	if err != nil {
		return h.failure(err, "resolve occurrence", id)
	}

	if res.Outcome == domain.OutcomeApplied {
		h.metrics.IncTransition(string(res.Previous), string(res.Occurrence.Status),
			string(domain.ResolvedByNotification))
	}

	word := outcomeWord(res.Occurrence.Status)
	if res.Outcome == domain.OutcomeNoop {
		// The second of two taps. The keyboard is usually gone by now; when it
		// is not, this is the idempotency table's second row and nothing was
		// written.
		return callbackReply{toast: "Already " + word + ".", suffix: " — " + word}
	}
	return callbackReply{toast: "Marked " + word + ".", suffix: " — " + word}
}

// snooze is a tap on one of R9's four presets, reached through the menu.
//
// The delta resolves through schedule.SnoozeAt, the same constructor the HTTP
// endpoint uses, so the presets cannot mean one thing here and another there —
// including across a DST boundary, which is the case that would otherwise
// diverge silently.
func (h *Inbound) snooze(ctx context.Context, id string, delta schedule.Delta) callbackReply {
	zones, err := schedule.LoadZones(ctx, h.resolver, h.defaultTZ)
	if err != nil {
		h.log.Error("webhook: callback resolve zones", "occurrence", id, "err", err)
		return callbackReply{toast: "Something went wrong. Try again."}
	}

	now := time.Now()
	at := schedule.SnoozeAt(zones, delta, now,
		func(item domain.Item, loc *time.Location, startsAt time.Time, fold schedule.Fold) {
			if fold == schedule.FoldNone {
				return
			}
			h.log.Debug("snooze: dst boundary", "occurrence", id, "item", item.ID,
				"delta", string(delta), "zone", loc.String(), "fold", fold.String(),
				"instant", domain.FormatTime(startsAt))
		})

	res, err := h.resolver.SnoozeOccurrence(ctx, id, domain.ResolvedByNotification, now, at)
	if err != nil {
		return h.failure(err, "snooze occurrence", id)
	}

	// The cap: the chain resolved as missed rather than acquiring another link
	// (R8). The write happened, so the edge is counted, and the note the store
	// composed is exactly what the toast is supposed to report.
	if res.CapReached {
		h.metrics.IncTransition(string(res.Previous), string(res.Parent.Status),
			string(domain.ResolvedByNotification))

		note := "the snooze cap has been reached"
		if res.Parent.ResolutionNote != nil {
			note = *res.Parent.ResolutionNote
		}
		return callbackReply{
			toast:  note + " — marked missed.",
			alert:  true,
			suffix: " — missed",
		}
	}

	if res.Outcome == domain.OutcomeApplied {
		h.metrics.IncTransition(string(res.Previous), string(res.Parent.Status),
			string(domain.ResolvedByNotification))
	}

	when := localTime(res.Child.StartsAt, displayZone(zones), now)
	toast := "Pushed to " + when + "."
	if res.Chain.SnoozeCount > 1 {
		// The chain, not the row. Reading it off store.Snooze is why the
		// endpoint returns it — no second query, and no walking
		// parent_occurrence_id here (D-011, R7).
		toast = fmt.Sprintf("Pushed to %s, snoozed %d×.", when, res.Chain.SnoozeCount)
	}
	return callbackReply{toast: toast, suffix: " — snoozed to " + when}
}

// failure renders a store error. The three cases are the three
// internal/httpapi distinguishes, mapped onto a toast instead of a status code.
func (h *Inbound) failure(err error, op, id string) callbackReply {
	var te *domain.TransitionError
	var ve *domain.ValidationError

	switch {
	case errors.As(err, &te):
		// The 409. te.Message already names the current state — that is what it
		// was written for — so the toast reports it without this layer working
		// out which of the two 409 flavours it was.
		return callbackReply{toast: te.Message, suffix: " — " + outcomeWord(te.From)}

	case errors.Is(err, store.ErrNotFound):
		// A well-formed id for a row that is gone. Not the same as a payload
		// that would not decode, and worth saying differently.
		return callbackReply{toast: "That reminder no longer exists."}

	case errors.As(err, &ve):
		// The delta could not be resolved against this item — an unreadable
		// stored schedule, or a timezone that no longer loads.
		return callbackReply{toast: ve.Message}

	default:
		h.log.Error("webhook: "+op, "occurrence", id, "err", err)
		return callbackReply{toast: "Something went wrong. Try again."}
	}
}

// swapKeyboard is the menu and back actions: R9's four presets in place of the
// three buttons, and the way back.
//
// It writes nothing and never asks the state machine anything, which is the
// property that makes a two-stage snooze safe. A tap that only changes what is
// on screen must not be capable of resolving an occurrence.
func (h *Inbound) swapKeyboard(ctx context.Context, q *tgCallbackQuery, cb callback) {
	h.answer(ctx, q, callbackReply{})

	if q.Message == nil {
		return
	}

	kb := actionKeyboard(cb.OccurrenceID)
	if cb.Action == actionMenu {
		kb = snoozeKeyboard(cb.OccurrenceID)
	}

	apiCtx, cancel := h.apiContext(ctx)
	defer cancel()

	if err := h.api.EditMessageReplyMarkup(apiCtx, q.Message.MessageID, kb); err != nil {
		// Nothing to fall back to: a confirmation message carrying no buttons
		// is not a snooze menu. The user still has the original keyboard.
		h.log.Error("webhook: swap keyboard", "occurrence", cb.OccurrenceID, "err", err)
	}
}

// answer clears the spinner. Its failure is logged and never propagated: the
// write it is reporting on has already committed, and a lost toast is not a
// reason to make Telegram redeliver a tap that already took effect.
func (h *Inbound) answer(ctx context.Context, q *tgCallbackQuery, reply callbackReply) {
	apiCtx, cancel := h.apiContext(ctx)
	defer cancel()

	if err := h.api.AnswerCallbackQuery(apiCtx, q.ID, reply.toast, reply.alert); err != nil {
		h.log.Error("webhook: answer callback query", "err", err)
	}
}

// fold rewrites the original message to show the outcome and drops its
// keyboard, so a resolved reminder leaves one message in the chat rather than
// two (N6).
//
// The fallback is N6's other half, and it fires for two different reasons: a
// transport that cannot edit at all, which is a capability question and never a
// name one (T5, D-007), and an edit that was refused — a message too old, or one
// the user deleted. Both end the same way, with the short confirmation sent as
// its own message.
func (h *Inbound) fold(ctx context.Context, q *tgCallbackQuery, suffix string) {
	text := suffix
	if q.Message != nil && q.Message.Text != "" {
		text = q.Message.Text + suffix
	}

	apiCtx, cancel := h.apiContext(ctx)
	defer cancel()

	if h.api.Capabilities().SupportsMessageEditing && q.Message != nil {
		if err := h.api.EditMessageText(apiCtx, q.Message.MessageID, text, nil); err == nil {
			return
		} else {
			h.log.Warn("webhook: edit message, falling back to a confirmation", "err", err)
		}
	}

	// No SubjectID, so no keyboard: the confirmation describes something that
	// has already happened and there is nothing left to tap.
	if _, err := h.api.Send(apiCtx, transport.Outbound{Body: text}); err != nil {
		h.log.Error("webhook: send confirmation", "err", err)
	}
}

// apiContext detaches the API calls from the webhook request.
//
// The write has already committed by the time either of them runs, and the case
// that most needs a toast is the one where Telegram has given up waiting for
// this response — the same reasoning scheduler.release is built on.
func (h *Inbound) apiContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), apiTimeout)
}

// outcomeWord renders a status the way a person would say it. It is display
// only; nothing parses it back.
func outcomeWord(s domain.Status) string {
	switch s {
	case domain.StatusCompleted:
		return "done"
	case domain.StatusSkipped:
		return "skipped"
	case domain.StatusMissed:
		return "missed"
	case domain.StatusSnoozed:
		return "snoozed"
	}
	return string(s)
}

// displayZone is where the user's clock is, which is not necessarily where the
// delta resolved.
//
// A fixed item resolves its wall clocks in its own zone — that is what fixed
// means — but the person reading the toast is reading it on a device in one
// place. So the resolution uses the item's zone and the report uses the
// device's.
func displayZone(zones schedule.Zones) *time.Location {
	if zones.Device != nil {
		return zones.Device
	}
	if zones.Fallback != nil {
		return zones.Fallback
	}
	return time.UTC
}

// localTime renders an instant for a person. The date is included only when it
// is not today, because "09:00" for tomorrow morning reads as nine minutes from
// now on a reminder snoozed at 08:51.
func localTime(at time.Time, loc *time.Location, now time.Time) string {
	local := at.In(loc)
	if local.Format("2006-01-02") == now.In(loc).Format("2006-01-02") {
		return local.Format("15:04")
	}
	return local.Format("Mon 15:04")
}

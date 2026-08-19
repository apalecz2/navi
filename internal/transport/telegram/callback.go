package telegram

import (
	"strings"

	"github.com/aidenpaleczny/navi/internal/domain"
	"github.com/aidenpaleczny/navi/internal/schedule"
	"github.com/aidenpaleczny/navi/internal/transport"
)

// callback_data is how a tap gets home on Telegram, and it is entirely this
// adapter's business: the shared vocabulary describes three actions and names
// the occurrence they act on, and says nothing about the wire (D-007, and the
// Action doc comment in internal/transport).
//
// The framing is version-prefixed and colon-separated:
//
//	n1:<action>:<occurrence-id>[:<arg>]
//
// The worst case is a snooze preset —
// n1:snooze:01ARZ3NDEKTSV4RRFFQ69G5FAV:tomorrow, 45 bytes — against Telegram's
// 64-byte ceiling, so a 26-character ULID and an action name fit with room to
// spare. That budget is exactly why docs/07-api-spec.md#action-tokens declines
// to build the signed-token scheme here: callback_data carries the pair over a
// channel this service already authenticates.
//
// Colon separates unambiguously. A ULID is Crockford base32, the action names
// are lowercase ASCII, and the four deltas are 10m, 1h, tonight and tomorrow —
// none of them can contain one.
//
// The n1 prefix exists so a later change of format is *distinguishable* rather
// than ambiguous. A button rendered before a redeploy and tapped after it
// decodes as an unknown version and is refused, instead of being parsed under
// the new rules into some other occurrence's id.
const codecVersion = "n1"

// maxCallbackData is Telegram's own ceiling on callback_data, in bytes. Nothing
// this codec produces approaches it; encodeCallback checks anyway, because the
// failure it prevents is a button that silently does not render.
const maxCallbackData = 64

// maxToastLength is Telegram's ceiling on answerCallbackQuery text.
const maxToastLength = 200

// The actions a callback_data payload can name.
//
// The first three are transport.Action ids, unchanged, so the vocabulary the
// scheduler attaches and the vocabulary the webhook parses are the same one.
// The last two are this adapter's alone and resolve nothing: they open and close
// the snooze preset keyboard. They are separated from the resolving actions
// everywhere it matters — see actionResolves.
const (
	actionComplete = transport.ActionComplete
	actionSnooze   = transport.ActionSnooze
	actionSkip     = transport.ActionSkip

	// actionMenu opens R9's four presets in place of the three buttons.
	actionMenu = "menu"

	// actionBack closes them again.
	actionBack = "back"
)

// actionResolves reports whether an action asks the state machine for anything.
// menu and back do not: they are keyboard rendering and nothing else, which is
// what keeps a two-stage snooze from ever being mistaken for a resolution.
func actionResolves(action string) bool {
	switch action {
	case actionComplete, actionSnooze, actionSkip:
		return true
	}
	return false
}

// knownAction reports whether an action is one this codec emits at all.
func knownAction(action string) bool {
	return actionResolves(action) || action == actionMenu || action == actionBack
}

// encodeCallback builds the payload for one button. arg is empty for every
// action but snooze.
//
// An over-long payload returns "", which callers render as no button rather
// than as a button Telegram will reject. Nothing in this adapter can produce
// one — the arithmetic above is fixed by the ULID length and the closed delta
// set — so this is a guard against a future action name, not a live case.
func encodeCallback(action, occurrenceID, arg string) string {
	data := codecVersion + ":" + action + ":" + occurrenceID
	if arg != "" {
		data += ":" + arg
	}
	if len(data) > maxCallbackData {
		return ""
	}
	return data
}

// callback is a decoded tap.
type callback struct {
	Action       string
	OccurrenceID string
	Arg          string
}

// decodeCallback parses a payload back, reporting whether it is one this
// service produced.
//
// Every rejection is the same answer to the caller — see Inbound.handleCallback,
// which toasts and writes nothing — but they are different questions and it is
// worth being able to read which one failed:
//
//   - a version this build does not know
//   - the wrong number of fields
//   - an action outside the set
//   - an id that is not a ULID
//   - an arg on an action that takes none, or a snooze without one
//
// The delta itself is not checked here. schedule.Delta.Valid is the closed set
// and checking it twice is how two copies of a closed set begin; an unknown
// delta fails when the snooze closure resolves it, with the message that names
// the four.
func decodeCallback(data string) (callback, bool) {
	parts := strings.Split(data, ":")
	if len(parts) < 3 || len(parts) > 4 {
		return callback{}, false
	}
	if parts[0] != codecVersion {
		return callback{}, false
	}

	cb := callback{Action: parts[1], OccurrenceID: parts[2]}
	if len(parts) == 4 {
		cb.Arg = parts[3]
	}

	if !knownAction(cb.Action) {
		return callback{}, false
	}
	if !domain.ValidID(cb.OccurrenceID) {
		return callback{}, false
	}
	// An arg belongs to snooze and to nothing else. A complete carrying one is
	// not a payload this codec wrote.
	if (cb.Action == actionSnooze) != (cb.Arg != "") {
		return callback{}, false
	}
	return cb, true
}

// inlineKeyboard is Telegram's reply_markup for tappable buttons attached to a
// message.
type inlineKeyboard struct {
	InlineKeyboard [][]inlineButton `json:"inline_keyboard"`
}

type inlineButton struct {
	Text         string `json:"text"`
	CallbackData string `json:"callback_data"`
}

// keyboardFor renders an outbound message's actions, or nil when there is
// nothing to render.
//
// Nil for a message with no SubjectID is the load-bearing case rather than a
// defensive one: the conversation ladder sends replies through this same
// adapter with no subject behind them, and this is what keeps those
// keyboard-free without anyone asking why the message was sent.
func keyboardFor(msg transport.Outbound) *inlineKeyboard {
	if msg.SubjectID == "" || len(msg.Actions) == 0 {
		return nil
	}

	row := make([]inlineButton, 0, len(msg.Actions))
	for _, a := range msg.Actions {
		// Snooze is the one action that does not resolve on the first tap. N4
		// describes one button and R9 defines four presets, and picking a
		// default would put three of them out of reach on the only transport
		// there is — so the button opens them instead. Two-stage rendering is a
		// rendering decision and lives here; the scheduler still describes three
		// actions and knows nothing about it.
		action := a.ID
		if action == actionSnooze {
			action = actionMenu
		}

		data := encodeCallback(action, msg.SubjectID, a.Arg)
		if data == "" {
			continue
		}
		row = append(row, inlineButton{Text: a.Label, CallbackData: data})
	}
	if len(row) == 0 {
		return nil
	}
	return &inlineKeyboard{InlineKeyboard: [][]inlineButton{row}}
}

// actionKeyboard is the first stage: the three things a user can do about a
// reminder. It is what Back returns to, so its labels match the scheduler's
// descriptors rather than being re-derived from them.
func actionKeyboard(occurrenceID string) *inlineKeyboard {
	return keyboardFor(transport.Outbound{
		SubjectID: occurrenceID,
		Actions: []transport.Action{
			{ID: transport.ActionComplete, Label: "Done"},
			{ID: transport.ActionSnooze, Label: "Snooze"},
			{ID: transport.ActionSkip, Label: "Skip"},
		},
	})
}

// snoozeKeyboard is the second stage: R9's four presets on one row and a way
// back on the next.
//
// The set comes from schedule.Deltas and the labels from Delta.Label, so the
// keyboard cannot offer a delta the resolver does not know or name one
// differently from another surface.
func snoozeKeyboard(occurrenceID string) *inlineKeyboard {
	presets := make([]inlineButton, 0, len(schedule.Deltas))
	for _, d := range schedule.Deltas {
		data := encodeCallback(actionSnooze, occurrenceID, string(d))
		if data == "" {
			continue
		}
		presets = append(presets, inlineButton{Text: d.Label(), CallbackData: data})
	}

	back := []inlineButton{{
		Text:         "‹ Back",
		CallbackData: encodeCallback(actionBack, occurrenceID, ""),
	}}

	return &inlineKeyboard{InlineKeyboard: [][]inlineButton{presets, back}}
}

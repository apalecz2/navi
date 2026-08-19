// Package transport is the shared vocabulary for reaching the user and being
// reached by them: one interface, two roles. A notification transport carries
// reminders out; a conversation transport carries messages both ways. Both are
// named by environment variable and can point at different adapters, and today
// both point at the same one (D-006).
//
// This package is a leaf and must stay one. It imports the standard library and
// nothing else — not internal/domain, not internal/model, not internal/store.
// The scheduler imports this package, so everything reachable from here is
// reachable from the firing path, and invariant 1 stops being a compile-time
// property the moment that set grows. An adapter may import whatever it needs;
// the vocabulary may not.
//
// Callers branch on Capabilities, never on Name (D-007). There is one adapter
// serving both roles right now, which is exactly why the rule is worth keeping:
// code written against the flags already works the day a push transport lands,
// and code written against "is this Telegram" has to be found first.
package transport

import (
	"context"
	"time"
)

// Transport is one adapter: an outbound half, an inbound half, and an honest
// description of what it can do.
type Transport interface {
	Name() string
	Capabilities() Capabilities

	// Send returns the transport's own message id.
	Send(ctx context.Context, msg Outbound) (externalID string, err error)

	// Receive streams inbound messages until ctx is cancelled. Transports that
	// poll and transports that are fed by a webhook both satisfy this; the
	// difference is confined to the adapter.
	Receive(ctx context.Context) (<-chan IncomingMessage, error)
}

// Outbound is one message to deliver.
//
// A struct rather than a parameter list because Go has no default arguments and
// most calls set two of the five fields. The zero values are the defaults.
type Outbound struct {
	// Recipient names who to deliver to. Empty means the adapter's own
	// configured default recipient, which is the only case that exists today:
	// this is a single-user system (S1) with no user table, so the scheduler has
	// no recipient to name and leaves this blank.
	Recipient string

	Body string

	// Actions are what the user can do about this message, described and not
	// rendered. How a tap gets home is entirely the adapter's business.
	Actions []Action

	// SubjectID names the thing the Actions act on. Empty means this message has
	// nothing resolvable behind it, and an adapter renders no actions in that
	// case even when Actions is non-empty — which is what keeps a conversational
	// reply keyboard-free without anyone branching on why it was sent.
	//
	// An id has to travel somehow: Action.Arg is spoken for by the snooze delta
	// and ThreadRef means a thread, so neither can carry it. What the id refers
	// to is not this package's business. Today the scheduler puts an occurrence
	// id here and Telegram packs it into callback_data; a transport that can only
	// carry a URL would sign it into one instead (T10).
	SubjectID string

	Priority Priority

	// ThreadRef ties this message to an existing conversation where the
	// transport has a notion of one.
	ThreadRef string
}

// Capabilities is what an adapter can actually do. Every flag is answered
// honestly, including the ones that make the caller do more work: a transport
// that overstates itself produces a message the user cannot act on, which is
// worse than a plainer one.
type Capabilities struct {
	// SupportsActions reports whether an Action can be rendered as something
	// tappable that gets back to this service. A transport that cannot renders a
	// numbered plain-text list instead.
	SupportsActions bool

	// SupportsNativeNotificationActions reports whether those actions appear on
	// the lock screen rather than inside a chat message. Nothing sets this true
	// until T10, which may never be built — it is recorded here so the code that
	// would have to change is already the code reading the flag.
	SupportsNativeNotificationActions bool

	SupportsRichText bool

	// SupportsMessageEditing reports whether a message this adapter has already
	// sent can be rewritten in place. N6 asks for the outcome of a tap to be
	// folded into the original message rather than pushed after it, so a
	// resolved reminder leaves one message in the chat instead of two; a
	// transport that cannot sends the short confirmation instead. That fallback
	// is a capability question rather than a name question (T5), which is why it
	// is a flag here and not an if in the adapter.
	SupportsMessageEditing bool

	// MaxBodyLength is a limit in runes, zero meaning unlimited. Runes because
	// this is the portable unit and the shared vocabulary has no business
	// knowing that Telegram counts UTF-16 code units; an adapter with a limit in
	// its own units applies that itself, on top of whatever the caller did.
	MaxBodyLength int
}

// Action names what it does and nothing about how it travels.
//
// An earlier draft carried URL, Method, and Headers, which pushed one
// transport's delivery mechanism into the shared vocabulary and made every
// caller responsible for signing. Telegram packs the action id and the
// occurrence id into callback_data and receives the tap on its own webhook; a
// push transport that can only carry a URL builds a signed one instead. Neither
// is visible from here.
type Action struct {
	ID    string // "complete" | "snooze" | "skip"
	Label string
	Arg   string // optional, e.g. a snooze delta
}

// Action ids. They are the vocabulary both halves agree on: the scheduler
// attaches them, the adapter renders them, and P2's callback handler parses
// them back.
const (
	ActionComplete = "complete"
	ActionSnooze   = "snooze"
	ActionSkip     = "skip"
)

// IncomingMessage is an inbound message normalized out of whatever shape the
// adapter received it in. Nothing consumes this until P1.
type IncomingMessage struct {
	SenderID   string
	Text       string
	Transport  string
	ExternalID string
	ReplyTo    string
	ReceivedAt time.Time
}

// Priority is how loudly to deliver, mirroring items.priority: 1 is quietest, 5
// is loudest. An adapter maps the range onto the strongest signal it offers,
// which on Telegram is silent versus normal delivery (N5).
type Priority int

// PriorityUnspecified is the zero value, meaning the adapter's own default. It
// is deliberately not 1: items.priority is CHECK (priority BETWEEN 1 AND 5)
// with a default of 3, so zero is not a priority any item can hold and no
// adapter should have a rule for it beyond "use the default".
const PriorityUnspecified Priority = 0

// Truncate cuts body to at most max runes, appending an ellipsis when it cuts.
// A max of zero or a body already short enough is returned unchanged.
//
// Rune-wise rather than byte-wise because slicing a Go string at a byte offset
// splits a multi-byte rune and produces invalid UTF-8, which is a corrupted
// message rather than a shortened one.
func Truncate(body string, max int) string {
	if max <= 0 {
		return body
	}
	runes := []rune(body)
	if len(runes) <= max {
		return body
	}
	const ellipsis = "…"
	if max == 1 {
		return ellipsis
	}
	return string(runes[:max-1]) + ellipsis
}

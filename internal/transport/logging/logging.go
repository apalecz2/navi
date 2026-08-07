// Package logging is a notification transport that delivers to the log.
//
// It exists so the fire path could be built and exercised before any real
// adapter did, which is the only test D-007 gets at P0: the scheduler is written
// against the interface, and session 6 swaps in Telegram without touching it. It
// stays in the tree afterwards as the development default — NOTIFY_TRANSPORT=
// logging runs the whole system with no bot token and no network.
package logging

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/aidenpaleczny/navi/internal/transport"
)

// Name is this adapter's identity in NOTIFY_TRANSPORT and in the log.
const Name = "logging"

// Transport writes what it would have sent to slog.
type Transport struct {
	log *slog.Logger
}

// New returns a logging transport.
func New(log *slog.Logger) *Transport { return &Transport{log: log} }

// Name identifies the adapter. Nothing branches on it (D-007); it is here for
// the interface, the log line, and the startup warning.
func (t *Transport) Name() string { return Name }

// Capabilities answers honestly, which for this adapter means answering no four
// times.
//
// SupportsActions is false because nothing can tap a log line. The adapter still
// prints the descriptors, as a numbered plain-text list — that is not a
// consolation prize, it is precisely the rendering the capability model
// prescribes for a transport without action support, so the false branch of
// D-007 is exercised by the only notification adapter that exists.
func (t *Transport) Capabilities() transport.Capabilities {
	return transport.Capabilities{
		SupportsActions:                   false,
		SupportsNativeNotificationActions: false,
		SupportsRichText:                  false,
		MaxBodyLength:                     0, // unlimited; a log line has no limit worth enforcing
	}
}

// Send records the message and returns a synthetic external id.
//
// The id is not a real message identifier because there is no real message.
// Returning one anyway keeps the contract honest for a caller that stores it,
// and makes an accidental dependency on this adapter visible in whatever it was
// stored into.
func (t *Transport) Send(ctx context.Context, msg transport.Outbound) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", fmt.Errorf("logging: send: %w", err)
	}

	t.log.Info("notification (not delivered: logging transport)",
		"recipient", recipient(msg.Recipient),
		"priority", int(msg.Priority),
		"body", msg.Body,
		"actions", renderActions(msg.Actions),
	)
	return "log", nil
}

// Receive returns a channel nothing is ever written to, closed when ctx is
// cancelled. Messages do not arrive by log file, and pretending otherwise would
// give P1 an inbound half that silently never fires.
func (t *Transport) Receive(ctx context.Context) (<-chan transport.IncomingMessage, error) {
	ch := make(chan transport.IncomingMessage)
	go func() {
		<-ctx.Done()
		close(ch)
	}()
	return ch, nil
}

func recipient(r string) string {
	if r == "" {
		return "(adapter default)"
	}
	return r
}

// renderActions is the no-action-support fallback: a numbered plain-text list.
func renderActions(actions []transport.Action) string {
	if len(actions) == 0 {
		return ""
	}
	var b strings.Builder
	for i, a := range actions {
		if i > 0 {
			b.WriteString("  ")
		}
		fmt.Fprintf(&b, "%d) %s", i+1, a.Label)
		if a.Arg != "" {
			fmt.Fprintf(&b, " [%s]", a.Arg)
		}
	}
	return b.String()
}

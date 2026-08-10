package conversation

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/aidenpaleczny/navi/internal/supervisor"
	"github.com/aidenpaleczny/navi/internal/transport"
)

// Name identifies this loop in health, metrics, and log lines.
const Name = "conversation"

// tickInterval is a design fact, not a deployment knob (CLAUDE.md: "loop
// intervals are constants in the loop packages, not environment
// variables"). Draining an empty channel is nearly free, so 2s adds
// negligible latency to a reply while keeping the loop cheap when idle.
const tickInterval = 2 * time.Second

// intakeBufferSize bounds how many accepted-but-unprocessed messages can
// queue up. Single-user, one turn processed at a time: this is not a
// throughput knob, it is the backstop against an unbounded queue if the
// model is slow for a stretch. A full buffer means Enqueue returns false and
// the webhook counts the drop; the message itself is already durably in
// conversations by the time Enqueue is even called.
const intakeBufferSize = 32

// Intake is the bounded, supervisor-driven consumer of accepted inbound
// messages - the seam that keeps a slow model call from ever needing its
// own goroutine with its own recover(), which CLAUDE.md's two-recover rule
// (supervisor.tickOnce, the httpapi middleware, nowhere else) forbids.
// telegram.Inbound.Enqueue's target; satisfies telegram.Dispatcher
// structurally.
type Intake struct {
	ch     chan transport.IncomingMessage
	ladder *Ladder
}

// NewIntake returns an Intake with an empty, bounded queue.
func NewIntake(ladder *Ladder) *Intake {
	return &Intake{ch: make(chan transport.IncomingMessage, intakeBufferSize), ladder: ladder}
}

// Enqueue is non-blocking. It returns false when the buffer is full, which
// the caller (the webhook) counts via navi_inbound_messages_dropped_total
// {reason="queue_full"} - no new metric series needed.
func (in *Intake) Enqueue(msg transport.IncomingMessage) bool {
	select {
	case in.ch <- msg:
		return true
	default:
		return false
	}
}

// Loop returns the supervisor.Loop wrapper - registered in main exactly
// like the other five loops, which is what gives Tick panic recovery and a
// /healthz entry for free.
func (in *Intake) Loop() supervisor.Loop {
	return supervisor.Loop{Name: Name, Interval: tickInterval, Tick: in.Tick}
}

// Tick drains every currently-queued message this call, sequentially - a
// single-user system has no reason to process more than one turn at a
// time, and draining fully rather than one-per-tick keeps a burst of
// messages from accumulating latency across ticks. It continues past a
// per-message error so one bad turn cannot starve the rest of the queue,
// and joins every error into one (the same pattern internal/scheduler and
// internal/store already use) so the supervisor logs it once with the loop
// name attached, per CLAUDE.md's "loop bodies return errors and never log
// them."
func (in *Intake) Tick(ctx context.Context) error {
	var errs []error
	for {
		select {
		case msg := <-in.ch:
			if err := in.ladder.Handle(ctx, msg); err != nil {
				errs = append(errs, fmt.Errorf("conversation: handle message from %s: %w", msg.SenderID, err))
			}
		case <-ctx.Done():
			return errors.Join(errs...)
		default:
			return errors.Join(errs...)
		}
	}
}

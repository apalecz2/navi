package briefing

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/aidenpaleczny/navi/internal/domain"
	"github.com/aidenpaleczny/navi/internal/schedule"
)

// Evaluation is what one EvaluateResponse pass did, for the log line and for
// naviseed.
type Evaluation struct {
	Date       string
	Answered   bool
	Unanswered bool
	Paused     bool
}

// LogValue renders the evaluation as one group.
func (e Evaluation) LogValue() slog.Value {
	return slog.GroupValue(
		slog.String("date", e.Date),
		slog.Bool("answered", e.Answered),
		slog.Bool("unanswered", e.Unanswered),
	)
}

// EvaluateResponse is the reply-tracking half (O8). It mirrors the reconciler's
// grace pass: a deterministic window, then a conclusion — but the conclusion
// here is only a record, because a briefing has no occurrence to transition.
// Nothing in this function writes missed, and nothing should.
//
// A reply is recognised the same way a reconciliation reply is: not by a
// classifier and not by a separate route, but by an inbound message existing
// after the briefing went out. The briefing asks an open question, so any
// message counts and it is never read. Clearing the marker is the only "it was
// answered" state there is.
//
// A global pause suppresses it, the same way it suppresses the reconciler's
// grace pass: vacation mode is a reason not to conclude anything today, and the
// marker is still there to evaluate when the pause lifts.
func (b *Briefing) EvaluateResponse(ctx context.Context, now time.Time) (Evaluation, error) {
	pausedUntil, paused, err := b.store.GlobalPauseUntil(ctx)
	if err != nil {
		return Evaluation{}, err
	}
	if paused && pausedUntil.After(now) {
		return Evaluation{Paused: true}, nil
	}

	date, sentAt, ok, err := b.store.BriefingAwaiting(ctx)
	if err != nil {
		return Evaluation{}, err
	}
	if !ok {
		return Evaluation{}, nil
	}

	answered, err := b.store.HasInboundSince(ctx, sentAt)
	if err != nil {
		return Evaluation{}, err
	}
	if answered {
		if err := b.store.ClearBriefingAwaiting(ctx); err != nil {
			return Evaluation{Date: date}, err
		}
		return Evaluation{Date: date, Answered: true}, nil
	}

	zones, err := schedule.LoadZones(ctx, b.store, b.defaultTZ)
	if err != nil {
		return Evaluation{}, fmt.Errorf("briefing: resolve zones: %w", err)
	}
	// The end of the person's local day — the same deadline K7 gives an
	// occurrence a check-in asked about with no per-item grace of its own. A
	// reply landing exactly at the deadline still counts, because the answered
	// check above runs first.
	deadline := domain.GraceDeadline(sentAt, nil, zones.Local())
	if now.Before(deadline) {
		return Evaluation{Date: date}, nil // still inside the window
	}

	b.metrics.IncBriefingUnanswered()
	if err := b.store.ClearBriefingAwaiting(ctx); err != nil {
		return Evaluation{Date: date}, err
	}
	return Evaluation{Date: date, Unanswered: true}, nil
}

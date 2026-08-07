// Package scheduler is the fire path. It polls occurrence rows, claims the ones
// that are due with BEGIN IMMEDIATE, and sends them.
//
// There is no model call in this package and there never will be one. A reminder
// fires because a SQLite row says it is due; every model contribution is
// generated in advance, stored on the row, and degrades to the plain item title
// when absent (D-001, N3). The import block of this package is where that
// invariant is enforced.
//
// # Claim, then send
//
// The claim commits before the first byte leaves. Holding the write lock across
// a network call would block the one writer connection for as long as the
// transport takes, which on a hung request is the whole system.
//
// That ordering has two consequences worth naming, because they pull in opposite
// directions and someone will eventually try to "fix" one of them:
//
//   - At most once across a crash. If the process dies between the commit and
//     the send, the row reads notified and nothing was sent. Nothing detects it —
//     the row is no longer pending, so navi_pending_overdue cannot see it — and
//     P3's reconciliation is the backstop, which is what reconciliation is for.
//   - At least once across a send timeout. If the transport delivered and the
//     response was lost, the claim is released and the next tick sends again.
//
// Duplicating a reminder is a mild annoyance; the alternative ordering, sending
// before claiming, duplicates on every restart and gives two overlapping ticks
// nothing to serialize on. The claim goes first.
//
// # Nothing here assigns missed
//
// Not a status, not a side effect, not a special case. missed means
// reconciliation asked and got no answer, and reconciliation does not exist until
// P3 (K6, D-008). A row this loop cannot fire stays pending.
package scheduler

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/aidenpaleczny/navi/internal/domain"
	"github.com/aidenpaleczny/navi/internal/store"
	"github.com/aidenpaleczny/navi/internal/supervisor"
	"github.com/aidenpaleczny/navi/internal/transport"
)

// Name is the loop's identity in logs, /healthz, and the metric labels.
const Name = "scheduler"

// Interval is the poll interval. Q1 allows a minute between the scheduled time
// and delivery; polling at 30s spends half of that budget and leaves the rest
// for the transport.
const Interval = 30 * time.Second

// RecoveryWindow is how much backlog a restart is allowed to deliver (C9, Q-2).
// It is anchored to process start, not to now — see ClaimFloor, where the
// distinction is the whole design.
//
// Fifteen minutes rather than the thirty the requirement was drafted around,
// because reconciliation is the safety net: too long and a two-hour outage dumps
// a batch of stale notifications, too short and a five-minute restart swallows a
// reminder, and the existence of a backstop argues for the shorter figure.
const RecoveryWindow = 15 * time.Minute

// MaxBatch bounds one claim. The claim holds the single writer connection, so it
// has to be bounded by something; the remainder is picked up 30 seconds later,
// and 200 rows a tick clears any backlog the recovery window can hold long
// before it ages out.
const MaxBatch = 200

// sendTimeout bounds one delivery. A transport that hangs must cost one
// reminder's tick, not the loop.
const sendTimeout = 10 * time.Second

// releaseTimeout bounds the rollback of unsent claims. It runs on a detached
// context — the release is most needed exactly when the tick's own context has
// been cancelled — so it needs a deadline of its own.
const releaseTimeout = 5 * time.Second

// Store is the narrow view this loop needs.
type Store interface {
	ClaimDue(ctx context.Context, upper, floor time.Time, limit int) (time.Time, []store.Due, error)
	ReleaseClaims(ctx context.Context, ids []string, claimedAt time.Time) (int, error)
	GlobalPauseUntil(ctx context.Context) (time.Time, bool, error)
}

// Notifier is the outbound half of a transport and nothing else.
//
// Deliberately not transport.Transport: omitting Receive makes it a compile-time
// fact that the firing path cannot read messages, in the same way that this
// package's import block makes it a compile-time fact that it cannot reach a
// model. Same shape as sweeper.Store and httpapi.Store.
type Notifier interface {
	Name() string
	Capabilities() transport.Capabilities
	Send(ctx context.Context, msg transport.Outbound) (externalID string, err error)
}

// Metrics is the fire path's slice of the registry.
type Metrics interface {
	ObserveDeliveryLatency(seconds float64)
	IncTransition(from, to, source string)
	IncClaimReleased()
	IncCopywriterFallback()
}

// Source is this loop's label in navi_occurrence_transitions_total. It is not a
// domain.ResolutionSource: the scheduler notifies, it never resolves.
const Source = "scheduler"

// Scheduler claims due occurrences and sends notifications.
type Scheduler struct {
	log      *slog.Logger
	store    Store
	notifier Notifier
	metrics  Metrics

	// startedAt anchors the claim floor. Read ClaimFloor before changing it.
	startedAt time.Time
}

// New returns a scheduler. now is the instant the recovery window is measured
// back from, which in production is process start.
func New(log *slog.Logger, st Store, n Notifier, m Metrics, now time.Time) *Scheduler {
	return &Scheduler{log: log, store: st, notifier: n, metrics: m, startedAt: now}
}

// Loop describes this loop to the supervisor.
func (s *Scheduler) Loop() supervisor.Loop {
	return supervisor.Loop{Name: Name, Interval: Interval, Tick: s.Tick}
}

// ClaimFloor is the oldest start time this process will ever fire. It is fixed
// at construction and never moves.
//
// One constant was being asked two different questions, and they only give the
// same answer before the scheduler has tried a row:
//
//   - "How much backlog piled up while we were not looking?" is C9's question,
//     and the boot anchor answers it. A restart fires what came due in the last
//     fifteen minutes and leaves an older backlog alone, so a container that has
//     been down since yesterday does not open with a wall of stale reminders.
//   - "Is this row still worth retrying?" is the failure-mode table's question,
//     and the answer is yes for as long as this process lives. A send that fails
//     releases its row, and a floor that slid with now would drop that row for
//     good after thirty ticks of an unreachable transport — silently, with
//     nothing to mark it missed until P3.
//
// The cost is the mirror image: a long outage inside a live process delivers a
// long backlog when the transport comes back. That state is loud — the error
// counter, the tick histogram, and navi_pending_overdue all show it — and the
// alternative is a silent loss, which is the trade this design makes everywhere
// else too.
func (s *Scheduler) ClaimFloor() time.Time { return s.startedAt.Add(-RecoveryWindow) }

// Result is what one pass did, for the loop's log line and for naviseed to
// assert against. It exists for the same reason materializer.Result does:
// reaching into the metrics registry to find out what happened is worse than
// being told.
type Result struct {
	Claimed  int
	Sent     int
	Failed   int
	Released int

	// Fallbacks counts sends that went out as the plain item title (N3). At P0
	// no copywriter exists, so this equals Sent.
	Fallbacks int

	// MaxLatency is the largest scheduled-time-to-sent gap in this pass.
	MaxLatency time.Duration

	// Paused reports that the pass did nothing because vacation mode is on.
	Paused bool
}

// LogValue renders the result as one group rather than eight top-level keys.
func (r Result) LogValue() slog.Value {
	return slog.GroupValue(
		slog.Int("claimed", r.Claimed),
		slog.Int("sent", r.Sent),
		slog.Int("failed", r.Failed),
		slog.Int("released", r.Released),
		slog.Int("fallbacks", r.Fallbacks),
		slog.Duration("max_latency", r.MaxLatency),
	)
}

// Tick is the loop body: one pass, then a line if it did anything.
func (s *Scheduler) Tick(ctx context.Context) error {
	res, err := s.Fire(ctx)
	if res.Sent > 0 || res.Failed > 0 || res.Released > 0 {
		s.log.Info("notifications sent", "result", res)
	}
	return err
}

// Fire claims what is due and sends it. It is the synchronous entry point, so
// that naviseed and later sessions can drive one pass without a ticker, the way
// materializer.Item does for the expansion path.
//
// The return values are named because the release of unsent claims is deferred,
// and a deferred function can only report what it did through a named result.
func (s *Scheduler) Fire(ctx context.Context) (res Result, err error) {

	// Vacation mode. Rows stay pending and untouched; nothing here decides they
	// were missed.
	//
	// Read outside the claim transaction, which makes it a time-of-check race: a
	// pause set between this read and the commit lets one batch through. That is
	// bounded by a single tick and by a user who has just said they are away, and
	// closing it would mean either a second query inside the hot transaction or a
	// kv read the claim does not otherwise need.
	pausedUntil, paused, err := s.store.GlobalPauseUntil(ctx)
	if err != nil {
		return res, err
	}
	now := time.Now()
	if paused && pausedUntil.After(now) {
		res.Paused = true
		return res, nil
	}

	claimedAt, due, err := s.store.ClaimDue(ctx, now, s.ClaimFloor(), MaxBatch)
	if err != nil {
		return res, err
	}
	res.Claimed = len(due)
	for range due {
		s.metrics.IncTransition(string(domain.StatusPending), string(domain.StatusNotified), Source)
	}
	if len(due) == 0 {
		return res, nil
	}

	// unsent starts as everything claimed and shrinks as sends succeed. The
	// deferred release is the only rollback path, which is what makes it cover
	// all four ways a claim can be stranded: a failed send, a cancelled context
	// mid-batch (D12), a panic that supervisor.tickOnce recovers, and an early
	// return. A per-failure release would handle the first and quietly miss the
	// other three.
	unsent := make([]string, 0, len(due))
	for _, d := range due {
		unsent = append(unsent, d.ID)
	}
	defer func() {
		res.Released = s.release(ctx, unsent, claimedAt)
	}()

	var failures []error
	for _, d := range due {
		if err := ctx.Err(); err != nil {
			// Shut down mid-batch. Everything still in unsent goes back to
			// pending on the way out, and the next process picks it up inside
			// its own recovery window.
			failures = append(failures, fmt.Errorf("scheduler: send cancelled with %d claimed and unsent: %w",
				len(unsent), err))
			break
		}

		latency, fellBack, err := s.send(ctx, d)
		if err != nil {
			// Logged here rather than returned alone, on the materializer's
			// precedent: the loop contract says a body does not log its own
			// errors, and it means the single error the supervisor prints. One
			// unreachable recipient must not hide the other nineteen sends, so
			// the per-send detail is logged and the count is returned.
			s.log.Error("send failed", "occurrence", d.ID, "item", d.ItemID, "err", err)
			res.Failed++
			failures = append(failures, err)
			continue
		}

		unsent = removeID(unsent, d.ID)
		res.Sent++
		if fellBack {
			res.Fallbacks++
			s.metrics.IncCopywriterFallback()
		}
		if latency > res.MaxLatency {
			res.MaxLatency = latency
		}
		s.metrics.ObserveDeliveryLatency(latency.Seconds())
	}

	if len(failures) > 0 {
		return res, fmt.Errorf("scheduler: %d of %d sends failed: %w",
			len(failures), len(due), errors.Join(failures...))
	}
	return res, nil
}

// send delivers one occurrence and reports how late it was and whether it went
// out as the plain title.
func (s *Scheduler) send(ctx context.Context, d store.Due) (time.Duration, bool, error) {
	caps := s.notifier.Capabilities()

	body := domain.NotificationBody(d.MessageText, d.Title)
	fellBack := body == d.Title

	msg := transport.Outbound{
		// Recipient is left empty: this is a single-user system with no user
		// table, so the adapter's configured default is the only recipient there
		// is.
		Body:     transport.Truncate(body, caps.MaxBodyLength),
		Actions:  actions(),
		Priority: transport.Priority(d.Priority),
	}

	sendCtx, cancel := context.WithTimeout(ctx, sendTimeout)
	defer cancel()

	// The transport's own message id is discarded. There is no column for it on
	// occurrences and P2 does not need one: a Telegram callback query carries the
	// message it came from, so editing the notification after a tap needs nothing
	// stored here. A session that wants it should add the column first.
	if _, err := s.notifier.Send(sendCtx, msg); err != nil {
		return 0, fellBack, fmt.Errorf("scheduler: send occurrence %s: %w", d.ID, err)
	}

	return time.Since(d.StartsAt), fellBack, nil
}

// release returns unsent claims to pending.
//
// It runs on a context detached from the tick's, because the case that most
// needs it is the one where the tick's context has just been cancelled. Its
// failure is logged and not returned: the caller is a deferred function on the
// way out of a pass that has already decided its own error, and a stranded claim
// is P3's to find either way.
func (s *Scheduler) release(ctx context.Context, ids []string, claimedAt time.Time) int {
	if len(ids) == 0 {
		return 0
	}

	releaseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), releaseTimeout)
	defer cancel()

	n, err := s.store.ReleaseClaims(releaseCtx, ids, claimedAt)
	if err != nil {
		s.log.Error("release claims", "count", len(ids), "err", err)
		return 0
	}
	for i := 0; i < n; i++ {
		s.metrics.IncClaimReleased()
	}
	if n != len(ids) {
		// A row moved between the claim and the release, which at P0 means
		// something outside this loop wrote to it.
		s.log.Warn("release claims: partial", "asked", len(ids), "released", n)
	}
	return n
}

// actions are the three things a user can do about a reminder, described and not
// rendered (N4). What a tap travels over is the adapter's business, and what
// happens when one arrives is P2's.
func actions() []transport.Action {
	return []transport.Action{
		{ID: transport.ActionComplete, Label: "Done"},
		{ID: transport.ActionSnooze, Label: "Snooze"},
		{ID: transport.ActionSkip, Label: "Skip"},
	}
}

func removeID(ids []string, id string) []string {
	for i, v := range ids {
		if v == id {
			return append(ids[:i], ids[i+1:]...)
		}
	}
	return ids
}

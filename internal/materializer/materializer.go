// Package materializer expands item schedules into occurrence rows ahead of
// time (D-003). Randomness resolves here rather than at fire time, which is what
// makes a windowed or fuzzy reminder an ordinary scheduled one everywhere
// downstream (D-005).
//
// Three invariants govern everything in this package
// (docs/05-schedule-spec.md#materialization):
//
//   - Only pending, non-override, future rows are ever touched. Everything else
//     is history or a deliberate exception.
//   - A row with is_override = 1 survives untouched. That is the whole mechanism
//     behind "skip tomorrow's" and behind snooze children.
//   - Running it twice produces the same set, because a slot that already exists
//     is kept rather than redrawn.
//
// The first two are enforced in SQL, by the guard on the store's delete. The
// third is this package's, and it is what the slot predicates in expand.go are
// for.
package materializer

import (
	"context"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"time"

	"github.com/aidenpaleczny/navi/internal/domain"
	"github.com/aidenpaleczny/navi/internal/schedule"
	"github.com/aidenpaleczny/navi/internal/store"
	"github.com/aidenpaleczny/navi/internal/supervisor"
)

// Name is the loop's identity in logs, /healthz, and the metric labels.
const Name = "materializer"

// Interval is how often the loop wakes, not how often it does work. The loop
// table calls this one nightly, and the nightly gate belongs in the tick body:
// a bare 24h ticker would report a stale last_tick within minutes of every
// restart and would contribute one sample a day to the tick-interval series,
// which is not enough to see the loop slowing down before it stops.
const Interval = 60 * time.Second

// HorizonDays is how far ahead occurrences are materialized (I3). The hourly
// sweeper re-runs materialization when the real horizon falls under 25 days.
const HorizonDays = 30

// Materializer expands schedules into occurrences.
type Materializer struct {
	log       *slog.Logger
	store     *store.Store
	defaultTZ *time.Location

	// newRand hands each run its own generator.
	//
	// Per run rather than per process, because a run is one goroutine and a
	// per-run generator therefore needs no lock, while the nightly loop and P1's
	// synchronous re-materialization on a schedule change can overlap and would
	// otherwise share one. Per run rather than per item, because a generator
	// seeded from a clock would hand two items materialized in the same
	// millisecond the same times.
	//
	// It is a field so Q-14's tests can seed it and get the same placement
	// twice. Nothing else replaces it.
	newRand func() *rand.Rand

	// lastNightly is the local date of the last completed nightly run, in
	// memory. A restart clears it, which materializes once at boot — which is
	// what a restart should do anyway, since nothing else knows how long the
	// process was down. Only Tick reads or writes it, and only from the
	// supervisor's goroutine.
	lastNightly string
}

// New returns a materializer. defaultTZ is the deployment default, the last
// rung of the ladder in schedule.Zones.
func New(log *slog.Logger, st *store.Store, defaultTZ *time.Location) *Materializer {
	return &Materializer{
		log:       log,
		store:     st,
		defaultTZ: defaultTZ,
		newRand: func() *rand.Rand {
			return rand.New(rand.NewPCG(rand.Uint64(), rand.Uint64()))
		},
	}
}

// Loop describes this loop to the supervisor.
func (m *Materializer) Loop() supervisor.Loop {
	return supervisor.Loop{Name: Name, Interval: Interval, Tick: m.Tick}
}

// Result is what a run did, for the log line and for naviseed to assert
// idempotence against.
type Result struct {
	Items   int
	Failed  int
	Applied store.Applied
	Through time.Time
}

// LogValue renders a result for the run's one log line.
func (r Result) LogValue() slog.Value {
	return slog.GroupValue(
		slog.Int("items", r.Items),
		slog.Int("failed", r.Failed),
		slog.Int("inserted", r.Applied.Inserted),
		slog.Int("deleted", r.Applied.Deleted),
		slog.Int("kept", r.Applied.Kept),
	)
}

// run is the state one materialization pass resolves once and then holds fixed:
// the clock, the horizon, which zone each item's wall clocks mean, and where the
// global pause ends.
//
// Reading them once is not only cheaper. A run that re-read kv.current_tz
// halfway through would place half its occurrences in one zone and half in
// another, and a run that re-read the clock would have a horizon that drifts by
// the length of the run.
type run struct {
	now   time.Time
	to    time.Time
	zones schedule.Zones

	// pause is the end of the global pause window, zero when there is none.
	pause time.Time
}

// covers reports whether an instant is inside this run's horizon and still
// ahead of it.
func (r run) covers(at time.Time) bool {
	return at.After(r.now) && !at.After(r.to)
}

// paused reports whether an instant falls inside either pause window (I6). Both
// levels are checked the same way, because "I'm away until Monday" and "pause
// this one item" mean the same thing to an occurrence that would have landed in
// the middle of it.
func (r run) paused(item domain.Item, at time.Time) bool {
	return at.Before(r.pause) || item.IsPaused(at)
}

// Tick runs the nightly materialization once per local day.
//
// The gate is here rather than in the ticker for the reason Interval records: a
// loop that wakes once a day is a loop nothing can tell has stopped.
func (m *Materializer) Tick(ctx context.Context) error {
	today := time.Now().In(m.defaultTZ).Format(domain.DateLayout)
	if today == m.lastNightly {
		return nil
	}

	res, err := m.All(ctx)
	if err != nil {
		return err
	}

	m.lastNightly = today
	m.log.Info("nightly materialization", "date", today, "result", res)
	return nil
}

// All materializes every active item. It is the nightly run and the sweeper's
// horizon backfill, and it is the only entry point that moves the horizon.
func (m *Materializer) All(ctx context.Context) (Result, error) {
	r, err := m.begin(ctx)
	if err != nil {
		return Result{}, err
	}

	items, err := m.store.ListActiveItems(ctx)
	if err != nil {
		return Result{}, fmt.Errorf("materializer: list active items: %w", err)
	}

	res := Result{Through: r.to}
	for _, item := range items {
		applied, err := m.one(ctx, r, item)
		if err != nil {
			// Logged here rather than returned, against the usual rule that loop
			// bodies do not log, because the run continues past it: one item
			// with an unparseable schedule must not cost every other item its
			// night. What reaches the supervisor is the count.
			res.Failed++
			m.log.Error("materialize item", "item", item.ID, "title", item.Title, "err", err)
			continue
		}
		res.Items++
		res.Applied.Inserted += applied.Inserted
		res.Applied.Deleted += applied.Deleted
		res.Applied.Kept += applied.Kept
	}

	if res.Failed > 0 {
		return res, fmt.Errorf("materializer: %d of %d items failed", res.Failed, len(items))
	}

	// Written last and only on a clean run. The horizon is a claim about how far
	// ahead occurrences exist, and a run that skipped an item has not earned it —
	// better that /healthz shows the horizon decaying until the next run than
	// that it reports 30 days over a gap.
	if err := m.store.SetLastMaterializedThrough(ctx, r.to); err != nil {
		return res, err
	}
	return res, nil
}

// Item materializes one item. P1 calls it synchronously when a schedule changes,
// so the confirmation can name real timestamps rather than promise them.
//
// It does not move the horizon: one item says nothing about how far ahead the
// rest of them reach.
func (m *Materializer) Item(ctx context.Context, id string) (Result, error) {
	r, err := m.begin(ctx)
	if err != nil {
		return Result{}, err
	}

	item, err := m.store.GetItem(ctx, id)
	if err != nil {
		return Result{}, err
	}

	applied, err := m.one(ctx, r, item)
	if err != nil {
		return Result{}, err
	}
	return Result{Items: 1, Applied: applied, Through: r.to}, nil
}

// begin resolves everything a run holds fixed.
func (m *Materializer) begin(ctx context.Context) (run, error) {
	now := time.Now().Truncate(time.Second)
	r := run{
		now:   now,
		to:    now.AddDate(0, 0, HorizonDays),
		zones: schedule.Zones{Fallback: m.defaultTZ},
	}

	name, ok, err := m.store.CurrentTZ(ctx)
	if err != nil {
		return run{}, err
	}
	if ok {
		loc, err := schedule.LoadLocation(name)
		if err != nil {
			return run{}, err
		}
		r.zones.Device = loc
	}

	pause, ok, err := m.store.GlobalPauseUntil(ctx)
	if err != nil {
		return run{}, err
	}
	if ok {
		r.pause = pause
	}

	return r, nil
}

// one materializes a single item inside one transaction.
func (m *Materializer) one(ctx context.Context, r run, item domain.Item) (store.Applied, error) {
	loc, err := r.zones.For(item)
	if err != nil {
		return store.Applied{}, err
	}

	rq := request{
		item:     item,
		generate: item.Active && item.ArchivedAt == nil,
		loc:      loc,
		run:      r,
		rnd:      m.newRand(),
		log:      m.log,
	}

	if rq.generate {
		// Parse and not Prepare. The read path decodes the column exactly as it
		// is: a schedule that is missing a field is an item to report and skip,
		// not one to quietly complete from the defaults table on its way out of
		// the database.
		if rq.schedule, err = schedule.Parse(item.Schedule); err != nil {
			return store.Applied{}, err
		}
	}

	return m.store.MaterializeItem(ctx, item, r.now, rq.plan)
}

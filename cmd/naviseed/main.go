// Command naviseed writes one item and a few occurrences by hand, reads them
// back through the repository, exercises the status state machine, and puts the
// schedule machinery through the validation table.
//
// It exists because a test suite is out of scope (S2, D-020) and the parts that
// are expensive to get wrong still have to be verifiable. It is the substitute,
// and it is a deliberately small one: it proves that migrations apply, that a
// timestamp survives the round trip through TEXT, that the transition table
// answers the three ways it is supposed to, and that every row of
// docs/05-schedule-spec.md#validation rejects its case with a message worth
// feeding back to a model. The Dockerfile builds ./cmd/navi only, so this never
// ships in the image.
//
// It is also what gives sessions 4 to 6 rows to work against.
//
//	CONFIG_DIR=./config DEFAULT_TZ=America/Toronto DATA_DIR=./data go run ./cmd/naviseed
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sort"
	"sync"
	"time"

	// Same reason as cmd/navi: the schedule below resolves against an IANA
	// zone, and a scratch image has no /usr/share/zoneinfo.
	_ "time/tzdata"

	"github.com/aidenpaleczny/navi/internal/config"
	"github.com/aidenpaleczny/navi/internal/defaults"
	"github.com/aidenpaleczny/navi/internal/domain"
	"github.com/aidenpaleczny/navi/internal/materializer"
	"github.com/aidenpaleczny/navi/internal/metrics"
	"github.com/aidenpaleczny/navi/internal/schedule"
	"github.com/aidenpaleczny/navi/internal/scheduler"
	"github.com/aidenpaleczny/navi/internal/store"
	"github.com/aidenpaleczny/navi/internal/sweeper"
	"github.com/aidenpaleczny/navi/internal/transport"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: cfg.Log.Level}))
	ctx := context.Background()

	st, err := store.Open(ctx, cfg.Data, log)
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()

	version, err := st.SchemaVersion(ctx)
	if err != nil {
		return err
	}
	fmt.Printf("database %s at schema version %d\n\n", cfg.Data.DBPath(), version)

	item, err := seedItem(ctx, st, cfg.Schedule.DefaultTZ.String())
	if err != nil {
		return err
	}
	if err := seedOccurrences(ctx, st, item); err != nil {
		return err
	}
	if err := reportHealthInputs(ctx, st); err != nil {
		return err
	}
	reportTransitions()

	table, err := defaults.Load(cfg.Files.DefaultsPath())
	if err != nil {
		return err
	}
	if err := schedule.CheckTable(table); err != nil {
		return fmt.Errorf("naviseed: %s: %w", cfg.Files.DefaultsPath(), err)
	}
	fmt.Printf("\ndefaults %s  (%s)\n", cfg.Files.DefaultsPath(), table.Summary())

	reportRoundTrips()
	reportValidation(table, cfg.Schedule.DefaultTZ)
	reportResolution(table)
	if err := reportZones(ctx, st, item, cfg.Schedule.DefaultTZ); err != nil {
		return err
	}

	// Last, and after the timezone block, because setting kv.current_tz is what
	// re-materialization is for: floating items resolve against the device zone,
	// and the rows written here are the ones that move when it changes.
	reportDST()
	if err := reportMaterialization(ctx, st, cfg.Schedule.DefaultTZ, log); err != nil {
		return err
	}

	// Last, because it is the only section that sends anything, and because it
	// wants the global pause reportPause left lifted.
	return reportFire(ctx, st, cfg.Schedule.DefaultTZ.String(), log)
}

// seedItem creates the item on first run and reuses it afterwards, so running
// this twice does not accumulate duplicates.
func seedItem(ctx context.Context, st *store.Store, tz string) (domain.Item, error) {
	const title = "take vitamins"

	existing, err := st.ListActiveItems(ctx)
	if err != nil {
		return domain.Item{}, err
	}
	for _, it := range existing {
		if it.Title == title {
			fmt.Printf("item   %s  %-16q  reusing\n", it.ID, it.Title)
			return it, nil
		}
	}

	// Raw JSON, because schedule parsing is session 3's. The column is TEXT
	// either way, so the shape here is the one 05-schedule-spec describes and
	// nothing in this session interprets it.
	schedule := json.RawMessage(`{"kind":"fixed","rrule":"FREQ=DAILY","at":"09:00"}`)

	item, err := st.CreateItem(ctx, domain.NewItem{
		Title:    title,
		Schedule: schedule,
		TZ:       tz,
	})
	if err != nil {
		return domain.Item{}, err
	}
	fmt.Printf("item   %s  %-16q  %s  priority %d  snooze_cap %d  %s\n",
		item.ID, item.Title, item.Kind, item.Priority, item.SnoozeCap, item.TZ)
	return item, nil
}

// seedOccurrences writes three instances and reads each one back, comparing the
// stored timestamp against what went in. That comparison is the point: SQLite
// has no timestamp type, so the round trip is through a TEXT column and a
// layout, and a mismatch here is the bug that would otherwise surface as a
// reminder firing at the wrong hour.
func seedOccurrences(ctx context.Context, st *store.Store, item domain.Item) error {
	existing, err := st.ListOccurrencesForItem(ctx, item.ID)
	if err != nil {
		return err
	}
	if len(existing) > 0 {
		// Truncated because after the first run most of these are materialized
		// rows rather than seeded ones, and thirty of them buries everything
		// below. The materialization section prints the ones worth reading.
		const show = 3

		fmt.Printf("\n%d occurrence(s) already present, not adding more\n", len(existing))
		for i, occ := range existing {
			if i == show {
				fmt.Printf("occ    ... and %d more\n", len(existing)-show)
				break
			}
			fmt.Printf("occ    %s  %s  %s\n", occ.ID, domain.FormatTime(occ.StartsAt), occ.Status)
		}
		return nil
	}

	now := time.Now()
	starts := []time.Time{
		// Overdue on purpose: this is the row that makes pending_overdue
		// report a non-zero number on /healthz.
		now.Add(-1 * time.Hour),
		now.Add(2 * time.Minute),
		now.Add(24 * time.Hour),
	}

	fmt.Println()
	for _, at := range starts {
		created, err := st.CreateOccurrence(ctx, domain.NewOccurrence{
			ItemID:   item.ID,
			StartsAt: at,
		})
		if err != nil {
			return err
		}

		read, err := st.GetOccurrence(ctx, created.ID)
		if err != nil {
			return err
		}

		want := at.UTC().Truncate(time.Second)
		result := "ok"
		if !read.StartsAt.Equal(want) {
			result = fmt.Sprintf("MISMATCH, wrote %s", domain.FormatTime(want))
		}
		fmt.Printf("occ    %s  %s  %-8s  round-trip %s\n",
			read.ID, domain.FormatTime(read.StartsAt), read.Status, result)
	}
	return nil
}

// reportHealthInputs prints the two values /healthz reads from the database, so
// the endpoint's numbers can be checked against their source.
func reportHealthInputs(ctx context.Context, st *store.Store) error {
	// The floor a process that just started would use, which is what /healthz
	// and the gauge both count against.
	overdue, err := st.PendingOverdue(ctx, time.Now().Add(-scheduler.RecoveryWindow))
	if err != nil {
		return err
	}

	through, ok, err := st.LastMaterializedThrough(ctx)
	if err != nil {
		return err
	}
	horizon := "absent (nothing has materialized yet)"
	if ok {
		horizon = domain.FormatTime(through)
	}

	fmt.Printf("\npending_overdue            %d  (grace %s)\n", overdue, store.OverdueGrace)
	fmt.Printf("last_materialized_through  %s\n", horizon)
	return nil
}

// reportTransitions exercises the state machine's three answers. These are the
// cases the HTTP layer maps to 200, 200, and 409 in P2, and the point of
// printing them is that the mapping carries no judgement of its own — every
// decision visible here was made in internal/domain.
func reportTransitions() {
	cases := []struct {
		kind     domain.Kind
		from, to domain.Status
		want     string
	}{
		{domain.KindReminder, domain.StatusPending, domain.StatusNotified, "applied, 200"},
		{domain.KindReminder, domain.StatusNotified, domain.StatusCompleted, "applied, 200"},
		{domain.KindReminder, domain.StatusCompleted, domain.StatusCompleted, "noop, 200"},
		{domain.KindReminder, domain.StatusCompleted, domain.StatusSkipped, "illegal, 409"},
		{domain.KindReminder, domain.StatusSnoozed, domain.StatusSnoozed, "noop, 200"},
		{domain.KindReminder, domain.StatusSnoozed, domain.StatusCompleted, "illegal, 409"},
		{domain.KindReminder, domain.StatusPending, domain.StatusOccurred, "illegal, 409"},
		{domain.KindEvent, domain.StatusPending, domain.StatusOccurred, "applied, 200"},
	}

	fmt.Println("\ntransitions")
	for _, c := range cases {
		outcome, err := domain.Transition(c.kind, c.from, c.to)
		detail := ""
		var te *domain.TransitionError
		if errors.As(err, &te) {
			detail = "  " + te.Message
		}
		fmt.Printf("  %-8s %-9s -> %-9s %-8s expected %-12s%s\n",
			c.kind, c.from, c.to, outcome, c.want, detail)
	}

	fmt.Println("\nsnooze cap")
	for _, depth := range []int{2, 3} {
		err := domain.CheckSnoozeCap(depth, domain.DefaultSnoozeCap)
		result := "another snooze allowed"
		if err != nil {
			result = err.Error()
		}
		fmt.Printf("  depth %d of %d  %s\n", depth, domain.DefaultSnoozeCap, result)
	}
}

// specExamples are the four schedule kinds exactly as docs/05-schedule-spec.md
// writes them, whitespace removed. They are the round-trip fixtures, so they
// are copied verbatim rather than constructed: a fixture built by the code it
// checks proves nothing.
var specExamples = []string{
	`{"kind":"one_off","at":"2026-08-14T10:00:00"}`,
	`{"kind":"fixed","rrule":"FREQ=DAILY","at":"09:00"}`,
	`{"kind":"windowed","rrule":"FREQ=WEEKLY;BYDAY=MO,TU,WE,TH,FR","window":["09:00","17:00"]}`,
	`{"kind":"fuzzy","period":"week","count":3,"days_allowed":["MO","TU","WE","TH","FR"],"window":["09:00","21:00"],"min_gap_hours":20}`,
}

// reportRoundTrips checks JSON -> Go -> JSON is unchanged for each kind.
//
// Byte equality rather than semantic equality is the point. The union marshals
// through struct tags and field order alone, with no custom MarshalJSON, and
// this is what says so: if a field were reordered or a zero value started
// emitting, the comparison fails here rather than showing up as a diff in the
// column six sessions later.
func reportRoundTrips() {
	fmt.Println("\nschedule round-trip")
	for _, want := range specExamples {
		s, err := schedule.Parse(json.RawMessage(want))
		if err != nil {
			fmt.Printf("  %-9s PARSE FAILED  %s\n", "?", err)
			continue
		}
		got, err := s.Marshal()
		if err != nil {
			fmt.Printf("  %-9s MARSHAL FAILED  %s\n", s.Kind, err)
			continue
		}
		result := "identical"
		if string(got) != want {
			result = "DIFFERS\n    want " + want + "\n    got  " + string(got)
		}
		fmt.Printf("  %-9s %s\n", s.Kind, result)
	}
}

// reportValidation runs every row of the validation table, plus the four kinds
// that should pass.
//
// The messages are the output that matters. Each one is fed to a model verbatim
// on the same-tier retry of the escalation ladder, so reading this block is how
// you check that the ladder has something to work with rather than eighteen
// variations of "invalid schedule".
func reportValidation(table *defaults.Table, loc *time.Location) {
	now := time.Now()

	accepted := []string{
		fmt.Sprintf(`{"kind":"one_off","at":%q}`, now.AddDate(0, 0, 7).Format(schedule.LocalDateTimeLayout)),
		`{"kind":"fixed","rrule":"FREQ=DAILY","at":"09:00"}`,
		`{"kind":"windowed","rrule":"FREQ=WEEKLY;BYDAY=MO,TU,WE,TH,FR","window":["09:00","17:00"]}`,
		`{"kind":"fuzzy","period":"week","count":3,"days_allowed":["MO","TU","WE","TH","FR"],"window":["09:00","21:00"],"min_gap_hours":20}`,
	}

	fmt.Println("\nschedule validation, accepted")
	for _, raw := range accepted {
		s, inferences, err := schedule.Prepare(json.RawMessage(raw), table, now, loc)
		if err != nil {
			fmt.Printf("  %-9s REJECTED  %s\n", s.Kind, err)
			continue
		}
		note := ""
		if len(inferences) > 0 {
			note = "  inferred " + schedule.Describe(inferences)
		}
		fmt.Printf("  %-9s ok  %s%s\n", s.Kind, s, note)
	}

	// One case per row of the table, in the order the table lists them, plus
	// the shape rules the union adds. want is the rule that should fire; when
	// a different one does, the ordering inside Validate has drifted and the
	// message a model would see is about the wrong thing.
	rejected := []struct {
		want string
		raw  string
	}{
		{"unknown_field", `{"kind":"fixed","rrule":"FREQ=DAILY","time":"09:00"}`},
		{"schedule_kind", `{"kind":"weekly","at":"09:00"}`},
		{"field_not_allowed", `{"kind":"fuzzy","period":"week","count":3,"rrule":"FREQ=DAILY"}`},
		{"field_required", `{"kind":"fixed","rrule":"FREQ=DAILY"}`},
		{"rrule_parses", `{"kind":"fixed","rrule":"FREQ=WEEKLY;BYDAY=XX","at":"09:00"}`},
		{"rrule_produces_occurrences", `{"kind":"fixed","rrule":"FREQ=YEARLY;BYMONTH=2;BYMONTHDAY=30","at":"09:00"}`},
		{"rrule_density", `{"kind":"fixed","rrule":"FREQ=HOURLY","at":"09:00"}`},
		{"window_shape", `{"kind":"windowed","rrule":"FREQ=DAILY","window":["09:00"]}`},
		{"local_time_format", `{"kind":"windowed","rrule":"FREQ=DAILY","window":["9am","17:00"]}`},
		{"window_ordered", `{"kind":"windowed","rrule":"FREQ=DAILY","window":["17:00","09:00"]}`},
		{"window_width", `{"kind":"windowed","rrule":"FREQ=DAILY","window":["09:00","09:15"]}`},
		{"period_valid", `{"kind":"fuzzy","period":"fortnight","count":3}`},
		{"count_range", `{"kind":"fuzzy","period":"week","count":25}`},
		{"days_allowed", `{"kind":"fuzzy","period":"week","count":3,"days_allowed":["MON","TU"]}`},
		{"min_gap_range", `{"kind":"fuzzy","period":"week","count":3,"min_gap_hours":-2}`},
		// The row the fuzzy kind exists for: arithmetically impossible, and
		// without this check it burns 200 placement attempts and quietly
		// produces three.
		{"gap_satisfiable", `{"kind":"fuzzy","period":"day","count":5,"min_gap_hours":8}`},
		{"local_datetime_format", `{"kind":"one_off","at":"2026-08-14 10:00"}`},
		{"one_off_future", `{"kind":"one_off","at":"2020-01-01T10:00:00"}`},
		{"one_off_bounded", `{"kind":"one_off","at":"2099-01-01T10:00:00"}`},
	}

	fmt.Println("\nschedule validation, rejected")
	for _, c := range rejected {
		_, _, err := schedule.Prepare(json.RawMessage(c.raw), table, now, loc)

		var ve *domain.ValidationError
		if !errors.As(err, &ve) {
			fmt.Printf("  %-26s NOT REJECTED  %v\n", c.want, err)
			continue
		}
		mark := "  "
		if ve.Rule != c.want {
			mark = "!!"
		}
		fmt.Printf("%s%-26s %-28s %s\n", mark, ve.Rule, ve.Field, ve.Message)
	}

	// The timezone row is not a schedule field, so it is checked on its own.
	if _, err := schedule.LoadLocation("Mars/Olympus"); err != nil {
		var ve *domain.ValidationError
		if errors.As(err, &ve) {
			fmt.Printf("  %-26s %-28s %s\n", ve.Rule, ve.Field, ve.Message)
		}
	}
}

// reportResolution shows an under-specified schedule coming back filled, with
// the inferred fields enumerated.
//
// The list is the deliverable, not the filled schedule. A5 requires the agent
// to state what it assumed, and D-015 makes that statement the reason the agent
// is allowed to never ask a clarifying question — a default applied silently is
// a wrong schedule nobody has a reason to look at.
func reportResolution(table *defaults.Table) {
	const raw = `{"kind":"fuzzy","period":"week","count":3}`

	s, err := schedule.Parse(json.RawMessage(raw))
	if err != nil {
		fmt.Printf("\ndefaults resolution\n  PARSE FAILED  %s\n", err)
		return
	}
	filled, inferences, err := schedule.Resolve(s, table)
	if err != nil {
		fmt.Printf("\ndefaults resolution\n  RESOLVE FAILED  %s\n", err)
		return
	}
	out, err := filled.Marshal()
	if err != nil {
		fmt.Printf("\ndefaults resolution\n  MARSHAL FAILED  %s\n", err)
		return
	}

	fmt.Println("\ndefaults resolution")
	fmt.Printf("  in    %s\n", raw)
	fmt.Printf("  out   %s\n", out)
	fmt.Printf("  reads %s\n", filled)
	fmt.Println("  inferred")
	for _, inf := range inferences {
		fmt.Printf("    %s\n", inf)
	}
	fmt.Printf("  stated as  %q\n", schedule.Describe(inferences))
}

// reportZones resolves the timezone for a fixed and a floating item, before and
// after kv.current_tz is set.
//
// The before-and-after is the interesting half. A floating item on a database
// that has never been told where the device is falls back to the item's own
// zone, which was the device zone when the item was created — the last known
// good answer to the same question, not a consolation prize.
func reportZones(ctx context.Context, st *store.Store, item domain.Item, fallback *time.Location) error {
	const away = "Europe/Lisbon"

	floating := item
	floating.TZMode = domain.TZModeFloating

	fixed := item
	fixed.TZMode = domain.TZModeFixed

	fmt.Println("\ntimezone resolution")

	current, ok, err := st.CurrentTZ(ctx)
	if err != nil {
		return err
	}
	if !ok {
		fmt.Printf("  kv.current_tz  unset\n")
		zones := schedule.Zones{Fallback: fallback}
		printZone(zones, "floating", floating)
		printZone(zones, "fixed", fixed)

		if err := st.SetCurrentTZ(ctx, away); err != nil {
			return err
		}
		current = away
		fmt.Printf("\n  kv.current_tz  set to %s\n", away)
	} else {
		fmt.Printf("  kv.current_tz  %s\n", current)
	}

	device, err := schedule.LoadLocation(current)
	if err != nil {
		return err
	}
	zones := schedule.Zones{Device: device, Fallback: fallback}
	printZone(zones, "floating", floating)
	printZone(zones, "fixed", fixed)
	return nil
}

func printZone(zones schedule.Zones, label string, item domain.Item) {
	loc, err := zones.For(item)
	if err != nil {
		fmt.Printf("  %-9s item tz %-16s  FAILED  %s\n", label, item.TZ, err)
		return
	}
	fmt.Printf("  %-9s item tz %-16s  resolves to %s\n", label, item.TZ, loc)
}

// localStamp carries the weekday and the offset. Both matter here: the weekday
// is what days_allowed is about, and the offset is the only visible difference
// between a reminder that held its wall clock across a DST boundary and one that
// did not.
const localStamp = "Mon 2006-01-02 15:04 -0700"

// reportDST prints the two worked examples from docs/05-schedule-spec.md#dst
// against a real transition, plus the ordinary times either side of them.
//
// Neither answer is the stdlib's, which is why time.Date is printed beside each
// one. For the gap it resolves 02:30 to 01:30 EST — an hour before what was
// asked for, on the far side of the transition — and for the fold it picks an
// offset without reporting that two were available.
func reportDST() {
	const zone = "America/New_York"

	loc, err := schedule.LoadLocation(zone)
	if err != nil {
		fmt.Printf("\ndst  FAILED  %s\n", err)
		return
	}

	locals := []schedule.LocalDateTime{
		{Year: 2026, Month: time.March, Day: 8, Hour: 1, Minute: 30},
		{Year: 2026, Month: time.March, Day: 8, Hour: 2, Minute: 30},
		{Year: 2026, Month: time.March, Day: 8, Hour: 3, Minute: 0},
		{Year: 2026, Month: time.November, Day: 1, Hour: 1, Minute: 0},
		{Year: 2026, Month: time.November, Day: 1, Hour: 1, Minute: 30},
		{Year: 2026, Month: time.November, Day: 1, Hour: 2, Minute: 30},
	}

	fmt.Printf("\ndst resolution  %s\n", zone)
	fmt.Printf("  %-21s %-10s %-30s %s\n", "wall clock", "fold", "instant", "time.Date would give")
	for _, d := range locals {
		at, fold := schedule.Instant(d, loc)
		fmt.Printf("  %-21s %-10s %-30s %s\n",
			d, fold, at.In(loc).Format(localStamp), d.In(loc).Format(localStamp))
	}
}

// reportMaterialization is the three invariants in
// docs/05-schedule-spec.md#materialization, checked against a real database
// rather than argued about: run it twice and nothing moves, an override
// survives, and a plain pending row in the same place does not.
//
// It is the part of this session a test file could not cover. The expansion
// arithmetic is verified in internal/materializer/expand_test.go; what happens
// to rows inside a transaction is verified here.
func reportMaterialization(ctx context.Context, st *store.Store, defaultTZ *time.Location, log *slog.Logger) error {
	mat := materializer.New(log.With("component", "materializer"), st, defaultTZ)

	item, err := seedFuzzyItem(ctx, st, defaultTZ.String())
	if err != nil {
		return err
	}

	loc, err := itemZone(ctx, st, item, defaultTZ)
	if err != nil {
		return err
	}
	fmt.Printf("\nmaterialization  item %s  %q  resolves to %s\n", item.ID, item.Title, loc)

	first, err := mat.All(ctx)
	if err != nil {
		return err
	}
	fmt.Printf("  run 1   items %d  inserted %d  deleted %d  kept %d\n",
		first.Items, first.Applied.Inserted, first.Applied.Deleted, first.Applied.Kept)

	// Invariant three. A second run over unchanged items writes nothing: every
	// slot it wants is already filled, including the drawn ones, so the times
	// already on the calendar do not reshuffle (D-005).
	second, err := mat.All(ctx)
	if err != nil {
		return err
	}
	fmt.Printf("  run 2   items %d  inserted %d  deleted %d  kept %d   idempotent %s\n",
		second.Items, second.Applied.Inserted, second.Applied.Deleted, second.Applied.Kept,
		verdict(!second.Applied.Changed()))

	if err := reportFuzzyPlacement(ctx, st, item, loc); err != nil {
		return err
	}
	if err := reportOverrideSurvival(ctx, st, mat, item, loc); err != nil {
		return err
	}
	if err := reportPause(ctx, st, mat, item); err != nil {
		return err
	}
	if err := reportHorizon(ctx, st); err != nil {
		return err
	}
	return reportBackfill(ctx, st, mat, log)
}

// reportPause enters vacation mode for three days, re-materializes, and counts
// what is left in the window (I6).
//
// Both halves are the assertion. Occurrences inside the window go, which is what
// makes "I'm away until Monday" one statement rather than eighteen skips; and
// they come back when it is lifted, which is what says the pause suppressed them
// rather than corrupting the schedule.
func reportPause(ctx context.Context, st *store.Store, mat *materializer.Materializer, item domain.Item) error {
	until := time.Now().Add(72 * time.Hour)

	before, err := countBetween(ctx, st, item, time.Now(), until)
	if err != nil {
		return err
	}

	fmt.Printf("\n  global pause  until %s\n", domain.FormatTime(until))
	fmt.Printf("    before   %d occurrence(s) in the window\n", before)

	if err := st.SetGlobalPauseUntil(ctx, until); err != nil {
		return err
	}
	if _, err := mat.All(ctx); err != nil {
		return err
	}
	during, err := countBetween(ctx, st, item, time.Now(), until)
	if err != nil {
		return err
	}
	fmt.Printf("    paused   %d  %s\n", during, verdict(during == 0))

	// Lifted by setting it into the past rather than deleting the key, which is
	// what the agent does when a trip ends early.
	if err := st.SetGlobalPauseUntil(ctx, time.Now().Add(-time.Second)); err != nil {
		return err
	}
	if _, err := mat.All(ctx); err != nil {
		return err
	}
	after, err := countBetween(ctx, st, item, time.Now(), until)
	if err != nil {
		return err
	}
	fmt.Printf("    lifted   %d  %s   (redrawn, so not the same times: D-005)\n",
		after, verdict(after > 0))
	return nil
}

// reportBackfill forces the horizon under the sweeper's floor and ticks it once.
//
// This is the backstop for a missed nightly run, and it is the one path in the
// system with no other way to notice it is broken: a horizon that stops moving
// looks exactly like a healthy one until the last materialized row fires.
func reportBackfill(ctx context.Context, st *store.Store, mat *materializer.Materializer, log *slog.Logger) error {
	short := time.Now().AddDate(0, 0, sweeper.MinHorizonDays-1)
	if err := st.SetLastMaterializedThrough(ctx, short); err != nil {
		return err
	}

	was, _, err := st.Horizon(ctx)
	if err != nil {
		return err
	}

	sw := sweeper.New(log.With("component", "sweeper"), st, mat)
	if err := sw.Tick(ctx); err != nil {
		return err
	}

	restored, _, err := st.Horizon(ctx)
	if err != nil {
		return err
	}

	fmt.Printf("\n  sweeper backfill  floor %d days\n", sweeper.MinHorizonDays)
	fmt.Printf("    horizon forced to %d, after one tick %d  %s\n",
		was, restored, verdict(restored >= sweeper.MinHorizonDays))

	// And again, to show the check is a check and not an unconditional re-run.
	if err := sw.Tick(ctx); err != nil {
		return err
	}
	again, _, err := st.Horizon(ctx)
	if err != nil {
		return err
	}
	fmt.Printf("    second tick over a healthy horizon leaves it at %d  %s\n",
		again, verdict(again == restored))
	return nil
}

// countBetween counts an item's non-override occurrences in a window. Overrides
// are excluded because they are exactly the rows a pause does not touch, and
// counting them would hide the thing being checked.
func countBetween(ctx context.Context, st *store.Store, item domain.Item, from, to time.Time) (int, error) {
	occurrences, err := st.ListOccurrencesForItem(ctx, item.ID)
	if err != nil {
		return 0, err
	}

	n := 0
	for _, occ := range occurrences {
		if occ.IsOverride {
			continue
		}
		if occ.StartsAt.After(from) && occ.StartsAt.Before(to) {
			n++
		}
	}
	return n, nil
}

// seedFuzzyItem creates the fuzzy item on first run and reuses it afterwards.
// The schedule is docs/05-schedule-spec.md's own example, verbatim: three times
// a week, weekdays, 09:00 to 21:00, at least twenty hours apart.
func seedFuzzyItem(ctx context.Context, st *store.Store, tz string) (domain.Item, error) {
	const title = "stretch"

	existing, err := st.ListActiveItems(ctx)
	if err != nil {
		return domain.Item{}, err
	}
	for _, it := range existing {
		if it.Title == title {
			return it, nil
		}
	}

	return st.CreateItem(ctx, domain.NewItem{
		Title:    title,
		Schedule: json.RawMessage(specExamples[3]),
		TZ:       tz,
	})
}

// reportFuzzyPlacement prints what the placement loop produced: the gap to the
// previous occurrence, which has to clear min_gap_hours, and the count per
// calendar week, where the first week is partial and gets a scaled target.
func reportFuzzyPlacement(ctx context.Context, st *store.Store, item domain.Item, loc *time.Location) error {
	s, err := schedule.Parse(item.Schedule)
	if err != nil {
		return err
	}
	gap := time.Duration(*s.MinGapHours) * time.Hour

	occurrences, err := st.ListOccurrencesForItem(ctx, item.ID)
	if err != nil {
		return err
	}

	fmt.Printf("\n  placement  %s  (min gap %s)\n", s, gap)

	weeks := map[string]int{}
	var previous time.Time
	for _, occ := range occurrences {
		if occ.StartsAt.Before(time.Now()) {
			continue
		}
		local := occ.StartsAt.In(loc)

		year, week := local.ISOWeek()
		weeks[fmt.Sprintf("%d-W%02d", year, week)]++

		// An override is neither the placement loop's output nor its input: it
		// does not answer to the gap or the count, and measuring against one
		// would report a violation the loop did not commit. So it is labelled
		// and stepped over, and the gap column stays a claim about the placement
		// loop alone. A week showing four is three placed plus one that
		// survived, which is the override mechanism working.
		if occ.IsOverride {
			fmt.Printf("    %-28s %7s  %s\n", local.Format(localStamp), "", "override")
			continue
		}

		mark, since := "      ", ""
		if !previous.IsZero() {
			d := local.Sub(previous)
			since = fmt.Sprintf("%6.1fh", d.Hours())
			mark = "ok"
			if d < gap {
				mark = "UNDER GAP"
			}
		}
		fmt.Printf("    %-28s %7s  %s\n", local.Format(localStamp), since, mark)
		previous = local
	}

	fmt.Printf("\n  per week   count %d; the first week is partial, so its target is scaled, floored, minimum one\n", *s.Count)
	for _, key := range sortedKeys(weeks) {
		fmt.Printf("    %-10s %d\n", key, weeks[key])
	}
	return nil
}

// reportOverrideSurvival plants two future pending rows no schedule asked for,
// one marked is_override and one not, and re-materializes.
//
// The contrast is the assertion. Both rows are equally unwanted, the only
// difference between them is the flag, and after the run the override is still
// there and the plain row is gone. That is the whole mechanism behind "skip
// tomorrow's" and behind snooze children, and it holds because of the WHERE
// clause on the store's delete rather than because the planner remembered to be
// careful.
func reportOverrideSurvival(ctx context.Context, st *store.Store, mat *materializer.Materializer, item domain.Item, loc *time.Location) error {
	// 04:00 and 05:00 local, outside the schedule's 09:00-21:00 window, so
	// neither can be mistaken for a slot the schedule wanted filled.
	day := time.Now().In(loc).AddDate(0, 0, 10)
	planted := []struct {
		hour     int
		override bool
	}{{4, true}, {5, false}}

	fmt.Println("\n  override survival")

	existing, err := st.ListOccurrencesForItem(ctx, item.ID)
	if err != nil {
		return err
	}

	ids := make([]string, 0, len(planted))
	for _, p := range planted {
		at, _ := schedule.Instant(schedule.LocalDateTime{
			Year: day.Year(), Month: day.Month(), Day: day.Day(), Hour: p.hour,
		}, loc)

		// The override survives every run, so a second invocation of this
		// command would stack a duplicate on top of it. Reusing it is both
		// tidier and a stronger claim: the row being checked below is one that
		// has already been through a materialization in an earlier process.
		if occ := occurrenceAt(existing, at); occ != nil {
			ids = append(ids, occ.ID)
			fmt.Printf("    present  %-28s is_override %t\n", at.In(loc).Format(localStamp), p.override)
			continue
		}

		occ, err := st.CreateOccurrence(ctx, domain.NewOccurrence{
			ItemID:     item.ID,
			StartsAt:   at,
			IsOverride: p.override,
		})
		if err != nil {
			return err
		}
		ids = append(ids, occ.ID)
		fmt.Printf("    planted  %-28s is_override %t\n", at.In(loc).Format(localStamp), p.override)
	}

	if _, err := mat.All(ctx); err != nil {
		return err
	}

	for i, id := range ids {
		_, err := st.GetOccurrence(ctx, id)
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			return err
		}
		survived := err == nil
		fmt.Printf("    after    is_override %-5t  survived %-5t  %s\n",
			planted[i].override, survived, verdict(survived == planted[i].override))
	}
	return nil
}

// reportHorizon prints what /healthz and navi_materializer_horizon_days now
// read. Before this session both were absent; the point of printing it is that
// the two are the same subtraction, made in one place.
func reportHorizon(ctx context.Context, st *store.Store) error {
	through, ok, err := st.LastMaterializedThrough(ctx)
	if err != nil {
		return err
	}
	if !ok {
		fmt.Println("\n  horizon    absent (nothing has materialized yet)")
		return nil
	}

	days, _, err := st.Horizon(ctx)
	if err != nil {
		return err
	}
	fmt.Printf("\n  horizon    through %s  =  %d days  (the sweeper re-runs under %d)\n",
		domain.FormatTime(through), days, sweeper.MinHorizonDays)
	return nil
}

// itemZone answers which location this item's wall clocks mean, the same way a
// materialization run does.
func itemZone(ctx context.Context, st *store.Store, item domain.Item, fallback *time.Location) (*time.Location, error) {
	zones := schedule.Zones{Fallback: fallback}

	name, ok, err := st.CurrentTZ(ctx)
	if err != nil {
		return nil, err
	}
	if ok {
		device, err := schedule.LoadLocation(name)
		if err != nil {
			return nil, err
		}
		zones.Device = device
	}
	return zones.For(item)
}

// occurrenceAt finds a row at exactly this instant, or nil.
func occurrenceAt(occurrences []domain.Occurrence, at time.Time) *domain.Occurrence {
	for i := range occurrences {
		if occurrences[i].StartsAt.Equal(at) {
			return &occurrences[i]
		}
	}
	return nil
}

func sortedKeys(m map[string]int) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// verdict renders a check, so a failure is greppable rather than something to be
// worked out from the numbers beside it.
func verdict(ok bool) string {
	if ok {
		return "ok"
	}
	return "FAILED"
}

// reportFire drives the scheduler against a transport that records instead of
// delivering, and checks every claim the fire path makes.
//
// Assertions are scoped to this section's own occurrences rather than to totals,
// because the database also holds thirty days of materialized rows from the
// section above and a previous run's rows may have come due since. Every
// occurrence written here carries a distinctive body for that reason, except one
// left deliberately without generated text so the N3 fallback is exercised.
func reportFire(ctx context.Context, st *store.Store, tz string, log *slog.Logger) error {
	fmt.Printf("\nfire path  recovery window %s  batch %d\n",
		scheduler.RecoveryWindow, scheduler.MaxBatch)

	item, err := seedFireItem(ctx, st, "fire path probe", domain.NotifyAtTime, tz)
	if err != nil {
		return err
	}
	silent, err := seedFireItem(ctx, st, "fire path silent", domain.NotifySilent, tz)
	if err != nil {
		return err
	}

	now := time.Now()

	// Inside the window, and with no message_text: this is the only send path
	// that exists at P0, since no copywriter has ever run.
	due, err := fireOccurrence(ctx, st, item, now.Add(-5*time.Minute), nil)
	if err != nil {
		return err
	}
	// Older than the window. C9 leaves this pending and silent; P3 asks about it.
	stale, err := fireOccurrence(ctx, st, item, now.Add(-2*time.Hour), nil)
	if err != nil {
		return err
	}
	// notify_policy = silent: generates, appears in the day view, resolvable,
	// never pushed (K1, K2).
	quiet, err := fireOccurrence(ctx, st, silent, now.Add(-5*time.Minute), ptr("fire path silent body"))
	if err != nil {
		return err
	}

	rec := &recordingTransport{}
	m := metrics.New()
	sched := scheduler.New(log.With("component", "scheduler"), st, rec, m, now)

	res, err := sched.Fire(ctx)
	if err != nil {
		return err
	}
	fmt.Printf("  claim floor %s  (%s before now)\n",
		domain.FormatTime(sched.ClaimFloor()), scheduler.RecoveryWindow)
	fmt.Printf("  pass 1   claimed %d  sent %d  failed %d  released %d  fallbacks %d  max latency %s\n",
		res.Claimed, res.Sent, res.Failed, res.Released, res.Fallbacks, res.MaxLatency.Truncate(time.Second))

	sentDue, err := statusOf(ctx, st, due.ID)
	if err != nil {
		return err
	}
	notifiedAtSet, err := notifiedAt(ctx, st, due.ID)
	if err != nil {
		return err
	}
	fireOnce := rec.count(item.Title) == 1 && sentDue == domain.StatusNotified && notifiedAtSet
	fmt.Printf("    due row fired once            %s -> %s, notified_at %s, %d send(s)  %s\n",
		domain.StatusPending, sentDue, presence(notifiedAtSet), rec.count(item.Title), verdict(fireOnce))

	// N3: the body is the plain item title, because message_text is null.
	fmt.Printf("    body fell back to the title   %q  %s\n",
		item.Title, verdict(rec.count(item.Title) == 1 && res.Fallbacks >= 1))

	staleStatus, err := statusOf(ctx, st, stale.ID)
	if err != nil {
		return err
	}
	fmt.Printf("    2h overdue left alone         %s, %d send(s)  %s\n",
		staleStatus, rec.count(item.Title)-1, verdict(staleStatus == domain.StatusPending))

	quietStatus, err := statusOf(ctx, st, quiet.ID)
	if err != nil {
		return err
	}
	fmt.Printf("    silent item not pushed        %s, %d send(s)  %s\n",
		quietStatus, rec.count("fire path silent body"),
		verdict(quietStatus == domain.StatusPending && rec.count("fire path silent body") == 0))

	if err := reportFireConcurrency(ctx, st, item, now, log); err != nil {
		return err
	}
	if err := reportFireRetry(ctx, st, item, now, log); err != nil {
		return err
	}
	if err := reportFirePause(ctx, st, item, now, log); err != nil {
		return err
	}

	fmt.Printf("    latency observed              %s  %s\n",
		res.MaxLatency.Truncate(time.Second), verdict(res.MaxLatency > 0))

	// The one thing this session must not have done. missed belongs to
	// reconciliation, which does not exist (K6, D-008).
	missed, err := countStatus(ctx, st, []string{item.ID, silent.ID}, domain.StatusMissed)
	if err != nil {
		return err
	}
	fmt.Printf("    nothing marked missed         %d row(s)  %s\n", missed, verdict(missed == 0))

	// The gauge does not latch: the two-hour row above is pending and overdue,
	// and is deliberately not counted, because it is past firing rather than
	// waiting on a stalled scheduler.
	overdue, err := st.PendingOverdue(ctx, sched.ClaimFloor())
	if err != nil {
		return err
	}
	fmt.Printf("    pending_overdue after firing  %d  (stale rows excluded by the floor)  %s\n",
		overdue, verdict(overdue == 0))

	// Item-level pause shares its predicate verbatim with the global one and with
	// the gauge, so it is checked by construction rather than here: there is no
	// writer for items.paused_until at P0, and adding one belongs to P3's edit
	// path rather than to a seeding tool.
	fmt.Printf("    item-level pause              no writer at P0, predicate shared with the claim\n")
	return nil
}

// reportFireConcurrency forces two passes at once. This is the BEGIN IMMEDIATE
// guarantee the whole claim rests on, and the one P1's synchronous write path
// and P2's endpoints will lean on next: two claimers see disjoint sets, so
// nothing is sent twice.
func reportFireConcurrency(ctx context.Context, st *store.Store, item domain.Item, now time.Time, log *slog.Logger) error {
	bodies := []string{"fire path concurrent A", "fire path concurrent B"}
	for _, b := range bodies {
		if _, err := fireOccurrence(ctx, st, item, now.Add(-4*time.Minute), ptr(b)); err != nil {
			return err
		}
	}

	rec := &recordingTransport{}
	m := metrics.New()
	first := scheduler.New(log.With("component", "scheduler"), st, rec, m, now)
	second := scheduler.New(log.With("component", "scheduler"), st, rec, m, now)

	var wg sync.WaitGroup
	results := make([]scheduler.Result, 2)
	errs := make([]error, 2)
	for i, s := range []*scheduler.Scheduler{first, second} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i], errs[i] = s.Fire(ctx)
		}()
	}
	wg.Wait()
	if err := errors.Join(errs...); err != nil {
		return err
	}

	disjoint := true
	for _, b := range bodies {
		if rec.count(b) != 1 {
			disjoint = false
		}
	}
	fmt.Printf("    double tick, disjoint claims  %d + %d claimed, each body sent once  %s\n",
		results[0].Claimed, results[1].Claimed, verdict(disjoint))
	return nil
}

// reportFireRetry simulates a transport outage. The failure-mode table says the
// occurrence stays pending and is retried, which is what the released claim
// makes true — and is why the claim floor is anchored to process start rather
// than sliding, since a sliding floor would age this row out after thirty ticks
// of the same failure.
func reportFireRetry(ctx context.Context, st *store.Store, item domain.Item, now time.Time, log *slog.Logger) error {
	const body = "fire path retry"

	occ, err := fireOccurrence(ctx, st, item, now.Add(-3*time.Minute), ptr(body))
	if err != nil {
		return err
	}

	m := metrics.New()
	failing := scheduler.New(log.With("component", "scheduler"), st, &failingTransport{}, m, now)

	// An error is expected here: the pass returns one so navi_loop_errors_total
	// sees the outage.
	res, _ := failing.Fire(ctx)
	afterFail, err := statusOf(ctx, st, occ.ID)
	if err != nil {
		return err
	}
	stampCleared, err := notifiedAt(ctx, st, occ.ID)
	if err != nil {
		return err
	}
	fmt.Printf("    send failure releases claim   %s, notified_at %s, released %d  %s\n",
		afterFail, presence(stampCleared), res.Released,
		verdict(afterFail == domain.StatusPending && !stampCleared && res.Released >= 1))

	rec := &recordingTransport{}
	working := scheduler.New(log.With("component", "scheduler"), st, rec, m, now)
	if _, err := working.Fire(ctx); err != nil {
		return err
	}
	afterRetry, err := statusOf(ctx, st, occ.ID)
	if err != nil {
		return err
	}
	fmt.Printf("    next pass delivers it         %s, %d send(s)  %s\n",
		afterRetry, rec.count(body),
		verdict(afterRetry == domain.StatusNotified && rec.count(body) == 1))
	return nil
}

// reportFirePause checks vacation mode at the fire path rather than at
// materialization: a paused system claims nothing, and the rows it did not claim
// are still pending afterwards.
func reportFirePause(ctx context.Context, st *store.Store, item domain.Item, now time.Time, log *slog.Logger) error {
	const body = "fire path paused"

	occ, err := fireOccurrence(ctx, st, item, now.Add(-2*time.Minute), ptr(body))
	if err != nil {
		return err
	}
	if err := st.SetGlobalPauseUntil(ctx, time.Now().Add(time.Hour)); err != nil {
		return err
	}

	rec := &recordingTransport{}
	sched := scheduler.New(log.With("component", "scheduler"), st, rec, metrics.New(), now)
	paused, err := sched.Fire(ctx)
	if err != nil {
		return err
	}
	during, err := statusOf(ctx, st, occ.ID)
	if err != nil {
		return err
	}

	// Counted before the pause is lifted. Reading the recorder afterwards would
	// see the delivery the next pass makes and report a pause that leaked.
	duringSends := rec.count(body)

	// Lifted the way the agent does when a trip ends early, rather than by
	// deleting the key.
	if err := st.SetGlobalPauseUntil(ctx, time.Now().Add(-time.Second)); err != nil {
		return err
	}
	if _, err := sched.Fire(ctx); err != nil {
		return err
	}
	after, err := statusOf(ctx, st, occ.ID)
	if err != nil {
		return err
	}

	fmt.Printf("    global pause claims nothing   paused=%t, %s, %d send(s)  %s\n",
		paused.Paused, during, duringSends,
		verdict(paused.Paused && during == domain.StatusPending && duringSends == 0))
	fmt.Printf("    lifted, then it fires         %s, %d send(s)  %s\n",
		after, rec.count(body), verdict(after == domain.StatusNotified && rec.count(body) == 1))
	return nil
}

// seedFireItem creates one of this section's items on first run and reuses it
// afterwards. Occurrences are not reused: every pass needs rows that have not
// been claimed yet.
func seedFireItem(ctx context.Context, st *store.Store, title string, policy domain.NotifyPolicy, tz string) (domain.Item, error) {
	existing, err := st.ListActiveItems(ctx)
	if err != nil {
		return domain.Item{}, err
	}
	for _, it := range existing {
		if it.Title == title {
			return it, nil
		}
	}

	// A one-off in the past, so the materializer expands nothing from it and
	// this section's rows stay the ones written here. Naive local date-time, not
	// an instant: the trailing Z belongs to stored timestamps, never to a
	// schedule's wall-clock time.
	sched := json.RawMessage(`{"kind":"one_off","at":"2020-01-01T09:00:00"}`)
	return st.CreateItem(ctx, domain.NewItem{
		Title:        title,
		Schedule:     sched,
		TZ:           tz,
		NotifyPolicy: &policy,
	})
}

func fireOccurrence(ctx context.Context, st *store.Store, item domain.Item, at time.Time, body *string) (domain.Occurrence, error) {
	return st.CreateOccurrence(ctx, domain.NewOccurrence{
		ItemID:      item.ID,
		StartsAt:    at,
		MessageText: body,

		// An override, so the materializer leaves it alone no matter what the
		// item's schedule expands to.
		IsOverride: true,
	})
}

func statusOf(ctx context.Context, st *store.Store, id string) (domain.Status, error) {
	occ, err := st.GetOccurrence(ctx, id)
	if err != nil {
		return "", err
	}
	return occ.Status, nil
}

func notifiedAt(ctx context.Context, st *store.Store, id string) (bool, error) {
	occ, err := st.GetOccurrence(ctx, id)
	if err != nil {
		return false, err
	}
	return occ.NotifiedAt != nil, nil
}

func countStatus(ctx context.Context, st *store.Store, itemIDs []string, want domain.Status) (int, error) {
	n := 0
	for _, id := range itemIDs {
		occurrences, err := st.ListOccurrencesForItem(ctx, id)
		if err != nil {
			return 0, err
		}
		for _, occ := range occurrences {
			if occ.Status == want {
				n++
			}
		}
	}
	return n, nil
}

func presence(set bool) string {
	if set {
		return "set"
	}
	return "null"
}

func ptr[T any](v T) *T { return &v }

// recordingTransport keeps what it was asked to send instead of sending it.
// Concurrency-safe because reportFireConcurrency drives two passes at once
// through one instance, which is the whole point of that check.
type recordingTransport struct {
	mu   sync.Mutex
	sent []transport.Outbound
}

func (t *recordingTransport) Name() string { return "recording" }

func (t *recordingTransport) Capabilities() transport.Capabilities {
	return transport.Capabilities{SupportsActions: true}
}

func (t *recordingTransport) Send(_ context.Context, msg transport.Outbound) (string, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.sent = append(t.sent, msg)
	return fmt.Sprintf("rec-%d", len(t.sent)), nil
}

func (t *recordingTransport) count(body string) int {
	t.mu.Lock()
	defer t.mu.Unlock()
	n := 0
	for _, msg := range t.sent {
		if msg.Body == body {
			n++
		}
	}
	return n
}

// failingTransport is a transport outage.
type failingTransport struct{}

func (t *failingTransport) Name() string { return "failing" }

func (t *failingTransport) Capabilities() transport.Capabilities { return transport.Capabilities{} }

func (t *failingTransport) Send(context.Context, transport.Outbound) (string, error) {
	return "", errors.New("failing: transport unreachable")
}

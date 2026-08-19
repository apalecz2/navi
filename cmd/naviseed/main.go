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
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	// Same reason as cmd/navi: the schedule below resolves against an IANA
	// zone, and a scratch image has no /usr/share/zoneinfo.
	_ "time/tzdata"

	"github.com/aidenpaleczny/navi/internal/agent"
	"github.com/aidenpaleczny/navi/internal/config"
	"github.com/aidenpaleczny/navi/internal/conversation"
	"github.com/aidenpaleczny/navi/internal/defaults"
	"github.com/aidenpaleczny/navi/internal/domain"
	"github.com/aidenpaleczny/navi/internal/health"
	"github.com/aidenpaleczny/navi/internal/httpapi"
	"github.com/aidenpaleczny/navi/internal/materializer"
	"github.com/aidenpaleczny/navi/internal/metrics"
	"github.com/aidenpaleczny/navi/internal/model"
	"github.com/aidenpaleczny/navi/internal/schedule"
	"github.com/aidenpaleczny/navi/internal/scheduler"
	"github.com/aidenpaleczny/navi/internal/store"
	"github.com/aidenpaleczny/navi/internal/sweeper"
	"github.com/aidenpaleczny/navi/internal/transport"
	"github.com/aidenpaleczny/navi/internal/transport/telegram"
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

	// After materialization, since it needs a real materializer and defaults
	// table, both already resolved by that point, and before fire, so the
	// items and occurrences it writes are not noise the fire-path assertions
	// have to account for.
	if err := reportAgentTools(ctx, st, table, cfg.Schedule.DefaultTZ, log); err != nil {
		return err
	}

	// Last, because it is the only section that sends anything, and because it
	// wants the global pause reportPause left lifted.
	if err := reportFire(ctx, st, cfg.Schedule.DefaultTZ.String(), log); err != nil {
		return err
	}

	// After fire, because the notified rows it resolves are ones the scheduler
	// claimed rather than ones written as notified by hand.
	if err := reportResolve(ctx, st, cfg.Schedule.DefaultTZ.String(), log); err != nil {
		return err
	}

	// After fire, since it exercises the other direction — inbound rather
	// than outbound — and needs nothing fire left behind.
	if err := reportConversations(ctx, st, log); err != nil {
		return err
	}

	// The model client, against local fake tier endpoints rather than a real
	// provider, needing nothing any earlier section left behind either.
	if err := reportModelClient(ctx, st, log, cfg.Files.ModelRoutingPath(), cfg.Model.Provider); err != nil {
		return err
	}

	// Last of all: the escalation ladder built on top of it - the session
	// this repository is on right now.
	return reportConversationLadder(ctx, st, table, cfg.Schedule.DefaultTZ, log)
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

// reportConversations exercises store.CreateConversation's round trip and its
// dedup guarantee directly, against the same predicate the webhook handler
// relies on, then hands off to reportWebhook to prove the same thing through
// the actual HTTP handler.
func reportConversations(ctx context.Context, st *store.Store, log *slog.Logger) error {
	fmt.Printf("\nconversations\n")

	tp, externalID := telegram.Name, "naviseed-dedup-probe"
	first, inserted, err := st.CreateConversation(ctx, domain.NewConversation{
		Role:       domain.RoleUser,
		Content:    "naviseed round trip",
		Transport:  &tp,
		ExternalID: &externalID,
	})
	if err != nil {
		return err
	}
	fmt.Printf("  create                          inserted=%v id=%s  %s\n",
		inserted, first.ID, verdict(inserted))

	back, err := st.GetConversation(ctx, first.ID)
	if err != nil {
		return err
	}
	fmt.Printf("  read back                       role=%s content=%q  %s\n",
		back.Role, back.Content, verdict(back.Role == domain.RoleUser && back.Content == first.Content))

	// Same (transport, external_id) again: the update dedup path Telegram's
	// retry-on-non-2xx behaviour depends on, exercised at the store level
	// before reportWebhook exercises it through the handler.
	dup, insertedAgain, err := st.CreateConversation(ctx, domain.NewConversation{
		Role:       domain.RoleUser,
		Content:    "naviseed round trip, redelivered",
		Transport:  &tp,
		ExternalID: &externalID,
	})
	if err != nil {
		return err
	}
	fmt.Printf("  redelivered (same external_id)  inserted=%v id matches first %v, content unchanged %v  %s\n",
		insertedAgain, dup.ID == first.ID, dup.Content == first.Content,
		verdict(!insertedAgain && dup.ID == first.ID && dup.Content == first.Content))

	return reportWebhook(ctx, st, log)
}

// fakeDispatcher stands in for internal/conversation.Intake: it satisfies
// telegram.Dispatcher without pulling a model client or an escalation
// ladder into a webhook-only check. full simulates a saturated intake
// buffer — Enqueue always returns false — without needing to actually fill
// one.
type fakeDispatcher struct {
	full  bool
	calls []transport.IncomingMessage
}

func (d *fakeDispatcher) Enqueue(msg transport.IncomingMessage) bool {
	if d.full {
		return false
	}
	d.calls = append(d.calls, msg)
	return true
}

// reportWebhook drives telegram.Inbound.ServeHTTP directly, in process,
// against the real store — the same "exercise the component without a
// listener" style reportFire uses for the scheduler. It proves secret
// rejection, allowlist drop, acceptance and update dedup through the actual
// handler rather than only through the store method underneath it, and,
// since session 11, that an accepted message is handed to the Dispatcher
// exactly once — never on a deduped redelivery — and that a saturated
// dispatcher still acknowledges the webhook while counting the drop.
func reportWebhook(ctx context.Context, st *store.Store, log *slog.Logger) error {
	fmt.Printf("\nwebhook  POST /webhook/telegram\n")

	const (
		secret     = "naviseed-webhook-secret"
		allowedID  = "111"
		strangerID = "999"
	)
	m := metrics.New()
	m.RegisterInboundAccepted(telegram.Name)
	m.RegisterInboundDropped("allowlist")
	m.RegisterInboundDropped("queue_full")
	disp := &fakeDispatcher{}
	h := telegram.NewInbound(secret, allowedID, st, disp, m, log.With("component", "webhook"))

	post := func(handler http.Handler, secretHeader, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/webhook/telegram", strings.NewReader(body))
		if secretHeader != "" {
			req.Header.Set("X-Telegram-Bot-Api-Secret-Token", secretHeader)
		}
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req.WithContext(ctx))
		return rec
	}
	update := func(id int, senderID, text string) string {
		return fmt.Sprintf(`{"update_id":%d,"message":{"from":{"id":%s},"text":%q}}`, id, senderID, text)
	}

	wrong := post(h, "wrong-secret", update(9001, allowedID, "hi"))
	fmt.Printf("  wrong secret            status %d  %s\n",
		wrong.Code, verdict(wrong.Code == http.StatusUnauthorized))

	stranger := post(h, secret, update(9002, strangerID, "hi"))
	_, strangerErr := st.GetConversationByExternalID(ctx, telegram.Name, "9002")
	strangerDropped := errors.Is(strangerErr, store.ErrNotFound)
	fmt.Printf("  non-allowlisted sender  status %d  dropped, nothing stored %v  %s\n",
		stranger.Code, strangerDropped, verdict(stranger.Code == http.StatusOK && strangerDropped))

	accepted := post(h, secret, update(9003, allowedID, "naviseed webhook probe"))
	row, err := st.GetConversationByExternalID(ctx, telegram.Name, "9003")
	if err != nil {
		return err
	}
	dispatched := len(disp.calls) == 1 && disp.calls[0].Text == "naviseed webhook probe"
	fmt.Printf("  allowlisted sender      status %d  content %q  dispatched %v  %s\n",
		accepted.Code, row.Content, dispatched,
		verdict(accepted.Code == http.StatusOK && row.Content == "naviseed webhook probe" && dispatched))

	redelivered := post(h, secret, update(9003, allowedID, "naviseed webhook probe, redelivered"))
	again, err := st.GetConversationByExternalID(ctx, telegram.Name, "9003")
	if err != nil {
		return err
	}
	fmt.Printf("  duplicate update_id     status %d  same row %v, content unchanged %v, not re-dispatched %v  %s\n",
		redelivered.Code, again.ID == row.ID, again.Content == row.Content, len(disp.calls) == 1,
		verdict(redelivered.Code == http.StatusOK && again.ID == row.ID && again.Content == row.Content && len(disp.calls) == 1))

	full := &fakeDispatcher{full: true}
	hFull := telegram.NewInbound(secret, allowedID, st, full, m, log.With("component", "webhook"))
	fullResp := post(hFull, secret, update(9004, allowedID, "queue full probe"))
	metricsRec := httptest.NewRecorder()
	m.Handler().ServeHTTP(metricsRec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	queueFullCounted := strings.Contains(metricsRec.Body.String(), `navi_inbound_messages_dropped_total{reason="queue_full"} 1`)
	fmt.Printf("  dispatcher queue full   status %d  drop counted %v  %s\n",
		fullResp.Code, queueFullCounted, verdict(fullResp.Code == http.StatusOK && queueFullCounted))

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

// reportResolve drives POST /api/occurrences/{id}/resolve through a real
// httpapi server, the same no-socket way reportWebhook drives the Telegram
// webhook: an http.Handler and httptest, so the route, the decode, the outcome
// mapping and the store write are all the ones production runs.
//
// The four cases are the four rows of docs/07-api-spec.md#idempotency. The
// last is driven one layer down, at the store, because pending is not a member
// of the endpoint's status enum and no valid request body can name it -
// checking it there is what shows ResolveOccurrence adds no rule of its own
// beyond domain.Transition's.
func reportResolve(ctx context.Context, st *store.Store, tz string, log *slog.Logger) error {
	fmt.Println("\nresolve  POST /api/occurrences/{id}/resolve")

	m := metrics.New()
	srv := httpapi.New(config.HTTP{Addr: ":0"}, log.With("component", "httpapi"),
		health.New(), m, st, time.Time{}, nil)

	post := func(id, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/api/occurrences/"+id+"/resolve",
			strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		srv.Handler.ServeHTTP(rec, req.WithContext(ctx))
		return rec
	}

	// A notified row, reached the way the scheduler reaches one rather than by
	// writing the status by hand.
	item, err := seedFireItem(ctx, st, "resolve path probe", domain.NotifyAtTime, tz)
	if err != nil {
		return err
	}
	claim := func(title string) (domain.Occurrence, error) {
		occ, err := fireOccurrence(ctx, st, item, time.Now().Add(-time.Minute), ptr(title))
		if err != nil {
			return domain.Occurrence{}, err
		}
		rec := &recordingTransport{}
		sched := scheduler.New(log.With("component", "scheduler-resolve"), st, rec, m, time.Now())
		if _, err := sched.Fire(ctx); err != nil {
			return domain.Occurrence{}, err
		}
		return st.GetOccurrence(ctx, occ.ID)
	}

	occ, err := claim("resolve probe body")
	if err != nil {
		return err
	}
	if occ.Status != domain.StatusNotified {
		return fmt.Errorf("naviseed: resolve: occurrence %s is %s, not notified", occ.ID, occ.Status)
	}

	// 1. A legal edge applies and writes all three columns.
	applied := post(occ.ID, `{"status":"completed","note":"done early","source":"web"}`)
	after, err := st.GetOccurrence(ctx, occ.ID)
	if err != nil {
		return err
	}
	appliedOK := applied.Code == http.StatusOK &&
		after.Status == domain.StatusCompleted &&
		after.ResolvedAt != nil &&
		after.ResolutionSource != nil && *after.ResolutionSource == domain.ResolvedByWeb
	fmt.Printf("  notified -> completed   status %d  occurrence=%s resolved_at=%s source=%s  %s\n",
		applied.Code, after.Status, presence(after.ResolvedAt != nil),
		sourceOf(after.ResolutionSource), verdict(appliedOK))

	// 2. The same terminal state again: 200, and nothing written.
	noop := post(occ.ID, `{"status":"completed","note":"second tap","source":"web"}`)
	again, err := st.GetOccurrence(ctx, occ.ID)
	if err != nil {
		return err
	}
	unchanged := again.ResolvedAt != nil && after.ResolvedAt != nil &&
		again.ResolvedAt.Equal(*after.ResolvedAt) &&
		noteOf(again.ResolutionNote) == noteOf(after.ResolutionNote)
	fmt.Printf("  completed -> completed  status %d  nothing rewritten=%v  %s\n",
		noop.Code, unchanged, verdict(noop.Code == http.StatusOK && unchanged))

	// 3. A different terminal state: 409, reporting the state it is in.
	conflict := post(occ.ID, `{"status":"skipped","note":null,"source":"web"}`)
	fmt.Printf("  completed -> skipped    status %d  current_state=%q  %s\n",
		conflict.Code, currentState(conflict.Body.Bytes()),
		verdict(conflict.Code == http.StatusConflict &&
			currentState(conflict.Body.Bytes()) == string(domain.StatusCompleted)))

	// 4. An illegal edge, at the store: notified -> pending is not in the table.
	illegalOcc, err := claim("resolve illegal body")
	if err != nil {
		return err
	}
	res, err := st.ResolveOccurrence(ctx, illegalOcc.ID, domain.StatusPending, nil, domain.ResolvedByWeb, time.Now())
	var te *domain.TransitionError
	illegal := errors.As(err, &te) && res.Outcome == domain.OutcomeIllegal
	stillNotified, err := statusOf(ctx, st, illegalOcc.ID)
	if err != nil {
		return err
	}
	fmt.Printf("  notified -> pending     illegal=%v  row still %s  %s\n",
		illegal, stillNotified,
		verdict(illegal && stillNotified == domain.StatusNotified))
	if te != nil {
		fmt.Printf("    message               %q\n", te.Message)
	}

	// R3, US-4.1: a pending occurrence resolved before it fires is one the
	// scheduler never sends. There is no cancel path and no flag - the row
	// leaves status = 'pending', which is the predicate both ListDueOccurrences
	// and ClaimOccurrence are built on, so it falls out of the fire path by
	// construction. This drives it end to end rather than asserting it: a row
	// that is due right now, completed first, then a real scheduler pass.
	early, err := fireOccurrence(ctx, st, item, time.Now().Add(-time.Minute), ptr("resolve early body"))
	if err != nil {
		return err
	}
	earlyResolve := post(early.ID, `{"status":"completed","note":"did it at breakfast","source":"agent"}`)
	earlyRec := &recordingTransport{}
	earlySched := scheduler.New(log.With("component", "scheduler-early"), st, earlyRec, m, time.Now())
	earlyFire, err := earlySched.Fire(ctx)
	if err != nil {
		return err
	}
	earlyAfter, err := st.GetOccurrence(ctx, early.ID)
	if err != nil {
		return err
	}
	neverSent := earlyResolve.Code == http.StatusOK &&
		earlyAfter.Status == domain.StatusCompleted &&
		earlyAfter.NotifiedAt == nil &&
		earlyRec.count("resolve early body") == 0
	fmt.Printf("  pending -> completed    status %d  claimed=%d  %d send(s)  notified_at=%s  %s\n",
		earlyResolve.Code, earlyFire.Claimed, earlyRec.count("resolve early body"),
		presence(earlyAfter.NotifiedAt != nil), verdict(neverSent))

	// The metric carries the resolution, labelled by the surface that made it.
	metricsRec := httptest.NewRecorder()
	m.Handler().ServeHTTP(metricsRec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	wantSeries := `navi_occurrence_transitions_total{from="notified",source="web",to="completed"} 1`
	counted := strings.Contains(metricsRec.Body.String(), wantSeries)
	fmt.Printf("  transitions metric      %s  %s\n", wantSeries, verdict(counted))

	// A missing occurrence is a 404 and not a 500, and a body the schema would
	// reject never reaches the store.
	missing := post("01ARZ3NDEKTSV4RRFFQ69G5FAV", `{"status":"completed","note":null,"source":"web"}`)
	badStatus := post(occ.ID, `{"status":"finished","note":null,"source":"web"}`)
	badSource := post(occ.ID, `{"status":"completed","note":null,"source":"telepathy"}`)
	fmt.Printf("  unknown occurrence      status %d  %s\n",
		missing.Code, verdict(missing.Code == http.StatusNotFound))
	fmt.Printf("  status not in the enum  status %d  %s\n",
		badStatus.Code, verdict(badStatus.Code == http.StatusBadRequest))
	fmt.Printf("  source not in the enum  status %d  %s\n",
		badSource.Code, verdict(badSource.Code == http.StatusBadRequest))

	return nil
}

// currentState reads the current_state field a 409 body carries.
func currentState(body []byte) string {
	var out struct {
		CurrentState string `json:"current_state"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return ""
	}
	return out.CurrentState
}

func sourceOf(s *domain.ResolutionSource) string {
	if s == nil {
		return "null"
	}
	return string(*s)
}

func noteOf(s *string) string {
	if s == nil {
		return "null"
	}
	return *s
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

// reportModelClient exercises the model client's mechanism — tier
// resolution, one HTTP round trip per attempt, error classification, and
// the llm_calls row and metric observations every path (success or failure)
// writes — against local httptest servers standing in for tier endpoints,
// on the same fake-instead-of-real-network reasoning
// recordingTransport/failingTransport give for the fire path, just at the
// HTTP layer instead of a Go interface. Nothing here calls a real provider
// or needs OPENROUTER_API_KEY.
func reportModelClient(ctx context.Context, st *store.Store, log *slog.Logger, routingPath, provider string) error {
	fmt.Printf("\nmodel client\n")

	okServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{
			"model": "naviseed-ok",
			"choices": [{
				"index": 0,
				"message": {
					"role": "assistant",
					"content": "",
					"tool_calls": [{
						"id": "call_1",
						"type": "function",
						"function": {"name": "bulk_resolve", "arguments": "{\"resolutions\":[{\"occurrence_id\":\"occ_1\",\"status\":\"completed\"}]}"}
					}]
				},
				"finish_reason": "tool_calls"
			}],
			"usage": {"prompt_tokens": 142, "completion_tokens": 23}
		}`)
	}))
	defer okServer.Close()

	failServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprint(w, `{"error": {"message": "upstream outage", "type": "server_error"}}`)
	}))
	defer failServer.Close()

	slowServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(3 * time.Second)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"choices":[{"index":0,"message":{"role":"assistant","content":"too slow to matter"},"finish_reason":"stop"}]}`)
	}))
	defer slowServer.Close()

	// A routing table built in Go rather than loaded from YAML, so each task
	// can point at a fake server this function controls instead of a real
	// provider. bulk_resolve has one tier that succeeds outright; crud fails
	// at tier 1 and succeeds at tier 2, for the escalation check; reconcile's
	// one tier is slower than its own timeout, for the timeout check.
	routing := &model.Routing{
		Tasks: map[model.Task]model.TaskRouting{
			model.TaskBulkResolve: {
				Tiers: []model.Tier{
					{Model: "naviseed-tier1", BaseURL: okServer.URL, TimeoutSeconds: 5},
				},
			},
			model.TaskCRUD: {
				Thinking: true,
				Tiers: []model.Tier{
					{Model: "naviseed-tier1", BaseURL: failServer.URL, TimeoutSeconds: 5},
					{Model: "naviseed-tier2", BaseURL: okServer.URL, TimeoutSeconds: 5},
				},
			},
			model.TaskReconcile: {
				Tiers: []model.Tier{
					{Model: "naviseed-slow", BaseURL: slowServer.URL, TimeoutSeconds: 1},
				},
			},
		},
	}

	m := metrics.New()
	client := model.New(log.With("component", "model"), routing, "", st, m)
	msgs := []model.Message{{Role: model.RoleUser, Content: "mark vitamins and stretching done"}}

	// Tier 1 succeeds directly: a completion with tool calls, and an
	// llm_calls row carrying token counts and latency.
	res1, err1 := client.Complete(ctx, model.Request{Task: model.TaskBulkResolve, Tier: 1, Messages: msgs})
	row1, err := lastLLMCall(ctx, st)
	if err != nil {
		return err
	}
	ok1 := err1 == nil && len(res1.Message.ToolCalls) == 1 &&
		row1.PromptTokens != nil && *row1.PromptTokens > 0 &&
		row1.CompletionTokens != nil && row1.LatencyMS != nil && row1.Error == nil
	fmt.Printf("  tier-1 success                  tool_calls=%d tokens=%s/%s latency_ms=%s  %s\n",
		len(res1.Message.ToolCalls), intOrNull(row1.PromptTokens), intOrNull(row1.CompletionTokens),
		intOrNull(row1.LatencyMS), verdict(ok1))

	// Tier 1 fails (the server returns 500), which must still write a row.
	// Tier 2, called with Escalation set the way the ladder will next
	// session, succeeds and its row carries the reason verbatim.
	_, err2a := client.Complete(ctx, model.Request{Task: model.TaskCRUD, Tier: 1, Messages: msgs})
	rowFail, err := lastLLMCall(ctx, st)
	if err != nil {
		return err
	}
	var kind2a model.ErrorKind
	var mErr2a *model.Error
	if errors.As(err2a, &mErr2a) {
		kind2a = mErr2a.Kind
	}

	const reason = "tier 1 unavailable"
	_, err2b := client.Complete(ctx, model.Request{
		Task: model.TaskCRUD, Tier: 2, Messages: msgs,
		Escalation: &model.Escalation{Reason: reason},
	})
	rowEscalated, err := lastLLMCall(ctx, st)
	if err != nil {
		return err
	}
	ok2 := err2a != nil && kind2a == model.KindUnavailable && rowFail.Error != nil &&
		err2b == nil && rowEscalated.Escalated && strOrNull(rowEscalated.EscalationReason) == reason
	fmt.Printf("  tier-1 failure then escalation  tier1_kind=%s tier1_row_error=%s escalated=%v reason=%q  %s\n",
		kind2a, strOrNull(rowFail.Error), rowEscalated.Escalated, strOrNull(rowEscalated.EscalationReason), verdict(ok2))

	// The tier's own timeout (1s) is shorter than the server's delay (3s):
	// Complete must return well short of 3s, classified KindTimeout, with a
	// row still written.
	timeoutStart := time.Now()
	_, err3 := client.Complete(ctx, model.Request{Task: model.TaskReconcile, Tier: 1, Messages: msgs})
	elapsed := time.Since(timeoutStart)
	rowTimeout, err := lastLLMCall(ctx, st)
	if err != nil {
		return err
	}
	var kind3 model.ErrorKind
	var mErr3 *model.Error
	if errors.As(err3, &mErr3) {
		kind3 = mErr3.Kind
	}
	ok3 := err3 != nil && kind3 == model.KindTimeout && elapsed < 2*time.Second && rowTimeout.Error != nil
	fmt.Printf("  timeout bounded                 elapsed=%s kind=%s row_latency_ms=%s  %s\n",
		elapsed.Round(time.Millisecond), kind3, intOrNull(rowTimeout.LatencyMS), verdict(ok3))

	// The two metric families, scraped the same way reportWebhook scrapes
	// navi_inbound_messages_accepted_total: through the handler, not by
	// reaching into the registry.
	metricsReq := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	metricsRec := httptest.NewRecorder()
	m.Handler().ServeHTTP(metricsRec, metricsReq)
	metricsBody := metricsRec.Body.String()
	hasCalls := strings.Contains(metricsBody, "navi_llm_calls_total")
	hasLatency := strings.Contains(metricsBody, "navi_llm_latency_seconds")
	fmt.Printf("  metrics                          navi_llm_calls_total=%v navi_llm_latency_seconds=%v  %s\n",
		hasCalls, hasLatency, verdict(hasCalls && hasLatency))

	// The real committed config/model.yaml, loaded and validated the way
	// main will — independent of the fake routing table used above.
	real, loadErr := model.LoadRouting(routingPath)
	taskCount := 0
	if real != nil {
		taskCount = len(real.Tasks)
	}
	fmt.Printf("  %s                     tasks=%d  %s\n",
		routingPath, taskCount, verdict(loadErr == nil && taskCount == 5))

	// Checked against whichever MODEL_PROVIDER this run is actually
	// configured with — the same check main runs before wiring a Client —
	// rather than a hardcoded default, so a deployment that has deliberately
	// switched to Gemini sees this pass, not a false FAILED against a
	// provider it never chose. A base_url edited without updating
	// MODEL_PROVIDER (or vice versa) still fails here on a clean checkout.
	var providerErr error
	if real != nil {
		providerErr = real.ValidateProvider(model.Provider(provider))
	}
	fmt.Printf("  %s provider=%s  %s\n",
		routingPath, provider, verdict(real != nil && providerErr == nil))

	// Retention, exercised directly against the store method the sweeper
	// calls: a cutoff in the deep past deletes nothing that exists, a cutoff
	// in the future deletes everything this section just wrote. This proves
	// the WHERE clause both ways without backdating a row through the
	// store's normal (always-time.Now()) write path.
	before, err := countLLMCalls(ctx, st)
	if err != nil {
		return err
	}
	deletedNone, err := st.PruneLLMCalls(ctx, time.Now().Add(-365*24*time.Hour))
	if err != nil {
		return err
	}
	afterNone, err := countLLMCalls(ctx, st)
	if err != nil {
		return err
	}
	fmt.Printf("  retention: old cutoff            deleted=%d, before=%d, after=%d  %s\n",
		deletedNone, before, afterNone, verdict(deletedNone == 0 && afterNone == before))

	deletedAll, err := st.PruneLLMCalls(ctx, time.Now().Add(time.Hour))
	if err != nil {
		return err
	}
	afterAll, err := countLLMCalls(ctx, st)
	if err != nil {
		return err
	}
	fmt.Printf("  retention: future cutoff          deleted=%d, remaining=%d  %s\n",
		deletedAll, afterAll, verdict(int(deletedAll) == afterNone && afterAll == 0))

	return nil
}

// lastLLMCall returns the most recently written llm_calls row.
func lastLLMCall(ctx context.Context, st *store.Store) (domain.LLMCall, error) {
	rows, err := st.ListLLMCalls(ctx, 1)
	if err != nil {
		return domain.LLMCall{}, err
	}
	if len(rows) == 0 {
		return domain.LLMCall{}, fmt.Errorf("naviseed: llm_calls is empty")
	}
	return rows[0], nil
}

// countLLMCalls counts every row. Fine at naviseed's scale; not a query this
// codebase would run against a real deployment's table.
func countLLMCalls(ctx context.Context, st *store.Store) (int, error) {
	rows, err := st.ListLLMCalls(ctx, 100000)
	if err != nil {
		return 0, err
	}
	return len(rows), nil
}

// intOrNull and strOrNull render a nullable column for a report line, on the
// same "null" convention presence() uses for a bool.
func intOrNull(p *int) string {
	if p == nil {
		return "null"
	}
	return fmt.Sprintf("%d", *p)
}

func strOrNull(p *string) string {
	if p == nil {
		return "null"
	}
	return *p
}

// fakeSender stands in for the outbound Telegram transport: it records the
// body of every reply the ladder sends instead of reaching a real chat.
type fakeSender struct {
	bodies []string
}

func (f *fakeSender) Send(ctx context.Context, msg transport.Outbound) (string, error) {
	f.bodies = append(f.bodies, msg.Body)
	return "fake-message-id", nil
}

func (f *fakeSender) last() string {
	if len(f.bodies) == 0 {
		return ""
	}
	return f.bodies[len(f.bodies)-1]
}

// toolCallServer answers every request with a single tool call, on the same
// wire shape reportModelClient's okServer uses. argsJSON is the tool's raw
// JSON arguments, marshaled here into the escaped string the
// "arguments" field carries on the wire.
func toolCallServer(toolName, argsJSON string) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		argsEscaped, _ := json.Marshal(argsJSON)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"choices":[{"index":0,"message":{"role":"assistant","content":"","tool_calls":[{"id":"call_1","type":"function","function":{"name":%q,"arguments":%s}}]},"finish_reason":"tool_calls"}]}`,
			toolName, string(argsEscaped))
	}))
}

// wireRequest is the minimal shape of the OpenAI-compatible chat request
// body this file needs to inspect - just enough to find a message by role,
// not the full internal/model wire format (which stays unexported there on
// purpose; this is a second, deliberately narrower reader of the same JSON).
type wireRequest struct {
	Messages []struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	} `json:"messages"`
}

// systemContent returns the request's one system message, so a test can
// check what internal/conversation.Ladder actually assembled and sent
// rather than only what came back in the reply.
func (r wireRequest) systemContent() string {
	for _, m := range r.Messages {
		if m.Role == "system" {
			return m.Content
		}
	}
	return ""
}

// countUserContent counts how many messages carry role "user" and exactly
// this content - seedHistory's drop-the-duplicate rule should always leave
// exactly one, never zero (dropped for good) and never two (never deduped).
func (r wireRequest) countUserContent(text string) int {
	n := 0
	for _, m := range r.Messages {
		if m.Role == "user" && m.Content == text {
			n++
		}
	}
	return n
}

// capturingToolCallServer behaves like toolCallServer but also records every
// request body it receives, in order - used to inspect the system prompt
// and the carried-over history Handle actually sent, not only what it wrote
// back through the fake Sender.
func capturingToolCallServer(toolName, argsJSON string, captured *[]wireRequest) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req wireRequest
		if err := json.Unmarshal(body, &req); err == nil {
			*captured = append(*captured, req)
		}
		argsEscaped, _ := json.Marshal(argsJSON)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"choices":[{"index":0,"message":{"role":"assistant","content":"","tool_calls":[{"id":"call_1","type":"function","function":{"name":%q,"arguments":%s}}]},"finish_reason":"tool_calls"}]}`,
			toolName, string(argsEscaped))
	}))
}

// proseServer answers every request with plain content and no tool call —
// the "no tool call when a write was expected" case (L4).
func proseServer(text string) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		textEscaped, _ := json.Marshal(text)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"choices":[{"index":0,"message":{"role":"assistant","content":%s},"finish_reason":"stop"}]}`,
			string(textEscaped))
	}))
}

// authFailServer answers every request with the 401 a dead API key
// produces against a real provider — classified model.KindMalformed, not
// retryable, so the ladder should spend no wasted retry against it.
func authFailServer() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprint(w, `{"error": {"message": "invalid api key", "type": "invalid_request_error"}}`)
	}))
}

// truncate shortens s for a report line so a long confirmation or apology
// doesn't wrap the table.
func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}

// reportConversationLadder drives internal/conversation.Ladder.Handle
// directly against fake tier endpoints and a fake Sender - no real model
// provider, no real Telegram bot - proving the escalation ladder's "done
// when" bullets: a valid schedule creates the item and gets a reply; an
// unsatisfiable one walks the whole ladder, writes nothing, and produces
// four llm_calls rows; request_escalation jumps a tier and is recorded;
// prose escalates rather than being accepted; a dead API key produces an
// apology without a wasted retry; and one turn produces user, assistant and
// tool rows in conversations.
func reportConversationLadder(ctx context.Context, st *store.Store, table *defaults.Table, defaultTZ *time.Location, log *slog.Logger) error {
	fmt.Println("\nconversation ladder")

	mat := materializer.New(log.With("component", "materializer"), st, defaultTZ)
	tools := agent.New(st, mat, table, defaultTZ)
	m := metrics.New()
	// The P5 persona slot, deliberately absent this session - GetPersona
	// tolerates a missing file, so this path never needs to exist.
	const personaPath = "config/persona.md.does-not-exist"

	newLadder := func(tier1, tier2 *httptest.Server) (*conversation.Ladder, *fakeSender) {
		tiers := []model.Tier{{Model: "naviseed-tier1", BaseURL: tier1.URL, TimeoutSeconds: 5}}
		if tier2 != nil {
			tiers = append(tiers, model.Tier{Model: "naviseed-tier2", BaseURL: tier2.URL, TimeoutSeconds: 5})
		}
		routing := &model.Routing{Tasks: map[model.Task]model.TaskRouting{
			model.TaskCRUD: {Thinking: true, Tiers: tiers},
		}}
		client := model.New(log.With("component", "model"), routing, "", st, m)
		sender := &fakeSender{}
		return conversation.New(client, tools, routing, st, table, personaPath, defaultTZ, sender), sender
	}

	// 1. A tier-1 server that always returns a valid create_item call for
	// "vitamins daily at 9am" - and, immediately after, read the turn back
	// (done-when #6: one turn produces user, assistant, and tool rows).
	{
		userMsg := "Remind me to take vitamins daily at 9am"
		if _, _, err := st.CreateConversation(ctx, domain.NewConversation{Role: domain.RoleUser, Content: userMsg}); err != nil {
			return err
		}

		args := `{"title":"vitamins daily test","schedule":{"kind":"fixed","rrule":"FREQ=DAILY","at":"09:00"}}`
		tier1 := toolCallServer("create_item", args)
		defer tier1.Close()
		ladder, sender := newLadder(tier1, nil)

		err := ladder.Handle(ctx, transport.IncomingMessage{SenderID: "111", Text: userMsg, Transport: telegram.Name})
		if err != nil {
			return err
		}

		items, err := st.ListActiveItems(ctx)
		if err != nil {
			return err
		}
		var created *domain.Item
		for i := range items {
			if items[i].Title == "vitamins daily test" {
				created = &items[i]
			}
		}
		var occCount int
		if created != nil {
			occs, oerr := st.ListOccurrencesForItem(ctx, created.ID)
			if oerr != nil {
				return oerr
			}
			occCount = len(occs)
		}
		ok1 := created != nil && occCount >= 3 && sender.last() != ""
		fmt.Printf("  1  create end to end          item created=%v occurrences=%d reply=%q  %s\n",
			created != nil, occCount, truncate(sender.last(), 60), verdict(ok1))

		rows, err := st.ListRecentConversations(ctx, 10)
		if err != nil {
			return err
		}
		var hasUser, hasAssistant, hasTool bool
		for _, r := range rows {
			switch r.Role {
			case domain.RoleUser:
				hasUser = true
			case domain.RoleAssistant:
				hasAssistant = true
			case domain.RoleTool:
				hasTool = true
			}
		}
		fmt.Printf("  6  conversations rows         user=%v assistant=%v tool=%v  %s\n",
			hasUser, hasAssistant, hasTool, verdict(hasUser && hasAssistant && hasTool))
	}

	// 2. Both tiers always return an unsatisfiable schedule (the same
	// gap-unsatisfiable fixture reportLayerRejections uses) - the whole
	// ladder walks, four llm_calls rows, and nothing is written.
	{
		before, err := countLLMCalls(ctx, st)
		if err != nil {
			return err
		}
		itemsBefore, err := st.ListActiveItems(ctx)
		if err != nil {
			return err
		}

		args := `{"title":"bad gap ladder test","schedule":{"kind":"fuzzy","period":"day","count":10,"min_gap_hours":8}}`
		tier1 := toolCallServer("create_item", args)
		defer tier1.Close()
		tier2 := toolCallServer("create_item", args)
		defer tier2.Close()
		ladder, sender := newLadder(tier1, tier2)

		if err := ladder.Handle(ctx, transport.IncomingMessage{SenderID: "111", Text: "remind me about the impossible thing", Transport: telegram.Name}); err != nil {
			return err
		}
		after, err := countLLMCalls(ctx, st)
		if err != nil {
			return err
		}
		itemsAfter, err := st.ListActiveItems(ctx)
		if err != nil {
			return err
		}
		callsMade := after - before
		ok := callsMade == 4 && len(itemsAfter) == len(itemsBefore) &&
			strings.Contains(strings.ToLower(sender.last()), "rephrase")
		fmt.Printf("  2  ladder exhaustion          llm_calls +%d  items unchanged=%v  reply=%q  %s\n",
			callsMade, len(itemsAfter) == len(itemsBefore), truncate(sender.last(), 60), verdict(ok))
	}

	// 3. request_escalation at tier 1, success at tier 2 - one retry
	// skipped, the reason recorded verbatim.
	{
		before, err := countLLMCalls(ctx, st)
		if err != nil {
			return err
		}

		const reason = "the request references an item I cannot identify"
		escArgs, _ := json.Marshal(map[string]string{"reason": reason})
		tier1 := toolCallServer("request_escalation", string(escArgs))
		defer tier1.Close()
		createArgs := `{"title":"escalated create test","schedule":{"kind":"fixed","rrule":"FREQ=DAILY","at":"10:00"}}`
		tier2 := toolCallServer("create_item", createArgs)
		defer tier2.Close()
		ladder, sender := newLadder(tier1, tier2)

		if err := ladder.Handle(ctx, transport.IncomingMessage{SenderID: "111", Text: "do the ambiguous thing", Transport: telegram.Name}); err != nil {
			return err
		}
		after, err := countLLMCalls(ctx, st)
		if err != nil {
			return err
		}
		rows, err := st.ListLLMCalls(ctx, 1)
		if err != nil {
			return err
		}
		callsMade := after - before
		escalatedOK := len(rows) == 1 && rows[0].Escalated && strOrNull(rows[0].EscalationReason) == reason
		ok := callsMade == 2 && escalatedOK
		fmt.Printf("  3  request_escalation         llm_calls +%d  escalation recorded=%v reply=%q  %s\n",
			callsMade, escalatedOK, truncate(sender.last(), 60), verdict(ok))
	}

	// 4. Prose only, both tiers - escalates through the ladder rather than
	// being accepted as an answer.
	{
		before, err := countLLMCalls(ctx, st)
		if err != nil {
			return err
		}
		tier1 := proseServer("I'm not sure what you mean.")
		defer tier1.Close()
		tier2 := proseServer("Still not sure.")
		defer tier2.Close()
		ladder, sender := newLadder(tier1, tier2)

		if err := ladder.Handle(ctx, transport.IncomingMessage{SenderID: "111", Text: "hmm", Transport: telegram.Name}); err != nil {
			return err
		}
		after, err := countLLMCalls(ctx, st)
		if err != nil {
			return err
		}
		callsMade := after - before
		ok := callsMade == 4 && strings.Contains(strings.ToLower(sender.last()), "rephrase")
		fmt.Printf("  4  prose escalates            llm_calls +%d  reply=%q  %s\n",
			callsMade, truncate(sender.last(), 60), verdict(ok))
	}

	// 5. Both tiers return the 401 a dead OPENROUTER_API_KEY produces - no
	// wasted retry (KindMalformed is not Retryable), an apology rather than
	// a rephrase request, and nothing here touches the fire path: the
	// scheduler package cannot import internal/model or internal/
	// conversation at all, which is a compile-time property, not a runtime
	// one - checkable independently with
	// `go list -deps ./internal/scheduler | grep -c internal/model`.
	{
		before, err := countLLMCalls(ctx, st)
		if err != nil {
			return err
		}
		tier1 := authFailServer()
		defer tier1.Close()
		tier2 := authFailServer()
		defer tier2.Close()
		ladder, sender := newLadder(tier1, tier2)

		if err := ladder.Handle(ctx, transport.IncomingMessage{SenderID: "111", Text: "remind me to do something", Transport: telegram.Name}); err != nil {
			return err
		}
		after, err := countLLMCalls(ctx, st)
		if err != nil {
			return err
		}
		callsMade := after - before
		ok := callsMade == 2 && strings.Contains(strings.ToLower(sender.last()), "sorry")
		fmt.Printf("  5  dead api key -> apology    llm_calls +%d  reply=%q  %s\n",
			callsMade, truncate(sender.last(), 60), verdict(ok))
	}

	// deviceZone mirrors Ladder.deviceZone exactly (internal/conversation/
	// prompt.go): kv.current_tz if reportZones (session 3) has set it, else
	// defaultTZ. Scenario 7 needs this to place a probe occurrence inside
	// whatever local day the prompt renderer will actually query - reportZones
	// runs earlier in this program and permanently sets kv.current_tz to
	// Europe/Lisbon the first time naviseed is ever run against a given
	// database, so assuming defaultTZ here would be wrong on every run after
	// the first.
	deviceZone := func() *time.Location {
		if name, ok, _ := st.CurrentTZ(ctx); ok {
			if loc, err := schedule.LoadLocation(name); err == nil {
				return loc
			}
		}
		return defaultTZ
	}

	// 7. Context injection (A3): an active item and a today's occurrence exist
	// before the turn starts, and the system prompt actually sent - captured
	// off the request the fake tier-1 server received, not just the reply -
	// names both, using the schedule's compact summary and the occurrence's
	// status.
	{
		loc := deviceZone()
		probe, err := st.CreateItem(ctx, domain.NewItem{
			Title:    "context injection probe",
			Schedule: json.RawMessage(`{"kind":"fixed","rrule":"FREQ=DAILY","at":"08:15"}`),
			TZ:       loc.String(),
		})
		if err != nil {
			return err
		}
		today := time.Now().In(loc)
		probeStart := time.Date(today.Year(), today.Month(), today.Day(), 8, 15, 0, 0, loc)
		if _, err := st.CreateOccurrence(ctx, domain.NewOccurrence{ItemID: probe.ID, StartsAt: probeStart}); err != nil {
			return err
		}

		var captured []wireRequest
		tier1 := capturingToolCallServer("list_items", `{}`, &captured)
		defer tier1.Close()
		ladder, _ := newLadder(tier1, nil)

		if err := ladder.Handle(ctx, transport.IncomingMessage{SenderID: "111", Text: "what's on my list", Transport: telegram.Name}); err != nil {
			return err
		}

		var prompt string
		if len(captured) > 0 {
			prompt = captured[0].systemContent()
		}
		hasItem := strings.Contains(prompt, probe.ID) &&
			strings.Contains(prompt, `"context injection probe"`) &&
			strings.Contains(prompt, "fixed FREQ=DAILY at 08:15")
		hasOccurrence := strings.Contains(prompt, "context injection probe") && strings.Contains(prompt, "pending")
		ok7 := hasItem && hasOccurrence
		fmt.Printf("  7  context injection           item in prompt=%v  occurrence in prompt=%v  %s\n",
			hasItem, hasOccurrence, verdict(ok7))
	}

	// 8. Last touched (A9): create_item sets kv.last_touched_item, and a
	// second, unrelated turn's system prompt names it - the referent "make it
	// more like five times" resolves against without the model re-naming the
	// item. A fake tier-1 server cannot demonstrate a real model doing that
	// resolution; this proves the plumbing it would resolve against exists.
	{
		createArgs := `{"title":"last touched probe","schedule":{"kind":"fixed","rrule":"FREQ=DAILY","at":"07:00"}}`
		tier1a := toolCallServer("create_item", createArgs)
		defer tier1a.Close()
		ladderA, _ := newLadder(tier1a, nil)
		if err := ladderA.Handle(ctx, transport.IncomingMessage{SenderID: "111", Text: "remind me about the last touched probe daily at 7am", Transport: telegram.Name}); err != nil {
			return err
		}

		items, err := st.ListActiveItems(ctx)
		if err != nil {
			return err
		}
		var probeID string
		for _, it := range items {
			if it.Title == "last touched probe" {
				probeID = it.ID
			}
		}

		storedID, ok, err := st.LastTouchedItemID(ctx)
		if err != nil {
			return err
		}
		okStore := ok && probeID != "" && storedID == probeID

		var captured []wireRequest
		tier1b := capturingToolCallServer("list_items", `{}`, &captured)
		defer tier1b.Close()
		ladderB, _ := newLadder(tier1b, nil)
		if err := ladderB.Handle(ctx, transport.IncomingMessage{SenderID: "111", Text: "what's on my list", Transport: telegram.Name}); err != nil {
			return err
		}
		var prompt string
		if len(captured) > 0 {
			prompt = captured[0].systemContent()
		}
		okPrompt := probeID != "" && strings.Contains(prompt, fmt.Sprintf("Last touched: %s", probeID))

		ok8 := okStore && okPrompt
		fmt.Printf("  8  last touched               kv.last_touched_item=%v  in next prompt=%v  %s\n",
			okStore, okPrompt, verdict(ok8))
	}

	// 9. Inferred parameters reach the confirmation (A5, D-015): a fuzzy
	// schedule naming only count and period gets its window, days_allowed and
	// min_gap_hours filled from defaults.yaml, the write happens without any
	// clarifying question, and the reply states what was assumed - the
	// "periodically through the week" exit criterion.
	{
		args := `{"title":"call my grandmother","schedule":{"kind":"fuzzy","period":"week","count":3}}`
		tier1 := toolCallServer("create_item", args)
		defer tier1.Close()
		ladder, sender := newLadder(tier1, nil)

		if err := ladder.Handle(ctx, transport.IncomingMessage{SenderID: "111", Text: "remind me to call my grandmother periodically through the week", Transport: telegram.Name}); err != nil {
			return err
		}
		reply := sender.last()
		ok9 := strings.Contains(reply, "I assumed") && strings.Contains(reply, "window") &&
			!strings.Contains(reply, "?")
		fmt.Printf("  9  inferred fields confirmed  reply=%q  %s\n", truncate(reply, 90), verdict(ok9))
	}

	// 10. Cross-turn history (A10): a second turn's outgoing request carries
	// the first turn's user message forward, and the current turn's own
	// message - already durably persisted before Handle runs, exactly as the
	// webhook persists it - appears exactly once rather than being duplicated
	// by seedHistory's own load (history.go's drop-the-duplicate rule).
	{
		firstText := "remind me about the history carryover probe daily at 6am"
		if _, _, err := st.CreateConversation(ctx, domain.NewConversation{Role: domain.RoleUser, Content: firstText}); err != nil {
			return err
		}
		args := `{"title":"history carryover probe","schedule":{"kind":"fixed","rrule":"FREQ=DAILY","at":"06:00"}}`
		tier1a := toolCallServer("create_item", args)
		defer tier1a.Close()
		ladderA, _ := newLadder(tier1a, nil)
		if err := ladderA.Handle(ctx, transport.IncomingMessage{SenderID: "111", Text: firstText, Transport: telegram.Name}); err != nil {
			return err
		}

		secondText := "what's on my list"
		if _, _, err := st.CreateConversation(ctx, domain.NewConversation{Role: domain.RoleUser, Content: secondText}); err != nil {
			return err
		}
		var captured []wireRequest
		tier1b := capturingToolCallServer("list_items", `{}`, &captured)
		defer tier1b.Close()
		ladderB, _ := newLadder(tier1b, nil)
		if err := ladderB.Handle(ctx, transport.IncomingMessage{SenderID: "111", Text: secondText, Transport: telegram.Name}); err != nil {
			return err
		}

		var carriedFirst bool
		var secondCount int
		if len(captured) > 0 {
			carriedFirst = captured[0].countUserContent(firstText) == 1
			secondCount = captured[0].countUserContent(secondText)
		}
		ok10 := carriedFirst && secondCount == 1
		fmt.Printf("  10 history carried forward   prior turn present=%v  current turn copies=%d  %s\n",
			carriedFirst, secondCount, verdict(ok10))
	}

	return nil
}

// reportAgentTools drives internal/agent.Tools with hand-written
// json.RawMessage, exactly as a model's tool call would arrive, and checks
// every layer's rejection shape plus the one-transaction guarantee for real.
// No model client exists yet - session 10's whole point is that nothing here
// needs one (docs/06-agent-spec.md#tool-catalog).
//
// It runs after materialization, since it needs a real materializer and
// defaults table, and before fire, so the items and occurrences it writes
// are not noise the fire-path assertions have to account for.
func reportAgentTools(ctx context.Context, st *store.Store, table *defaults.Table, defaultTZ *time.Location, log *slog.Logger) error {
	mat := materializer.New(log.With("component", "materializer"), st, defaultTZ)
	t := agent.New(st, mat, table, defaultTZ)

	fmt.Println("\nagent tool catalog")
	for _, tool := range t.Catalog() {
		fmt.Printf("  %-14s %s\n", tool.Name, tool.Description)
	}

	if err := reportListItems(ctx, t); err != nil {
		return err
	}
	if err := reportCreateItem(ctx, t, st); err != nil {
		return err
	}
	if err := reportLayerRejections(ctx, t); err != nil {
		return err
	}
	if err := reportUpdateScopes(ctx, t, st, mat, defaultTZ); err != nil {
		return err
	}
	return reportDeleteItem(ctx, t, st)
}

// callTool marshals args and drives them through Tools.Call, exactly as a
// model's structured tool call would arrive on the wire.
func callTool(ctx context.Context, t *agent.Tools, name string, args any) (agent.Result, error) {
	raw, err := json.Marshal(args)
	if err != nil {
		return agent.Result{}, err
	}
	return t.Call(ctx, name, raw)
}

// reportListItems is a smoke test: list_items with the default filter
// returns without error over whatever is in the table by this point in the
// run.
func reportListItems(ctx context.Context, t *agent.Tools) error {
	res, err := callTool(ctx, t, "list_items", agent.ListItemsArgs{})
	fmt.Printf("\nagent list_items  %d item(s)  %s\n", len(res.Items), verdict(err == nil))
	return err
}

// reportCreateItem drives create_item's happy path and its rejection path,
// proving two of the session's "done when" bullets: a valid schedule writes
// the item and its occurrences in one transaction and returns three real
// timestamps, and a failing schedule leaves no item and no occurrences
// behind.
func reportCreateItem(ctx context.Context, t *agent.Tools, st *store.Store) error {
	before, err := st.ListActiveItems(ctx)
	if err != nil {
		return err
	}

	res, err := callTool(ctx, t, "create_item", agent.CreateItemArgs{
		Title:    "agent create test",
		Schedule: schedule.Schedule{Kind: schedule.KindFixed, RRule: ptr("FREQ=DAILY"), At: ptr("09:30")},
	})
	if err != nil {
		return fmt.Errorf("naviseed: create_item happy path: %w", err)
	}

	timestampsOK := len(res.NextOccurrences) == 3
	for _, occ := range res.NextOccurrences {
		if _, err := domain.ParseTime(occ.StartsAt); err != nil {
			timestampsOK = false
		}
	}
	fmt.Println("\nagent create_item")
	fmt.Printf("  valid schedule  item %s  next_occurrences %d  %s\n",
		res.Item.ID, len(res.NextOccurrences), verdict(timestampsOK))

	occs, err := st.ListOccurrencesForItem(ctx, res.Item.ID)
	if err != nil {
		return err
	}
	fmt.Printf("    %d occurrence(s) written in the same transaction  %s\n",
		len(occs), verdict(len(occs) >= 3))

	// A schedule that fails Layer 2 (one_off in the past) must leave no item
	// and no occurrences behind: Layer 3 never opens a transaction, because
	// Layer 2 rejects first.
	past := time.Now().AddDate(0, 0, -1).Format("2006-01-02T15:04:05")
	_, err = callTool(ctx, t, "create_item", agent.CreateItemArgs{
		Title:    "agent create bad",
		Schedule: schedule.Schedule{Kind: schedule.KindOneOff, At: ptr(past)},
	})
	var ve *domain.ValidationError
	rejected := errors.As(err, &ve)

	after, err := st.ListActiveItems(ctx)
	if err != nil {
		return err
	}
	fmt.Printf("  failing schedule  rejected=%v rule=%s  active items %d -> %d  %s\n",
		rejected, ruleOf(ve), len(before), len(after),
		verdict(rejected && len(after) == len(before)+1))
	return nil
}

// reportLayerRejections drives one rejection per layer, including the
// unknown-field case Layer 1 owns, and checks each fires the named rule -
// the shape 07-api-spec.md's error body specifies.
func reportLayerRejections(ctx context.Context, t *agent.Tools) error {
	cases := []struct {
		label string
		tool  string
		raw   string
		want  string
	}{
		{
			"layer 1  unknown field",
			"create_item",
			`{"titel":"x","schedule":{"kind":"fixed","rrule":"FREQ=DAILY","at":"09:00"}}`,
			"unknown_field",
		},
		{
			"layer 1  bad enum",
			"list_items",
			`{"filter":"sometimes"}`,
			"enum",
		},
		{
			"layer 2  gap unsatisfiable",
			"create_item",
			`{"title":"bad gap","schedule":{"kind":"fuzzy","period":"day","count":10,"min_gap_hours":8}}`,
			"gap_satisfiable",
		},
		{
			"layer 2  item_id does not resolve",
			"delete_item",
			`{"item_id":"nonexistent","confirmed":true}`,
			"item_exists",
		},
	}

	fmt.Println("\nagent layer rejections")
	for _, c := range cases {
		_, err := t.Call(ctx, c.tool, json.RawMessage(c.raw))
		var ve *domain.ValidationError
		ok := errors.As(err, &ve) && ve.Rule == c.want
		mark := ""
		if !ok {
			mark = "  !!"
		}
		fmt.Printf("  %-26s rule=%-18s field=%-14s %-60s %s%s\n",
			c.label, ruleOf(ve), fieldOf(ve), messageOf(ve), verdict(ok), mark)
	}
	return nil
}

// reportUpdateScopes drives update_item through all three edit scopes plus
// the field-level diff, against one item created for this section alone.
func reportUpdateScopes(ctx context.Context, t *agent.Tools, st *store.Store, mat *materializer.Materializer, defaultTZ *time.Location) error {
	created, err := callTool(ctx, t, "create_item", agent.CreateItemArgs{
		Title:    "agent update scopes",
		Schedule: schedule.Schedule{Kind: schedule.KindFixed, RRule: ptr("FREQ=DAILY"), At: ptr("07:00")},
	})
	if err != nil {
		return err
	}
	itemID := created.Item.ID

	// The zone this item's wall clocks actually resolve against, which is what
	// from_date's boundary means below. It is not the process's zone and it is
	// not defaultTZ: reportZones has already set kv.current_tz to Europe/Lisbon
	// by the time this runs, so a floating item resolves there.
	loc, err := itemZone(ctx, st, *created.Item, defaultTZ)
	if err != nil {
		return err
	}

	fmt.Println("\nagent update_item scopes")

	// future_all: every future pending row changes.
	beforeIDs, err := pendingIDs(ctx, st, itemID)
	if err != nil {
		return err
	}
	res, err := callTool(ctx, t, "update_item", agent.UpdateItemArgs{
		ItemID: itemID,
		Scope:  agent.ScopeFutureAll,
		Changes: agent.ItemChanges{
			Schedule: &schedule.Schedule{Kind: schedule.KindFixed, RRule: ptr("FREQ=DAILY"), At: ptr("20:00")},
		},
	})
	if err != nil {
		return err
	}
	afterIDs, err := pendingIDs(ctx, st, itemID)
	if err != nil {
		return err
	}
	fmt.Printf("  future_all  deleted=%d inserted=%d  every id changed=%v  %s\n",
		res.Applied.Deleted, res.Applied.Inserted, disjoint(beforeIDs, afterIDs),
		verdict(res.Applied.Deleted > 0 && res.Applied.Inserted > 0 && disjoint(beforeIDs, afterIDs)))

	// from_date: rows before the date are untouched, rows after change.
	//
	// The boundary is a local date in the item's own zone, not an instant in
	// this process's: the server floors to midnight of from_date resolved
	// through loc (internal/agent/execute.go's ParseInLocation), and from_date
	// is inclusive of its whole target date (05-schedule-spec.md#edit-scope).
	// Classifying rows with StartsAt.After(a time.Now()-derived instant) asks a
	// different question in a different zone and gets a different answer - it
	// mis-files the boundary day's own row as "before" whenever the current
	// UTC time-of-day is past that row's, which is what made this check fail
	// every afternoon.
	fromDate := time.Now().In(loc).AddDate(0, 0, 7).Format(domain.DateLayout)
	beforeBoundary := func(occ domain.Occurrence) bool {
		return occ.StartsAt.In(loc).Format(domain.DateLayout) < fromDate
	}
	beforeRows, err := pendingRows(ctx, st, itemID)
	if err != nil {
		return err
	}
	_, err = callTool(ctx, t, "update_item", agent.UpdateItemArgs{
		ItemID:   itemID,
		Scope:    agent.ScopeFromDate,
		FromDate: ptr(fromDate),
		Changes: agent.ItemChanges{
			Schedule: &schedule.Schedule{Kind: schedule.KindFixed, RRule: ptr("FREQ=DAILY"), At: ptr("21:15")},
		},
	})
	if err != nil {
		return err
	}
	afterRows, err := pendingRows(ctx, st, itemID)
	if err != nil {
		return err
	}
	beforeUntouched := true
	for _, occ := range beforeRows {
		if !beforeBoundary(occ) {
			continue
		}
		if !stillPresent(afterRows, occ) {
			beforeUntouched = false
		}
	}
	changedAfter := false
	for _, occ := range afterRows {
		if !beforeBoundary(occ) && !stillPresent(beforeRows, occ) {
			changedAfter = true
		}
	}
	fmt.Printf("  from_date   %s  rows before untouched=%v  rows after changed=%v  %s\n",
		fromDate, beforeUntouched, changedAfter,
		verdict(beforeUntouched && changedAfter))

	// single: retime one occurrence, mark is_override, survive a full
	// nightly pass.
	rows, err := pendingRows(ctx, st, itemID)
	if err != nil {
		return err
	}
	if len(rows) == 0 {
		return fmt.Errorf("naviseed: update_item single: no pending occurrence to retime")
	}
	target := rows[0]
	retimeAt := time.Now().Add(48 * time.Hour).Format("2006-01-02T15:04:05")
	_, err = callTool(ctx, t, "update_item", agent.UpdateItemArgs{
		ItemID:       itemID,
		Scope:        agent.ScopeSingle,
		OccurrenceID: ptr(target.ID),
		Changes: agent.ItemChanges{
			Schedule: &schedule.Schedule{Kind: schedule.KindOneOff, At: ptr(retimeAt)},
		},
	})
	if err != nil {
		return err
	}
	retimed, err := st.GetOccurrence(ctx, target.ID)
	if err != nil {
		return err
	}
	if _, err := mat.All(ctx); err != nil {
		return err
	}
	survived, err := st.GetOccurrence(ctx, target.ID)
	if err != nil {
		return err
	}
	fmt.Printf("  single      occurrence %s  is_override=%v  survives a materializer run=%v  %s\n",
		target.ID, retimed.IsOverride, survived.StartsAt.Equal(retimed.StartsAt),
		verdict(retimed.IsOverride && survived.StartsAt.Equal(retimed.StartsAt)))

	// title-only: not one occurrence row is touched.
	before2, err := pendingRows(ctx, st, itemID)
	if err != nil {
		return err
	}
	_, err = callTool(ctx, t, "update_item", agent.UpdateItemArgs{
		ItemID:  itemID,
		Changes: agent.ItemChanges{Title: ptr("agent update scopes (renamed)")},
	})
	if err != nil {
		return err
	}
	after2, err := pendingRows(ctx, st, itemID)
	if err != nil {
		return err
	}
	fmt.Printf("  title-only  occurrence set unchanged=%v  %s\n",
		sameRows(before2, after2), verdict(sameRows(before2, after2)))
	return nil
}

// reportDeleteItem drives delete_item's confirmation gate and its A7
// mechanics: nothing is written without confirmed=true, and archiving
// preserves resolved history.
func reportDeleteItem(ctx context.Context, t *agent.Tools, st *store.Store) error {
	created, err := callTool(ctx, t, "create_item", agent.CreateItemArgs{
		Title:    "agent delete test",
		Schedule: schedule.Schedule{Kind: schedule.KindFixed, RRule: ptr("FREQ=DAILY"), At: ptr("12:00")},
	})
	if err != nil {
		return err
	}
	itemID := created.Item.ID

	// Seed one resolved occurrence so history survival is a real check
	// rather than an empty set trivially "surviving."
	history, err := st.CreateOccurrence(ctx, domain.NewOccurrence{
		ItemID:   itemID,
		StartsAt: time.Now().Add(-time.Hour),
		Status:   ptr(domain.StatusCompleted),
	})
	if err != nil {
		return err
	}

	fmt.Println("\nagent delete_item")

	_, err = callTool(ctx, t, "delete_item", agent.DeleteItemArgs{ItemID: itemID, Confirmed: false})
	var ve *domain.ValidationError
	rejected := errors.As(err, &ve)
	stillActive, err := st.GetItem(ctx, itemID)
	if err != nil {
		return err
	}
	fmt.Printf("  unconfirmed  rejected=%v rule=%s  archived_at %s  %s\n",
		rejected, ruleOf(ve), presence(stillActive.ArchivedAt != nil),
		verdict(rejected && stillActive.ArchivedAt == nil))

	_, err = callTool(ctx, t, "delete_item", agent.DeleteItemArgs{ItemID: itemID, Confirmed: true})
	if err != nil {
		return err
	}
	archived, err := st.GetItem(ctx, itemID)
	if err != nil {
		return err
	}
	pending, err := pendingRows(ctx, st, itemID)
	if err != nil {
		return err
	}
	survivor, err := st.GetOccurrence(ctx, history.ID)
	if err != nil {
		return err
	}
	fmt.Printf("  confirmed    archived_at %s  pending remaining=%d  history status=%s  %s\n",
		presence(archived.ArchivedAt != nil), len(pending), survivor.Status,
		verdict(archived.ArchivedAt != nil && len(pending) == 0 && survivor.Status == domain.StatusCompleted))
	return nil
}

func ruleOf(ve *domain.ValidationError) string {
	if ve == nil {
		return ""
	}
	return ve.Rule
}

func fieldOf(ve *domain.ValidationError) string {
	if ve == nil {
		return ""
	}
	return ve.Field
}

func messageOf(ve *domain.ValidationError) string {
	if ve == nil {
		return ""
	}
	return ve.Message
}

func pendingRows(ctx context.Context, st *store.Store, itemID string) ([]domain.Occurrence, error) {
	rows, err := st.ListOccurrencesForItem(ctx, itemID)
	if err != nil {
		return nil, err
	}
	out := make([]domain.Occurrence, 0, len(rows))
	for _, occ := range rows {
		if occ.Status == domain.StatusPending {
			out = append(out, occ)
		}
	}
	return out, nil
}

func pendingIDs(ctx context.Context, st *store.Store, itemID string) (map[string]bool, error) {
	rows, err := pendingRows(ctx, st, itemID)
	if err != nil {
		return nil, err
	}
	ids := make(map[string]bool, len(rows))
	for _, occ := range rows {
		ids[occ.ID] = true
	}
	return ids, nil
}

// disjoint reports whether no id in after also appears in before - the
// "every id changed" check for future_all, which deletes and reinserts
// rather than updating in place.
func disjoint(before, after map[string]bool) bool {
	if len(after) == 0 {
		return false
	}
	for id := range after {
		if before[id] {
			return false
		}
	}
	return true
}

func stillPresent(rows []domain.Occurrence, target domain.Occurrence) bool {
	for _, occ := range rows {
		if occ.ID == target.ID && occ.StartsAt.Equal(target.StartsAt) {
			return true
		}
	}
	return false
}

func sameRows(a, b []domain.Occurrence) bool {
	if len(a) != len(b) {
		return false
	}
	idx := make(map[string]time.Time, len(a))
	for _, occ := range a {
		idx[occ.ID] = occ.StartsAt
	}
	for _, occ := range b {
		want, ok := idx[occ.ID]
		if !ok || !want.Equal(occ.StartsAt) {
			return false
		}
	}
	return true
}

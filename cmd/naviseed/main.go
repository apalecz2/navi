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
	"time"

	// Same reason as cmd/navi: the schedule below resolves against an IANA
	// zone, and a scratch image has no /usr/share/zoneinfo.
	_ "time/tzdata"

	"github.com/aidenpaleczny/navi/internal/config"
	"github.com/aidenpaleczny/navi/internal/defaults"
	"github.com/aidenpaleczny/navi/internal/domain"
	"github.com/aidenpaleczny/navi/internal/schedule"
	"github.com/aidenpaleczny/navi/internal/store"
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
	return reportZones(ctx, st, item, cfg.Schedule.DefaultTZ)
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
		fmt.Printf("\n%d occurrence(s) already present, not adding more\n", len(existing))
		for _, occ := range existing {
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
	overdue, err := st.PendingOverdue(ctx)
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

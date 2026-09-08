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
	"github.com/aidenpaleczny/navi/internal/briefing"
	"github.com/aidenpaleczny/navi/internal/config"
	"github.com/aidenpaleczny/navi/internal/conversation"
	"github.com/aidenpaleczny/navi/internal/defaults"
	"github.com/aidenpaleczny/navi/internal/domain"
	"github.com/aidenpaleczny/navi/internal/health"
	"github.com/aidenpaleczny/navi/internal/httpapi"
	"github.com/aidenpaleczny/navi/internal/materializer"
	"github.com/aidenpaleczny/navi/internal/metrics"
	"github.com/aidenpaleczny/navi/internal/model"
	"github.com/aidenpaleczny/navi/internal/reconciler"
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

	// Beside resolve, and after it: snooze reaches notified rows the same way,
	// and the chain it builds is what gives the resolution path something to
	// roll up.
	if err := reportSnooze(ctx, st, cfg.Schedule.DefaultTZ.String(), cfg.Schedule.DefaultTZ, log); err != nil {
		return err
	}

	// After both endpoints, because a button tap reaches the same two store
	// methods they do and there is no point checking the wiring before the
	// thing it is wired to.
	if err := reportCallback(ctx, st, cfg.Schedule.DefaultTZ.String(), cfg.Schedule.DefaultTZ, log); err != nil {
		return err
	}

	// Beside it, and last of the resolution sections: bulk_resolve is the third
	// surface, and the only one that can resolve an occurrence that has not
	// fired yet.
	if err := reportBulkResolve(ctx, st, table, cfg.Schedule.DefaultTZ.String(), cfg.Schedule.DefaultTZ, log); err != nil {
		return err
	}

	// After every resolution surface, because the check-in is about what none of
	// them resolved: it needs a silent row nothing fired, a notified row nothing
	// answered, and a resolved row to be absent, and only the sections above can
	// leave the second and third in an honest state.
	if err := reportReconcile(ctx, st, cfg.Schedule.DefaultTZ.String(), cfg.Schedule.DefaultTZ, log); err != nil {
		return err
	}

	// Beside it: the composed check-in and, more to the point, its fallback.
	// Separate from reportReconcile because that section asserts what a pass
	// covers and this one asserts what it says, and the only way to check the
	// second is to break a real model client on purpose.
	if err := reportReconcileComposer(ctx, st, cfg.Schedule.DefaultTZ.String(), cfg.Schedule.DefaultTZ, log); err != nil {
		return err
	}

	// Then the reply to it, which is the other half of D-009: reconciliation
	// and unprompted batch completion are one code path, so answering a
	// check-in is bulk_resolve reached through the ordinary agent turn.
	if err := reportReconcileReply(ctx, st, table, cfg.Schedule.DefaultTZ.String(), cfg.Schedule.DefaultTZ, log); err != nil {
		return err
	}

	// And last, the conclusion: grace, then missed. It runs after the reply
	// section on purpose, because one of the things it has to show is that an
	// answer arriving inside the window prevents the miss - which needs an
	// answer to have arrived.
	if err := reportGrace(ctx, st, cfg.Schedule.DefaultTZ.String(), cfg.Schedule.DefaultTZ, log); err != nil {
		return err
	}

	// Beside it: pausing is the other half of P3's first session, and the
	// check-in honouring a pause is asserted inside reportReconcile rather than
	// here, because it is a property of the check-in and not of the endpoint.
	if err := reportPauseEndpoints(ctx, st, cfg.Schedule.DefaultTZ.String(), cfg.Schedule.DefaultTZ, log); err != nil {
		return err
	}

	// P3.5 goals: item-linked progress read live from chains, freestanding
	// progress as the newest goal_updates row, the validation table, and the
	// sweeper's period-end pass. After every occurrence section, since the
	// item-linked check completes real occurrences and the past-period checks
	// hand-insert some.
	if err := reportGoals(ctx, st, table, cfg.Schedule.DefaultTZ.String(), cfg.Schedule.DefaultTZ, log); err != nil {
		return err
	}

	// The morning briefing: the last of P3.5. After goals, because its context
	// blob reads the same goal progress, and after the reconciler sections,
	// because it reuses their grace-window shape and their recording transport.
	if err := reportBriefing(ctx, st, cfg.Schedule.DefaultTZ.String(), cfg.Schedule.DefaultTZ, log); err != nil {
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
	return reportBackfill(ctx, st, mat, defaultTZ, log)
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
func reportBackfill(ctx context.Context, st *store.Store, mat *materializer.Materializer, defaultTZ *time.Location, log *slog.Logger) error {
	short := time.Now().AddDate(0, 0, sweeper.MinHorizonDays-1)
	if err := st.SetLastMaterializedThrough(ctx, short); err != nil {
		return err
	}

	was, _, err := st.Horizon(ctx)
	if err != nil {
		return err
	}

	sw := sweeper.New(log.With("component", "sweeper"), st, mat, defaultTZ)
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
	// nil resolver and nil api: this section is the message half, and a
	// callback query with nothing wired to resolve it is acknowledged and
	// ignored — the state every build before session 15 was in. The tap half is
	// reportCallback's.
	h := telegram.NewInbound(secret, allowedID, st, nil, nil, disp, m, nil,
		log.With("component", "webhook"))

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
	hFull := telegram.NewInbound(secret, allowedID, st, nil, nil, full, m, nil,
		log.With("component", "webhook"))
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
	loc, err := schedule.LoadLocation(tz)
	if err != nil {
		return err
	}
	srv := httpapi.New(config.HTTP{Addr: ":0"}, log.With("component", "httpapi"),
		health.New(), m, st, materializer.New(log.With("component", "mat-http"), st, loc),
		time.Time{}, loc, nil)

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

// snoozeBody is the endpoint's response, decoded far enough to assert on. It
// is spelled out here rather than imported because internal/httpapi's own
// response types are unexported, which is correct — a wire shape a second
// package can reach is a wire shape that stops being the handler's to change.
type snoozeBody struct {
	Occurrence occurrenceBody `json:"occurrence"`
	Parent     occurrenceBody `json:"parent"`
	Chain      chainBody      `json:"chain"`
}

type occurrenceBody struct {
	ID                 string  `json:"id"`
	StartsAt           string  `json:"starts_at"`
	Status             string  `json:"status"`
	IsOverride         bool    `json:"is_override"`
	ParentOccurrenceID *string `json:"parent_occurrence_id"`
	SnoozeDepth        int     `json:"snooze_depth"`
	ResolvedAt         *string `json:"resolved_at"`
	ResolutionSource   *string `json:"resolution_source"`
}

type chainBody struct {
	RootID       string  `json:"root_id"`
	ScheduledAt  string  `json:"scheduled_at"`
	SnoozeCount  int     `json:"snooze_count"`
	WasCompleted bool    `json:"was_completed"`
	CompletedAt  *string `json:"completed_at"`
}

// reportSnooze drives POST /api/occurrences/{id}/snooze through a real httpapi
// server, the way reportResolve drives its sibling, and then checks the four
// delta presets one layer down against schedule.ResolveDelta directly.
//
// The split is deliberate. Everything that depends on a row — R6's untouched
// starts_at, the child's flags, the cap, the chain — has to go through the
// endpoint, because the point is that the wiring is right. Everything that
// depends on what time it is cannot: "tonight after 19:00" and "tomorrow across
// a spring-forward boundary" are questions about a specific now, and driving
// them through an HTTP handler that reads the wall clock would make them pass
// or fail depending on when this ran. The pure function takes its now as an
// argument, so both branches are checkable every time. That is the same lesson
// session 13's from_date fix learned the expensive way.
func reportSnooze(ctx context.Context, st *store.Store, tz string, fallback *time.Location, log *slog.Logger) error {
	fmt.Println("\nsnooze  POST /api/occurrences/{id}/snooze")

	m := metrics.New()
	srv := httpapi.New(config.HTTP{Addr: ":0"}, log.With("component", "httpapi"),
		health.New(), m, st, materializer.New(log.With("component", "mat-http"), st, fallback),
		time.Time{}, fallback, nil)

	post := func(id, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/api/occurrences/"+id+"/snooze",
			strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		srv.Handler.ServeHTTP(rec, req.WithContext(ctx))
		return rec
	}
	decode := func(rec *httptest.ResponseRecorder) snoozeBody {
		var out snoozeBody
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return out
	}

	item, err := seedFireItem(ctx, st, "snooze path probe", domain.NotifyAtTime, tz)
	if err != nil {
		return err
	}

	// A notified row, reached the way the scheduler reaches one. at is how far
	// in the past the row starts: a claimable row has to be behind now, and
	// walking a chain to its cap means every link has to be claimable in turn.
	claim := func(title string, at time.Time) (domain.Occurrence, error) {
		occ, err := fireOccurrence(ctx, st, item, at, ptr(title))
		if err != nil {
			return domain.Occurrence{}, err
		}
		if err := fireAll(ctx, st, m, log); err != nil {
			return domain.Occurrence{}, err
		}
		return st.GetOccurrence(ctx, occ.ID)
	}

	// 1. R6: the original keeps its timestamp and a child appears at the
	// resolved time. 1h is the delta on purpose — it is instant arithmetic, so
	// this assertion says nothing about zones and cannot be broken by which one
	// kv.current_tz happens to hold by the time this section runs.
	occ, err := claim("snooze probe body", time.Now().Add(-time.Minute))
	if err != nil {
		return err
	}
	if occ.Status != domain.StatusNotified {
		return fmt.Errorf("naviseed: snooze: occurrence %s is %s, not notified", occ.ID, occ.Status)
	}
	was := occ.StartsAt

	t0 := time.Now()
	applied := post(occ.ID, `{"delta":"1h","source":"web"}`)
	body := decode(applied)

	parent, err := st.GetOccurrence(ctx, occ.ID)
	if err != nil {
		return err
	}
	child, err := st.GetOccurrence(ctx, body.Occurrence.ID)
	if err != nil {
		return err
	}

	untouched := parent.StartsAt.Equal(was)
	fmt.Printf("  notified -> snoozed     status %d  starts_at %s unchanged=%v  resolved_at=%s source=%s  %s\n",
		applied.Code, domain.FormatTime(was), untouched,
		presence(parent.ResolvedAt != nil), sourceOf(parent.ResolutionSource),
		verdict(applied.Code == http.StatusOK && untouched &&
			parent.Status == domain.StatusSnoozed && parent.ResolvedAt != nil &&
			parent.ResolutionSource != nil && *parent.ResolutionSource == domain.ResolvedByWeb))

	// The child, and the hour it was pushed by. Bounded rather than compared
	// exactly, because the handler reads its own clock a moment after t0.
	offset := child.StartsAt.Sub(t0)
	childOK := child.Status == domain.StatusPending && child.IsOverride &&
		child.ParentOccurrenceID != nil && *child.ParentOccurrenceID == occ.ID &&
		child.SnoozeDepth == 1 &&
		offset >= time.Hour-5*time.Second && offset <= time.Hour+5*time.Second
	fmt.Printf("    child                 %s  is_override=%v parent=%s depth=%d  +%s  %s\n",
		child.Status, child.IsOverride, presence(child.ParentOccurrenceID != nil),
		child.SnoozeDepth, offset.Round(time.Second), verdict(childOK))

	// 2. A second tap on the same row. snoozed is terminal, so this is the
	// idempotency table's "already in the requested terminal state": 200,
	// nothing written, and crucially the same child rather than a second one.
	again := post(occ.ID, `{"delta":"1h","source":"web"}`)
	againBody := decode(again)
	all, err := st.ListOccurrencesForItem(ctx, item.ID)
	if err != nil {
		return err
	}
	children := 0
	for _, row := range all {
		if row.ParentOccurrenceID != nil && *row.ParentOccurrenceID == occ.ID {
			children++
		}
	}
	fmt.Printf("    snoozed -> snoozed    status %d  same child=%v  children=%d  %s\n",
		again.Code, againBody.Occurrence.ID == body.Occurrence.ID, children,
		verdict(again.Code == http.StatusOK &&
			againBody.Occurrence.ID == body.Occurrence.ID && children == 1))

	// 3. The scheduler never sees a snoozed row again. Both ListDueOccurrences
	// and ClaimOccurrence filter status = 'pending', so this falls out by
	// construction — drive it rather than assert it.
	rec := &recordingTransport{}
	sched := scheduler.New(log.With("component", "scheduler-snooze"), st, rec, m, time.Now())
	if _, err := sched.Fire(ctx); err != nil {
		return err
	}
	stillSnoozed, err := statusOf(ctx, st, occ.ID)
	if err != nil {
		return err
	}
	fmt.Printf("    original not re-fired  %s, %d send(s)  %s\n",
		stillSnoozed, rec.count("snooze probe body"),
		verdict(stillSnoozed == domain.StatusSnoozed && rec.count("snooze probe body") == 0))

	// 4. The child survives materialization. is_override is the whole
	// mechanism, guarded in the planner and again in the delete's WHERE clause;
	// this is the check that both are actually in force.
	if err := reportSnoozeSurvival(ctx, st, item, child, fallback, log); err != nil {
		return err
	}

	// 5. The chain, completed through its child (D-011, R7).
	if err := reportChainRollup(ctx, st, post, decode, claim); err != nil {
		return err
	}

	// 6. The cap, and the missed it resolves to (R8).
	if err := reportSnoozeCap(ctx, st, m, post, claim, log); err != nil {
		return err
	}

	// The two edges this endpoint can produce, labelled by the surface.
	metricsRec := httptest.NewRecorder()
	m.Handler().ServeHTTP(metricsRec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	for _, want := range []string{
		`navi_occurrence_transitions_total{from="notified",source="web",to="snoozed"}`,
		`navi_occurrence_transitions_total{from="notified",source="web",to="missed"} 1`,
	} {
		fmt.Printf("  transitions metric      %s  %s\n",
			want, verdict(strings.Contains(metricsRec.Body.String(), want)))
	}

	// A body the schema would reject never reaches the store, and source is
	// required even though 07-api-spec's example omits it.
	badDelta := post(occ.ID, `{"delta":"in a bit","source":"web"}`)
	noSource := post(occ.ID, `{"delta":"1h"}`)
	missing := post("01ARZ3NDEKTSV4RRFFQ69G5FAV", `{"delta":"1h","source":"web"}`)
	fmt.Printf("  delta not in the enum   status %d  %s\n",
		badDelta.Code, verdict(badDelta.Code == http.StatusBadRequest))
	fmt.Printf("  source omitted          status %d  %s\n",
		noSource.Code, verdict(noSource.Code == http.StatusBadRequest))
	fmt.Printf("  unknown occurrence      status %d  %s\n",
		missing.Code, verdict(missing.Code == http.StatusNotFound))

	reportSnoozeDeltas()
	return nil
}

// fakeTelegram stands in for the Bot API so the calls an adapter makes are
// observable rather than assumed.
//
// It is what makes N6 checkable at all: "the message updates in place, leaving
// one message in the chat rather than two" is a claim about which methods were
// called and in what order, and there is nowhere else to read that from.
type fakeTelegram struct {
	srv *httptest.Server

	mu    sync.Mutex
	calls []apiCall

	// editFails makes editMessageText return the API's own refusal, which is
	// the live half of N6's fallback — a message too old or since deleted.
	editFails bool

	nextMessageID int
}

type apiCall struct {
	Method string
	Body   map[string]any
}

func newFakeTelegram() *fakeTelegram {
	f := &fakeTelegram{nextMessageID: 5000}
	f.srv = httptest.NewServer(http.HandlerFunc(f.handle))
	return f
}

func (f *fakeTelegram) handle(w http.ResponseWriter, r *http.Request) {
	// /bot<token>/<method>
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	method := parts[len(parts)-1]

	var body map[string]any
	raw, _ := io.ReadAll(r.Body)
	_ = json.Unmarshal(raw, &body)

	f.mu.Lock()
	f.calls = append(f.calls, apiCall{Method: method, Body: body})
	id := f.nextMessageID
	f.nextMessageID++
	fails := f.editFails
	f.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	if fails && method == "editMessageText" {
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `{"ok":false,"error_code":400,"description":"message to edit not found"}`)
		return
	}
	fmt.Fprintf(w, `{"ok":true,"result":{"message_id":%d}}`, id)
}

func (f *fakeTelegram) URL() string { return f.srv.URL }
func (f *fakeTelegram) Close()      { f.srv.Close() }

func (f *fakeTelegram) reset() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = nil
}

// methods is the call sequence, which is the assertion for "answerCallbackQuery
// then editMessageText, in that order".
func (f *fakeTelegram) methods() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.calls))
	for i, c := range f.calls {
		out[i] = c.Method
	}
	return out
}

func (f *fakeTelegram) count(method string) int {
	n := 0
	for _, m := range f.methods() {
		if m == method {
			n++
		}
	}
	return n
}

// last returns the most recent body for a method, or nil.
func (f *fakeTelegram) last(method string) map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := len(f.calls) - 1; i >= 0; i-- {
		if f.calls[i].Method == method {
			return f.calls[i].Body
		}
	}
	return nil
}

// button is one decoded inline keyboard button.
type button struct {
	Text string
	Data string
}

// keyboardOf pulls reply_markup out of a recorded body. A nil result means the
// message carried no keyboard, which is itself an assertion in two places: the
// edit that folds an outcome in must drop it.
func keyboardOf(body map[string]any) [][]button {
	markup, ok := body["reply_markup"].(map[string]any)
	if !ok {
		return nil
	}
	rows, ok := markup["inline_keyboard"].([]any)
	if !ok {
		return nil
	}
	out := make([][]button, 0, len(rows))
	for _, r := range rows {
		cells, ok := r.([]any)
		if !ok {
			continue
		}
		row := make([]button, 0, len(cells))
		for _, c := range cells {
			b, ok := c.(map[string]any)
			if !ok {
				continue
			}
			text, _ := b["text"].(string)
			data, _ := b["callback_data"].(string)
			row = append(row, button{Text: text, Data: data})
		}
		out = append(out, row)
	}
	return out
}

// str reads a string field that may have been omitted.
func str(body map[string]any, key string) string {
	if body == nil {
		return ""
	}
	s, _ := body[key].(string)
	return s
}

// reportCallback drives the Telegram callback path end to end: a real
// scheduler pass through the real adapter renders the keyboard, and synthetic
// callback_query updates go through the real Inbound handler against a fake Bot
// API.
//
// It is the P2 exit criteria as assertions. Nothing here writes a status by
// hand or calls a store method the adapter would not have called — the whole
// question is whether a tap reaches the same state machine everything else
// does, and a shortcut anywhere in the middle would stop answering it.
func reportCallback(ctx context.Context, st *store.Store, tz string, fallback *time.Location, log *slog.Logger) error {
	fmt.Println("\ncallback  POST /webhook/telegram (button taps)")

	const (
		secret    = "naviseed-callback-secret"
		allowedID = "111"
	)

	m := metrics.New()
	m.RegisterInboundDropped("callback_decode")

	api := newFakeTelegram()
	defer api.Close()

	tg := telegram.New("naviseed-token", allowedID, telegram.WithAPIBase(api.URL()))
	h := telegram.NewInbound(secret, allowedID, st, st, tg, nil, m, fallback,
		log.With("component", "callback"))

	updateID := 9100
	tap := func(data string, messageID int, text string) *httptest.ResponseRecorder {
		updateID++
		body := fmt.Sprintf(
			`{"update_id":%d,"callback_query":{"id":"cb-%d","from":{"id":%s},"data":%q,`+
				`"message":{"message_id":%d,"chat":{"id":%s},"text":%q}}}`,
			updateID, updateID, allowedID, data, messageID, allowedID, text)

		req := httptest.NewRequest(http.MethodPost, "/webhook/telegram", strings.NewReader(body))
		req.Header.Set("X-Telegram-Bot-Api-Secret-Token", secret)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req.WithContext(ctx))
		return rec
	}

	item, err := seedFireItem(ctx, st, "callback path probe", domain.NotifyAtTime, tz)
	if err != nil {
		return err
	}

	// notify sends one occurrence through the real fire path with the real
	// adapter attached, so the keyboard under assertion is the one a reminder
	// actually carries — not one this file built to match.
	notify := func(title string) (domain.Occurrence, error) {
		occ, err := fireOccurrence(ctx, st, item, time.Now().Add(-time.Minute), ptr(title))
		if err != nil {
			return domain.Occurrence{}, err
		}
		sched := scheduler.New(log.With("component", "scheduler-callback"), st, tg, m, time.Now())
		if _, err := sched.Fire(ctx); err != nil {
			return domain.Occurrence{}, err
		}
		return st.GetOccurrence(ctx, occ.ID)
	}

	// 1. supports_actions is true and the keyboard is real. The callback_data
	// budget is the thing worth measuring: 64 bytes is the whole reason
	// 07-api-spec declines to build a signed-token scheme here.
	api.reset()
	occ, err := notify("callback probe body")
	if err != nil {
		return err
	}
	sent := api.last("sendMessage")
	kb := keyboardOf(sent)

	rendered := len(kb) == 1 && len(kb[0]) == 3
	longest := 0
	data := make([]string, 0, 3)
	if rendered {
		for _, b := range kb[0] {
			data = append(data, b.Data)
			if len(b.Data) > longest {
				longest = len(b.Data)
			}
		}
	}
	keyboardOK := rendered &&
		data[0] == "n1:complete:"+occ.ID &&
		data[1] == "n1:menu:"+occ.ID &&
		data[2] == "n1:skip:"+occ.ID &&
		longest <= 64
	fmt.Printf("  keyboard on the reminder  %d row(s), %d button(s), longest callback_data %d/64 bytes  %s\n",
		len(kb), len(kb[0]), longest, verdict(keyboardOK))

	// 2. One tap resolves it, and the message is edited rather than followed by
	// a second one — the first two exit criteria, and they are one assertion
	// because "in place" is exactly "no sendMessage beside the edit".
	api.reset()
	done := tap("n1:complete:"+occ.ID, 5001, "callback probe body")
	after, err := st.GetOccurrence(ctx, occ.ID)
	if err != nil {
		return err
	}
	edit := api.last("editMessageText")
	seq := api.methods()
	oneTapOK := done.Code == http.StatusOK &&
		after.Status == domain.StatusCompleted &&
		after.ResolvedAt != nil &&
		after.ResolutionSource != nil && *after.ResolutionSource == domain.ResolvedByNotification &&
		len(seq) == 2 && seq[0] == "answerCallbackQuery" && seq[1] == "editMessageText" &&
		api.count("sendMessage") == 0 &&
		keyboardOf(edit) == nil
	fmt.Printf("  Done resolves in one tap  status %d  %s  source=%s  calls=%v  keyboard dropped %v  %s\n",
		done.Code, after.Status, sourceOf(after.ResolutionSource), seq,
		keyboardOf(edit) == nil, verdict(oneTapOK))
	fmt.Printf("    toast %q  message %q\n",
		str(api.last("answerCallbackQuery"), "text"), str(edit, "text"))

	// 3. The double tap. Same terminal state again is the idempotency table's
	// second row: 200, nothing written, and the metric still reads one.
	firstResolvedAt := domain.FormatTime(*after.ResolvedAt)
	api.reset()
	twice := tap("n1:complete:"+occ.ID, 5001, "callback probe body")
	again, err := st.GetOccurrence(ctx, occ.ID)
	if err != nil {
		return err
	}
	unchanged := domain.FormatTime(*again.ResolvedAt) == firstResolvedAt
	fmt.Printf("  double tap does not double-record  status %d  resolved_at unchanged %v  toast %q  %s\n",
		twice.Code, unchanged, str(api.last("answerCallbackQuery"), "text"),
		verdict(twice.Code == http.StatusOK && unchanged))

	// 4. The two-stage snooze keyboard. menu writes nothing at all — that is
	// the property that makes a non-resolving action safe to put on the same
	// keyboard as three resolving ones.
	snoozed, err := notify("callback snooze body")
	if err != nil {
		return err
	}
	api.reset()
	menu := tap("n1:menu:"+snoozed.ID, 5002, "callback snooze body")
	stillNotified, err := statusOf(ctx, st, snoozed.ID)
	if err != nil {
		return err
	}
	presets := keyboardOf(api.last("editMessageReplyMarkup"))
	menuOK := menu.Code == http.StatusOK &&
		stillNotified == domain.StatusNotified &&
		api.count("editMessageText") == 0 &&
		len(presets) == 2 && len(presets[0]) == len(schedule.Deltas) && len(presets[1]) == 1
	labels := make([]string, 0, len(schedule.Deltas))
	if len(presets) > 0 {
		for _, b := range presets[0] {
			labels = append(labels, b.Text)
		}
	}
	fmt.Printf("  Snooze opens the presets  status %d  occurrence still %s  presets %v  %s\n",
		menu.Code, stillNotified, labels, verdict(menuOK))

	api.reset()
	pushed := tap("n1:snooze:"+snoozed.ID+":1h", 5002, "callback snooze body")
	parent, err := st.GetOccurrence(ctx, snoozed.ID)
	if err != nil {
		return err
	}
	chain, err := st.ChainFor(ctx, snoozed.ID)
	if err != nil {
		return err
	}
	snoozeOK := pushed.Code == http.StatusOK &&
		parent.Status == domain.StatusSnoozed &&
		parent.StartsAt.Equal(snoozed.StartsAt) &&
		parent.ResolutionSource != nil && *parent.ResolutionSource == domain.ResolvedByNotification &&
		chain.SnoozeCount == 1
	fmt.Printf("  a preset snoozes it       status %d  parent %s  starts_at unmoved %v  chain snooze_count=%d  %s\n",
		pushed.Code, parent.Status, parent.StartsAt.Equal(snoozed.StartsAt), chain.SnoozeCount,
		verdict(snoozeOK))

	// The clock in the toast is the device zone, which reportZones has already
	// set to Europe/Lisbon by the time this runs — not the deployment default.
	// That is the intended behaviour and it is worth naming, because a reader
	// comparing the number against their own wall clock would otherwise think
	// it wrong: the item's zone resolves the delta, the device's reports it.
	deviceZone := fallback.String()
	if name, ok, err := st.CurrentTZ(ctx); err == nil && ok {
		deviceZone = name
	}
	fmt.Printf("    toast %q  message %q  (clock in %s, the device zone)\n",
		str(api.last("answerCallbackQuery"), "text"), str(api.last("editMessageText"), "text"),
		deviceZone)

	if err := reportCallbackCap(ctx, st, m, api, tap, notify, log); err != nil {
		return err
	}
	if err := reportCallbackMalformed(ctx, st, m, api, tap, occ.ID); err != nil {
		return err
	}
	if err := reportCallbackEditFallback(api, tap, notify); err != nil {
		return err
	}

	// resolution_source = notification, reachable for the first time.
	metricsRec := httptest.NewRecorder()
	m.Handler().ServeHTTP(metricsRec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	for _, want := range []string{
		`navi_occurrence_transitions_total{from="notified",source="notification",to="completed"} 1`,
		`navi_occurrence_transitions_total{from="notified",source="notification",to="snoozed"} 1`,
		`navi_occurrence_transitions_total{from="notified",source="notification",to="missed"} 1`,
	} {
		fmt.Printf("  transitions metric      %s  %s\n",
			want, verdict(strings.Contains(metricsRec.Body.String(), want)))
	}
	return nil
}

// reportCallbackCap walks a chain to its cap through the keyboard and checks
// the toast says what the 409 would have (R8).
func reportCallbackCap(
	ctx context.Context,
	st *store.Store,
	m *metrics.Metrics,
	api *fakeTelegram,
	tap func(data string, messageID int, text string) *httptest.ResponseRecorder,
	notify func(title string) (domain.Occurrence, error),
	log *slog.Logger,
) error {
	root, err := notify("callback cap body")
	if err != nil {
		return err
	}
	item, err := st.GetItem(ctx, root.ItemID)
	if err != nil {
		return err
	}

	// Walk to the cap the way reportSnoozeCap does: through the store, with
	// each link landing in the past so the next scheduler pass can claim it and
	// make it notified. Only the last step goes through the keyboard, because
	// the keyboard is what this section is checking and the walk is scaffolding.
	pastMinute := func(domain.Item, domain.Occurrence) (time.Time, error) {
		return time.Now().Add(-30 * time.Second), nil
	}
	current := root
	for depth := 0; depth < item.SnoozeCap; depth++ {
		res, err := st.SnoozeOccurrence(ctx, current.ID, domain.ResolvedByNotification, time.Now(), pastMinute)
		if err != nil {
			return err
		}
		if res.CapReached {
			return fmt.Errorf("naviseed: callback cap: hit the cap at depth %d of %d", depth, item.SnoozeCap)
		}
		if err := fireAll(ctx, st, m, log); err != nil {
			return err
		}
		if current, err = st.GetOccurrence(ctx, res.Child.ID); err != nil {
			return err
		}
	}

	api.reset()
	over := tap("n1:snooze:"+current.ID+":10m", 5003, "callback cap body")
	final, err := st.GetOccurrence(ctx, current.ID)
	if err != nil {
		return err
	}
	toast := str(api.last("answerCallbackQuery"), "text")
	capOK := over.Code == http.StatusOK &&
		final.Status == domain.StatusMissed &&
		strings.Contains(toast, "snooze cap") &&
		strings.Contains(toast, "missed")
	fmt.Printf("  the cap resolves as missed  %s  toast %q  %s\n",
		final.Status, toast, verdict(capOK))
	return nil
}

// reportCallbackMalformed checks the three ways a payload fails to decode. All
// three take one path: a toast, a counter, and nothing else.
func reportCallbackMalformed(
	ctx context.Context,
	st *store.Store,
	m *metrics.Metrics,
	api *fakeTelegram,
	tap func(data string, messageID int, text string) *httptest.ResponseRecorder,
	knownID string,
) error {
	before, err := statusOf(ctx, st, knownID)
	if err != nil {
		return err
	}

	cases := []struct {
		name string
		data string
	}{
		{"unframed", "garbage"},
		{"unknown version", "n0:complete:" + knownID},
		{"id is not a ULID", "n1:complete:not-a-ulid"},
		{"arg on a non-snooze action", "n1:complete:" + knownID + ":1h"},
	}
	for _, c := range cases {
		api.reset()
		rec := tap(c.data, 5004, "callback probe body")
		ok := rec.Code == http.StatusOK &&
			api.count("editMessageText") == 0 &&
			api.count("editMessageReplyMarkup") == 0 &&
			api.count("sendMessage") == 0 &&
			api.count("answerCallbackQuery") == 1
		fmt.Printf("  malformed: %-26s status %d  answered only %v  %s\n",
			c.name, rec.Code, ok, verdict(ok))
	}

	after, err := statusOf(ctx, st, knownID)
	if err != nil {
		return err
	}
	metricsRec := httptest.NewRecorder()
	m.Handler().ServeHTTP(metricsRec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	counted := strings.Contains(metricsRec.Body.String(),
		`navi_inbound_messages_dropped_total{reason="callback_decode"} 4`)
	fmt.Printf("  malformed: nothing written  %s -> %s  drop counted %v  %s\n",
		before, after, counted, verdict(before == after && counted))
	return nil
}

// reportCallbackEditFallback is N6's other half: an edit the API refuses falls
// back to a short confirmation, so the outcome is never lost even though it
// costs the second message N6 exists to avoid.
func reportCallbackEditFallback(
	api *fakeTelegram,
	tap func(data string, messageID int, text string) *httptest.ResponseRecorder,
	notify func(title string) (domain.Occurrence, error),
) error {
	occ, err := notify("callback fallback body")
	if err != nil {
		return err
	}

	api.reset()
	api.mu.Lock()
	api.editFails = true
	api.mu.Unlock()
	defer func() {
		api.mu.Lock()
		api.editFails = false
		api.mu.Unlock()
	}()

	rec := tap("n1:skip:"+occ.ID, 5005, "callback fallback body")
	confirmation := api.last("sendMessage")
	ok := rec.Code == http.StatusOK &&
		api.count("editMessageText") == 1 &&
		api.count("sendMessage") == 1 &&
		keyboardOf(confirmation) == nil
	fmt.Printf("  edit refused -> confirmation  status %d  sent %q, no keyboard %v  %s\n",
		rec.Code, str(confirmation, "text"), keyboardOf(confirmation) == nil, verdict(ok))
	return nil
}

// reportBulkResolve drives the agent's bulk_resolve tool, which is what the
// last P2 exit criterion needs: "did my stretching already" at 07:00 has to
// cancel the 18:00 notification, and no P1 tool could resolve anything.
//
// The cancellation is the interesting half and it needs no cancel path. Both
// ListDueOccurrences and ClaimOccurrence filter status = 'pending', so a row
// moved to a terminal status has already left the fire path by construction —
// which is a claim about the SQL, and the only way to check it is to run a real
// scheduler pass afterwards and count zero.
func reportBulkResolve(ctx context.Context, st *store.Store, table *defaults.Table, tz string, defaultTZ *time.Location, log *slog.Logger) error {
	fmt.Println("\nagent bulk_resolve")

	mat := materializer.New(log.With("component", "materializer-bulk"), st, defaultTZ)

	// A real registry, because this is where the agent's own transition series
	// is checked. Every other resolution surface counts at its edge and this
	// one did not until session 17, so the assertion below is the thing that
	// keeps it counting.
	m := metrics.New()
	t := agent.New(st, mat, table, defaultTZ, m)

	item, err := seedFireItem(ctx, st, "bulk resolve probe", domain.NotifyAtTime, tz)
	if err != nil {
		return err
	}

	// Three pending rows, all already due, so the scheduler would take every
	// one of them on its next pass.
	titles := []string{"bulk stretching", "bulk vitamins", "bulk walk"}
	occs := make([]domain.Occurrence, 0, len(titles))
	for _, title := range titles {
		occ, err := fireOccurrence(ctx, st, item, time.Now().Add(-time.Minute), ptr(title))
		if err != nil {
			return err
		}
		occs = append(occs, occ)
	}

	// "Stretching and vitamins yes, skipped the walk" — one call, one
	// transaction, three rows.
	res, err := callTool(ctx, t, "bulk_resolve", agent.BulkResolveArgs{
		Resolutions: []agent.ResolutionArg{
			{OccurrenceID: occs[0].ID, Status: "completed"},
			{OccurrenceID: occs[1].ID, Status: "completed"},
			{OccurrenceID: occs[2].ID, Status: "skipped", Note: ptr("I was away")},
		},
	})
	if err != nil {
		return err
	}
	statuses := make([]domain.Status, 0, len(occs))
	for _, occ := range occs {
		s, err := statusOf(ctx, st, occ.ID)
		if err != nil {
			return err
		}
		statuses = append(statuses, s)
	}
	batchOK := len(res.Resolutions) == 3 &&
		statuses[0] == domain.StatusCompleted &&
		statuses[1] == domain.StatusCompleted &&
		statuses[2] == domain.StatusSkipped
	fmt.Printf("  three in one write      %v  %s\n", statuses, verdict(batchOK))

	// The exit criterion. Nothing was sent for any of them, because a resolved
	// row is not a pending row.
	rec := &recordingTransport{}
	sched := scheduler.New(log.With("component", "scheduler-bulk"), st, rec, metrics.New(), time.Now())
	if _, err := sched.Fire(ctx); err != nil {
		return err
	}
	sends := 0
	for _, title := range titles {
		sends += rec.count(title)
	}
	fmt.Printf("  early resolution cancels the notification  %d send(s)  %s\n",
		sends, verdict(sends == 0))

	// Idempotency, through the tool rather than the endpoint: the same batch
	// again writes nothing and is not an error.
	repeat, err := callTool(ctx, t, "bulk_resolve", agent.BulkResolveArgs{
		Resolutions: []agent.ResolutionArg{{OccurrenceID: occs[0].ID, Status: "completed"}},
	})
	noopOK := err == nil && len(repeat.Resolutions) == 1 && !repeat.Resolutions[0].Applied
	fmt.Printf("  repeat is a no-op       applied=%v  %s\n",
		len(repeat.Resolutions) == 1 && repeat.Resolutions[0].Applied, verdict(noopOK))

	// Atomicity: one bad id and nothing at all is written. This is the whole
	// argument for a list-taking tool over repeated single calls.
	fresh, err := fireOccurrence(ctx, st, item, time.Now().Add(-time.Minute), ptr("bulk atomic probe"))
	if err != nil {
		return err
	}
	_, atomicErr := callTool(ctx, t, "bulk_resolve", agent.BulkResolveArgs{
		Resolutions: []agent.ResolutionArg{
			{OccurrenceID: fresh.ID, Status: "completed"},
			{OccurrenceID: "01ARZ3NDEKTSV4RRFFQ69G5FAV", Status: "completed"},
		},
	})
	untouched, err := statusOf(ctx, st, fresh.ID)
	if err != nil {
		return err
	}
	fmt.Printf("  a bad id writes nothing  first row still %s  rejected %v  %s\n",
		untouched, atomicErr != nil,
		verdict(atomicErr != nil && untouched == domain.StatusPending))

	// Layer 1 over a list, which is what the slice walk in decode.go bought.
	empty := agent.BulkResolveArgs{Resolutions: []agent.ResolutionArg{}}
	_, emptyErr := callTool(ctx, t, "bulk_resolve", empty)
	_, enumErr := callTool(ctx, t, "bulk_resolve", agent.BulkResolveArgs{
		Resolutions: []agent.ResolutionArg{{OccurrenceID: fresh.ID, Status: "snoozed"}},
	})
	_, dupErr := callTool(ctx, t, "bulk_resolve", agent.BulkResolveArgs{
		Resolutions: []agent.ResolutionArg{
			{OccurrenceID: fresh.ID, Status: "completed"},
			{OccurrenceID: fresh.ID, Status: "skipped"},
		},
	})
	fmt.Printf("  empty list rejected      %v  %s\n", emptyErr != nil, verdict(emptyErr != nil))
	fmt.Printf("  per-row enum enforced    %v  %s\n", enumErr != nil, verdict(enumErr != nil))
	fmt.Printf("  duplicate id rejected    %v  %s\n", dupErr != nil, verdict(dupErr != nil))
	if enumErr != nil {
		fmt.Printf("    %s\n", enumErr)
	}
	return nil
}

// fireAll runs one real scheduler pass, which is how a row reaches notified
// without anything writing that status by hand.
func fireAll(ctx context.Context, st *store.Store, m *metrics.Metrics, log *slog.Logger) error {
	sched := scheduler.New(log.With("component", "scheduler-snooze"), st,
		&recordingTransport{}, m, time.Now())
	_, err := sched.Fire(ctx)
	return err
}

// reportSnoozeSurvival re-materializes the child's item and checks the child
// came through untouched. It is a check and not a change: nothing in this
// session touched the materializer.
func reportSnoozeSurvival(
	ctx context.Context,
	st *store.Store,
	item domain.Item,
	child domain.Occurrence,
	fallback *time.Location,
	log *slog.Logger,
) error {
	mat := materializer.New(log.With("component", "materializer-snooze"), st, fallback)
	if _, err := mat.Item(ctx, item.ID); err != nil {
		return err
	}

	after, err := st.GetOccurrence(ctx, child.ID)
	if err != nil {
		return err
	}
	survived := after.ID == child.ID && after.StartsAt.Equal(child.StartsAt) &&
		after.Status == domain.StatusPending && after.IsOverride
	fmt.Printf("    child survives materialization  starts_at %s  %s  %s\n",
		domain.FormatTime(after.StartsAt), after.Status, verdict(survived))
	return nil
}

// reportChainRollup snoozes a fresh occurrence, completes the child, and reads
// the chains view back.
//
// This is that view's first execution: it has existed since P0 and nothing
// created a chain to put through it until now, which is why session 13 built no
// roll-up rather than building one against an empty set. A chain counts once
// and any completed link completes it (D-011), so completing the child has to
// make the root's chain read completed with the depth it actually reached.
func reportChainRollup(
	ctx context.Context,
	st *store.Store,
	post func(id, body string) *httptest.ResponseRecorder,
	decode func(*httptest.ResponseRecorder) snoozeBody,
	claim func(title string, at time.Time) (domain.Occurrence, error),
) error {
	root, err := claim("snooze chain body", time.Now().Add(-time.Minute))
	if err != nil {
		return err
	}
	snoozed := decode(post(root.ID, `{"delta":"1h","source":"web"}`))
	childID := snoozed.Occurrence.ID

	res, err := st.ResolveOccurrence(ctx, childID, domain.StatusCompleted, nil,
		domain.ResolvedByWeb, time.Now())
	if err != nil {
		return err
	}

	chain, err := st.ChainFor(ctx, childID)
	if err != nil {
		return err
	}
	rootChain, err := st.ChainFor(ctx, root.ID)
	if err != nil {
		return err
	}

	// Two rows, one chain, and the same answer from either end of it.
	sameFromBothEnds := chain.RootID == rootChain.RootID && chain.RootID == root.ID
	rolledUp := chain.WasCompleted && chain.SnoozeCount == 1 &&
		chain.ScheduledAt.Equal(root.StartsAt) && chain.CompletedAt != nil
	fmt.Printf("  chain roll-up           root=%s snooze_count=%d was_completed=%v scheduled_at=%s  %s\n",
		presence(chain.RootID != ""), chain.SnoozeCount, chain.WasCompleted,
		domain.FormatTime(chain.ScheduledAt), verdict(rolledUp && sameFromBothEnds))

	// And the resolution endpoint reports the same roll-up it just caused, so
	// no surface has to walk parents for itself.
	fmt.Printf("    reported by resolve   snooze_count=%d was_completed=%v  %s\n",
		res.Chain.SnoozeCount, res.Chain.WasCompleted,
		verdict(res.Chain.WasCompleted && res.Chain.SnoozeCount == 1 &&
			res.Chain.RootID == root.ID))

	// The parent is still snoozed and was never rewritten: history is
	// immutable, and the chain's verdict is a read over the view rather than a
	// second terminal status written into an ancestor.
	parent, err := st.GetOccurrence(ctx, root.ID)
	if err != nil {
		return err
	}
	fmt.Printf("    parent left alone     %s  %s\n",
		parent.Status, verdict(parent.Status == domain.StatusSnoozed))
	return nil
}

// reportSnoozeCap walks a chain to the item's snooze_cap and asks for one more.
//
// The intermediate links go through the store rather than the endpoint, because
// each has to be claimable in turn and a real delta puts every child in the
// future. What the endpoint has to answer for is the last step, which is the
// only one that behaves differently: a 409 whose current_state is the missed
// the cap just wrote. This is missed's first caller anywhere in this repository
// (R8, D-008).
func reportSnoozeCap(
	ctx context.Context,
	st *store.Store,
	m *metrics.Metrics,
	post func(id, body string) *httptest.ResponseRecorder,
	claim func(title string, at time.Time) (domain.Occurrence, error),
	log *slog.Logger,
) error {
	root, err := claim("snooze cap body", time.Now().Add(-time.Minute))
	if err != nil {
		return err
	}
	item, err := st.GetItem(ctx, root.ItemID)
	if err != nil {
		return err
	}

	// Each link lands 30 seconds back so the next pass can claim it.
	pastMinute := func(domain.Item, domain.Occurrence) (time.Time, error) {
		return time.Now().Add(-30 * time.Second), nil
	}

	live := root
	for depth := 0; depth < item.SnoozeCap; depth++ {
		res, err := st.SnoozeOccurrence(ctx, live.ID, domain.ResolvedByWeb, time.Now(), pastMinute)
		if err != nil {
			return err
		}
		if res.CapReached {
			return fmt.Errorf("naviseed: snooze cap: hit the cap at depth %d of %d", depth, item.SnoozeCap)
		}
		if err := fireAll(ctx, st, m, log); err != nil {
			return err
		}
		if live, err = st.GetOccurrence(ctx, res.Child.ID); err != nil {
			return err
		}
	}

	fmt.Printf("  snooze cap %d            walked to depth %d, live link %s\n",
		item.SnoozeCap, live.SnoozeDepth, live.Status)

	capped := post(live.ID, `{"delta":"1h","source":"web"}`)
	after, err := st.GetOccurrence(ctx, live.ID)
	if err != nil {
		return err
	}
	children, err := st.ListOccurrencesForItem(ctx, item.ID)
	if err != nil {
		return err
	}
	fourth := 0
	for _, row := range children {
		if row.ParentOccurrenceID != nil && *row.ParentOccurrenceID == live.ID {
			fourth++
		}
	}

	state := currentState(capped.Body.Bytes())
	fmt.Printf("    one more              status %d  current_state=%q  row %s  no child=%v  %s\n",
		capped.Code, state, after.Status, fourth == 0,
		verdict(capped.Code == http.StatusConflict &&
			state == string(domain.StatusMissed) &&
			after.Status == domain.StatusMissed && fourth == 0))
	fmt.Printf("    reason recorded       %q\n", noteOf(after.ResolutionNote))

	chain, err := st.ChainFor(ctx, live.ID)
	fmt.Printf("    chain reads missed    snooze_count=%d was_completed=%v terminal %s  %s\n",
		chain.SnoozeCount, chain.WasCompleted, after.Status,
		verdict(err == nil && chain.RootID == root.ID &&
			chain.SnoozeCount == item.SnoozeCap && !chain.WasCompleted))
	return err
}

// reportSnoozeDeltas checks the four presets against schedule.ResolveDelta
// directly, with a fabricated now, so both branches of "tonight" and the
// spring-forward case are exercised on every run rather than on whichever ones
// happen to fall on the right side of a clock.
func reportSnoozeDeltas() {
	fmt.Println("\nsnooze deltas  resolved in the item's zone")

	toronto, err := schedule.LoadLocation("America/Toronto")
	if err != nil {
		fmt.Printf("  %s\n", verdict(false))
		return
	}

	fixedAt := func(hhmm string) schedule.Schedule {
		return schedule.Schedule{Kind: schedule.KindFixed, RRule: ptr("FREQ=DAILY"), At: ptr(hhmm)}
	}
	windowed := func(start, end string) schedule.Schedule {
		return schedule.Schedule{
			Kind:   schedule.KindWindowed,
			RRule:  ptr("FREQ=DAILY"),
			Window: []string{start, end},
		}
	}

	check := func(label string, d schedule.Delta, s schedule.Schedule, now, want time.Time, wantFold schedule.Fold) {
		at, fold, err := schedule.ResolveDelta(d, s, toronto, now)
		ok := err == nil && at.Equal(want) && fold == wantFold
		fmt.Printf("  %-34s %s -> %s  (%s local)  %s\n",
			label, domain.FormatTime(now), domain.FormatTime(at),
			at.In(toronto).Format("2006-01-02 15:04 MST"), verdict(ok))
		if !ok {
			fmt.Printf("    wanted              %s fold=%s  err=%v\n",
				domain.FormatTime(want), wantFold, err)
		}
	}

	// An ordinary summer afternoon, well away from any transition.
	noon := time.Date(2026, 8, 19, 12, 0, 0, 0, toronto)

	// The two offsets are instant arithmetic and ignore the schedule entirely.
	check("10m", schedule.Delta10m, fixedAt("07:00"), noon, noon.Add(10*time.Minute), schedule.FoldNone)
	check("1h", schedule.Delta1h, fixedAt("07:00"), noon, noon.Add(time.Hour), schedule.FoldNone)

	// tonight is 19:00 local, and now + 2h once 19:00 has gone.
	check("tonight, before 19:00", schedule.DeltaTonight, fixedAt("07:00"), noon,
		time.Date(2026, 8, 19, 19, 0, 0, 0, toronto), schedule.FoldNone)

	evening := time.Date(2026, 8, 19, 20, 30, 0, 0, toronto)
	check("tonight, after 19:00", schedule.DeltaTonight, fixedAt("07:00"), evening,
		evening.Add(2*time.Hour), schedule.FoldNone)

	// tomorrow is the item's own time of day, per kind. A fixed item has one;
	// a windowed item does not, so its window opens the day instead.
	check("tomorrow, fixed at 07:00", schedule.DeltaTomorrow, fixedAt("07:00"), noon,
		time.Date(2026, 8, 20, 7, 0, 0, 0, toronto), schedule.FoldNone)
	check("tomorrow, window 12:00-17:00", schedule.DeltaTomorrow, windowed("12:00", "17:00"), noon,
		time.Date(2026, 8, 20, 12, 0, 0, 0, toronto), schedule.FoldNone)

	// A schedule that names no time at all falls back to 09:00 — the spec's
	// literal answer, and the only place it applies.
	check("tomorrow, no time at all", schedule.DeltaTomorrow,
		schedule.Schedule{Kind: schedule.KindWindowed, RRule: ptr("FREQ=DAILY")}, noon,
		time.Date(2026, 8, 20, 9, 0, 0, 0, toronto), schedule.FoldNone)

	// Spring forward. 2027-03-14 is the second Sunday in March, so the clock
	// goes 02:00 EST -> 03:00 EDT overnight. 09:00 tomorrow is 23 hours away,
	// and Add(24 * time.Hour) would put the reminder at 10:00.
	beforeDST := time.Date(2027, 3, 13, 9, 0, 0, 0, toronto)
	wantDST := time.Date(2027, 3, 14, 9, 0, 0, 0, toronto)
	check("tomorrow, across spring-forward", schedule.DeltaTomorrow, fixedAt("09:00"),
		beforeDST, wantDST, schedule.FoldNone)
	fmt.Printf("  %-34s %s  %s\n", "  and it is 23h, not 24h",
		wantDST.Sub(beforeDST), verdict(wantDST.Sub(beforeDST) == 23*time.Hour))

	// A wall clock inside the gap never happened, so it resolves to the
	// transition instant itself — 03:00 EDT, not 03:30.
	check("tomorrow, into the DST gap", schedule.DeltaTomorrow, fixedAt("02:30"),
		beforeDST, time.Date(2027, 3, 14, 3, 0, 0, 0, toronto), schedule.FoldGap)
}

// reportReconcile drives the daily check-in end to end against a transport that
// records instead of delivering, the same way reportFire drives the scheduler.
//
// Two scenarios, and they anchor their clocks differently on purpose. The first
// runs against the real clock, because the notified row in it has to reach
// notified the way production does - through a real scheduler pass, which
// claims only rows inside its own recovery window. The second fabricates its
// instants, because what it checks is which pass sees which item, and two
// passes at two different times of day are not otherwise observable on a single
// run.
//
// The latch is reset between them. kv.last_reconcile_date is one key shared by
// every pass, so a scenario that ran at 16:00 would block a fabricated 14:00
// one - which is the mechanism working, not a bug, and resetting is how the
// second scenario gets to exercise it from a known state.
func reportReconcile(ctx context.Context, st *store.Store, tz string, fallback *time.Location, log *slog.Logger) error {
	fmt.Println("\nreconcile  the daily check-in")

	// The device zone, not DEFAULT_TZ. The reconciler resolves its clock and its
	// day boundary with schedule.Zones.Local(), and reportZones above has
	// already moved kv.current_tz to Europe/Lisbon - so a check written against
	// the deployment default would be five hours out and would fail or pass by
	// the hour of day it ran at. That is the shape of the from_date assertion
	// that survived twelve sessions by only failing in the afternoon.
	zones, err := schedule.LoadZones(ctx, st, fallback)
	if err != nil {
		return err
	}
	loc := zones.Local()
	m := metrics.New()

	// The layout the reconciler compares slots with has to be the one config
	// validates RECONCILE_AT against, or a pass could be due by one and not the
	// other. They are separate constants because internal/config is the
	// environment reader and nothing outside main depends on it; this is the
	// assertion that keeps the duplication honest.
	fmt.Printf("  slot layout             config=%q reconciler=%q  %s\n",
		config.LocalTimeLayout, reconciler.LocalTimeLayout,
		verdict(config.LocalTimeLayout == reconciler.LocalTimeLayout))

	resetLatch := func() error {
		return st.RecordCheckIn(ctx, nil, "", nil, time.Now())
	}
	if err := resetLatch(); err != nil {
		return err
	}

	// --- scenario one: what a check-in covers -------------------------------

	now := time.Now()
	nowHM := now.In(loc).Format(reconciler.LocalTimeLayout)

	// The pass runs at the instant this scenario calls "now", so the slot is
	// now's own local HH:MM: it has arrived by definition, and no lookback can
	// stray onto yesterday near midnight.
	// Nil composer: this scenario is about the gather, the send and the
	// latch, and the template is what it asserts against. reportReconcileComposer
	// is where a composer is wired in and where the fallback is proved.
	rec := &recordingTransport{}
	r := reconciler.New(log.With("loop", "reconciler"), st, rec, m, nil, nowHM, fallback)

	silent, err := seedFireItem(ctx, st, "reconcile silent probe", domain.NotifySilent, tz)
	if err != nil {
		return err
	}
	loud, err := seedFireItem(ctx, st, "reconcile notified probe", domain.NotifyAtTime, tz)
	if err != nil {
		return err
	}
	settled, err := seedFireItem(ctx, st, "reconcile resolved probe", domain.NotifyAtTime, tz)
	if err != nil {
		return err
	}

	// A minute ago, but never earlier than local midnight: the pass gathers
	// [midnight, now], and for the one minute a day when those two are less than
	// a minute apart a bare now-1min would land on yesterday and be invisible.
	// The scheduler's claim window is fifteen minutes, so clamping here still
	// leaves the row claimable.
	at := now.Add(-time.Minute)
	local := now.In(loc)
	if midnight := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, loc); at.Before(midnight) {
		at = midnight
	}

	silentOcc, err := fireOccurrence(ctx, st, silent, at, nil)
	if err != nil {
		return err
	}
	loudOcc, err := fireOccurrence(ctx, st, loud, at, ptr("reconcile notified body"))
	if err != nil {
		return err
	}
	settledOcc, err := fireOccurrence(ctx, st, settled, at, ptr("reconcile resolved body"))
	if err != nil {
		return err
	}

	// The at_time row reaches notified through a real scheduler pass. The silent
	// one does not move: ListDueOccurrences filters notify_policy = 'at_time',
	// so K2's "a silent item still generates occurrences" and K5's "covers both"
	// are the same row seen from two loops.
	if err := fireAll(ctx, st, m, log); err != nil {
		return err
	}
	loudStatus, err := statusOf(ctx, st, loudOcc.ID)
	if err != nil {
		return err
	}
	silentStatus, err := statusOf(ctx, st, silentOcc.ID)
	if err != nil {
		return err
	}
	fmt.Printf("  before the check-in     silent=%s notified=%s  %s\n",
		silentStatus, loudStatus,
		verdict(silentStatus == domain.StatusPending && loudStatus == domain.StatusNotified))

	// Resolved before the check-in, so it is absent from it rather than listed
	// and then explained away.
	if _, err := st.ResolveOccurrence(ctx, settledOcc.ID, domain.StatusCompleted,
		ptr("did it at breakfast"), domain.ResolvedByWeb, time.Now()); err != nil {
		return err
	}

	res, err := r.Reconcile(ctx, now)
	if err != nil {
		return err
	}
	body := ""
	if len(rec.sent) > 0 {
		body = rec.sent[len(rec.sent)-1].Body
	}
	oneMessage := len(rec.sent) == 1 && res.Sent
	fmt.Printf("  one message, not one per item  sent=%d covering %d occurrence(s)  %s\n",
		len(rec.sent), res.Occurrences, verdict(oneMessage))
	fmt.Printf("    %s\n", strings.ReplaceAll(body, "\n", "\n    "))

	hasSilent := strings.Contains(body, silent.Title)
	hasLoud := strings.Contains(body, loud.Title)
	hasSettled := strings.Contains(body, settled.Title)
	fmt.Printf("  silent item listed      %v  (it never pushed)                 %s\n",
		hasSilent, verdict(hasSilent))
	fmt.Printf("  notified item listed    %v  (it pushed and was ignored)       %s\n",
		hasLoud, verdict(hasLoud))
	fmt.Printf("  resolved item listed    %v  (absent, K5)                      %s\n",
		hasSettled, verdict(!hasSettled))

	// reconciled_at is the instant next session's grace window measures from,
	// and it is written on exactly what was asked about.
	askedSilent, err := reconciledAt(ctx, st, silentOcc.ID)
	if err != nil {
		return err
	}
	askedSettled, err := reconciledAt(ctx, st, settledOcc.ID)
	if err != nil {
		return err
	}
	fmt.Printf("  reconciled_at written   asked=%v resolved-row=%v  %s\n",
		askedSilent, askedSettled, verdict(askedSilent && !askedSettled))

	// Nothing is missed. K6 and D-008: the check-in has asked, and until the
	// grace window closes on an unanswered question nothing may say otherwise.
	missed, err := countStatus(ctx, st, []string{silent.ID, loud.ID, settled.ID}, domain.StatusMissed)
	if err != nil {
		return err
	}
	fmt.Printf("  nothing marked missed   %d  %s\n", missed, verdict(missed == 0))

	// The restart case, minus the restart: the latch is durable, so a second
	// pass at the same slot finds it already run and says nothing.
	before := len(rec.sent)
	second, err := r.Reconcile(ctx, now)
	if err != nil {
		return err
	}
	fmt.Printf("  second pass, same slot  slot=%q sent=%d  %s\n",
		second.Slot, len(rec.sent)-before,
		verdict(second.Slot == "" && !second.Sent && len(rec.sent) == before))

	// --- pause, at both levels ---------------------------------------------

	if err := resetLatch(); err != nil {
		return err
	}
	pauseOcc, err := fireOccurrence(ctx, st, silent, at, nil)
	if err != nil {
		return err
	}
	_ = pauseOcc

	if err := st.SetGlobalPauseUntil(ctx, now.Add(72*time.Hour)); err != nil {
		return err
	}
	pausedRes, err := r.Reconcile(ctx, now)
	if err != nil {
		return err
	}
	latch, _, err := st.LastReconcileSlot(ctx)
	if err != nil {
		return err
	}
	fmt.Printf("  global pause            suppressed=%v sent=%d latch=%q  %s\n",
		pausedRes.Paused, len(rec.sent)-before, latch,
		verdict(pausedRes.Paused && !pausedRes.Sent && len(rec.sent) == before && latch == ""))

	if err := st.ClearGlobalPause(ctx); err != nil {
		return err
	}

	// A row on a second item, so the next pass has something to say once the
	// paused one drops out. Without it the pass would be silent for the ordinary
	// reason - nothing outstanding - and "the paused item is absent" would pass
	// for the wrong one.
	if _, err := fireOccurrence(ctx, st, loud, at, ptr("reconcile unpaused body")); err != nil {
		return err
	}

	// One item paused, the rest still asked about. This is what makes pausing a
	// window rather than a resolution: the occurrence is neither listed nor
	// skipped, it is simply not the subject of a question.
	if _, _, err := st.PauseItemAndMaterialize(ctx, silent.ID, ptr(now.Add(72*time.Hour)), now,
		func(domain.Item, []domain.Occurrence) (store.Plan, error) { return store.Plan{}, nil }); err != nil {
		return err
	}
	itemPausedRes, err := r.Reconcile(ctx, now)
	if err != nil {
		return err
	}
	pausedBody := ""
	if len(rec.sent) > 0 {
		pausedBody = rec.sent[len(rec.sent)-1].Body
	}
	stillAsked := itemPausedRes.Sent &&
		!strings.Contains(pausedBody, silent.Title) &&
		strings.Contains(pausedBody, loud.Title)
	fmt.Printf("  one item paused         paused item listed=%v, unpaused one listed=%v  %s\n",
		strings.Contains(pausedBody, silent.Title),
		strings.Contains(pausedBody, loud.Title), verdict(stillAsked))
	if _, _, err := st.PauseItemAndMaterialize(ctx, silent.ID, nil, now,
		func(domain.Item, []domain.Occurrence) (store.Plan, error) { return store.Plan{}, nil }); err != nil {
		return err
	}

	// --- scenario two: per-item timing (K8) vs the one-message rule (K4) ----

	return reportReconcileTiming(ctx, st, tz, fallback, m, log, loc)
}

// reportReconcileTiming is US-5.4: an item checked at 14:00 rather than 21:00.
//
// The decision this asserts is that a per-item reconcile_at triggers its own
// pass rather than joining the global one, so K4's one-message rule binds per
// pass. The other half of the decision is that the later pass still sweeps
// whatever the earlier one could not see yet - an occurrence that started after
// it - which is what occurrences.reconciled_at makes possible without either
// pass knowing the other exists.
//
// Instants are fabricated so both passes happen on one run whatever the wall
// clock says.
func reportReconcileTiming(
	ctx context.Context,
	st *store.Store,
	tz string,
	fallback *time.Location,
	m *metrics.Metrics,
	log *slog.Logger,
	loc *time.Location,
) error {
	if err := st.RecordCheckIn(ctx, nil, "", nil, time.Now()); err != nil {
		return err
	}

	rec := &recordingTransport{}
	r := reconciler.New(log.With("loop", "reconciler-timing"), st, rec, m, nil, "21:00", fallback)

	early, err := seedFireItem(ctx, st, "reconcile midday probe", domain.NotifySilent, tz)
	if err != nil {
		return err
	}
	if early.ReconcileAt == nil {
		updated, _, err := st.UpdateItemAndMaterialize(ctx, early.ID,
			store.ItemPatch{ReconcileAt: ptr("14:00")}, time.Now(), nil)
		if err != nil {
			return err
		}
		early = updated
	}
	evening, err := seedFireItem(ctx, st, "reconcile evening probe", domain.NotifySilent, tz)
	if err != nil {
		return err
	}

	// Yesterday, not today, and for two reasons. A completed day is isolated:
	// every other section's rows are today's, so neither pass here can see them
	// and both assertions are about exactly the items this function seeded.
	// And it keeps the clock this fabricates behind the real one - RecordCheckIn
	// stamps its conversations row with the pass's own instant, so a pass
	// fabricated at 21:30 today would write a row dated hours in the future and
	// sort itself to the top of the history the conversation ladder loads much
	// later in this run. That is a real property of the row, not an artifact:
	// the timestamp is the moment the check-in asked.
	day := time.Now().In(loc).AddDate(0, 0, -1)
	localAt := func(hour, min int) time.Time {
		return time.Date(day.Year(), day.Month(), day.Day(), hour, min, 0, 0, loc)
	}

	// One occurrence of each before the midday pass, plus a second for the
	// midday item after it - the row only the evening pass can reach.
	if _, err := fireOccurrence(ctx, st, early, localAt(8, 0), nil); err != nil {
		return err
	}
	if _, err := fireOccurrence(ctx, st, evening, localAt(8, 0), nil); err != nil {
		return err
	}
	if _, err := fireOccurrence(ctx, st, early, localAt(18, 0), nil); err != nil {
		return err
	}

	midday, err := r.Reconcile(ctx, localAt(14, 30))
	if err != nil {
		return err
	}
	middayBody := ""
	if len(rec.sent) > 0 {
		middayBody = rec.sent[len(rec.sent)-1].Body
	}
	middayOK := midday.Slot == "14:00" &&
		strings.Contains(middayBody, early.Title) &&
		!strings.Contains(middayBody, evening.Title)
	fmt.Printf("  14:00 pass              slot=%q covers the 14:00 item only  %s\n",
		midday.Slot, verdict(middayOK))

	night, err := r.Reconcile(ctx, localAt(21, 30))
	if err != nil {
		return err
	}
	nightBody := ""
	if len(rec.sent) > 0 {
		nightBody = rec.sent[len(rec.sent)-1].Body
	}
	// The 14:00 item is named again, but for its 18:00 row and not its 08:00
	// one - reconciled_at is per row, so nothing is asked about twice while
	// nothing outstanding is dropped either (Q-4's leaning).
	nightOK := night.Slot == "21:00" &&
		strings.Contains(nightBody, evening.Title) &&
		strings.Contains(nightBody, early.Title)
	fmt.Printf("  21:00 pass              slot=%q sweeps the rest, including the later midday row  %s\n",
		night.Slot, verdict(nightOK))
	fmt.Printf("    %s\n", strings.ReplaceAll(nightBody, "\n", "\n    "))
	return nil
}

// composeServer answers with plain prose, the shape a check-in composition
// comes back as. Distinct from proseServer only in that this one is the
// success case rather than the ladder's failure case.
func composeServer(text string) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		escaped, _ := json.Marshal(text)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"choices":[{"index":0,"message":{"role":"assistant","content":%s},"finish_reason":"stop"}]}`,
			string(escaped))
	}))
}

// brokenServer answers every request with a 500 - model.KindUnavailable, the
// provider being down rather than the request being wrong.
func brokenServer() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprint(w, `{"error":{"message":"upstream exploded"}}`)
	}))
}

// reportReconcileComposer drives the model-composed check-in and, more
// importantly, its fallback.
//
// The fallback is the point. D-009 gives this component the failure mode "fall
// back to a templated list", and until this session the template was the only
// path, so it had never actually been a fallback. What is asserted here is that
// breaking the model produces the template verbatim - by breaking a real model
// client against a real HTTP server that returns 500, not by reading the code
// and agreeing that it would.
//
// Four cases, and each is a different way for composition to be unusable: the
// provider is down, the provider answers with nothing, the provider answers
// with an essay, and there is no provider configured at all. All four have to
// reach the same text, because a check-in that does not go out is a day with no
// misses and no answers.
func reportReconcileComposer(ctx context.Context, st *store.Store, tz string, fallback *time.Location, log *slog.Logger) error {
	fmt.Println("\nreconcile  composing the check-in, and falling back")

	zones, err := schedule.LoadZones(ctx, st, fallback)
	if err != nil {
		return err
	}
	loc := zones.Local()
	m := metrics.New()

	// One item, one outstanding row, re-used by every case below: what varies
	// is the composer, so everything else has to be identical or the bodies
	// are not comparable.
	item, err := seedFireItem(ctx, st, "compose probe", domain.NotifySilent, tz)
	if err != nil {
		return err
	}

	now := time.Now()
	local := now.In(loc)
	at := now.Add(-time.Minute)
	if midnight := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, loc); at.Before(midnight) {
		at = midnight
	}

	// The template the composer is being compared against, built from the same
	// shape the reconciler will gather. composeTemplate is unexported, so this
	// is its output as the reconciler with a nil composer produces it - which
	// is the honest comparison anyway: what matters is that the two paths send
	// the same bytes, not that a helper agrees with itself.
	nowHM := now.In(loc).Format(reconciler.LocalTimeLayout)

	// run drives one pass with one composer and returns what went out.
	//
	// Every case has to see the *same* outstanding set, or the bodies are not
	// comparable and "the fallback matches the template" would be comparing
	// two different lists of items. Earlier sections leave plenty outstanding,
	// so each call drains first - one throwaway pass that stamps reconciled_at
	// on whatever is left over - and only then seeds the single row this case
	// is about. The drain runs against its own metrics registry so its
	// nil-composer fallback is not counted against the real cases below.
	run := func(c reconciler.Composer) (string, error) {
		if err := st.RecordCheckIn(ctx, nil, "", nil, time.Now()); err != nil {
			return "", err
		}
		drain := reconciler.New(log.With("loop", "reconciler-drain"), st,
			&recordingTransport{}, metrics.New(), nil, nowHM, fallback)
		if _, err := drain.Reconcile(ctx, now); err != nil {
			return "", err
		}

		if err := st.RecordCheckIn(ctx, nil, "", nil, time.Now()); err != nil {
			return "", err
		}
		if _, err := fireOccurrence(ctx, st, item, at, nil); err != nil {
			return "", err
		}
		rec := &recordingTransport{}
		r := reconciler.New(log.With("loop", "reconciler-compose"), st, rec, m, c, nowHM, fallback)
		res, err := r.Reconcile(ctx, now)
		if err != nil {
			return "", err
		}
		if !res.Sent || len(rec.sent) == 0 {
			return "", nil
		}
		return rec.sent[len(rec.sent)-1].Body, nil
	}

	// newComposer builds a real ModelComposer against a fake provider, through
	// a real model.Client - so a failure is classified by the same code the
	// production path uses and lands in llm_calls the same way.
	newComposer := func(srv *httptest.Server) reconciler.Composer {
		routing := &model.Routing{Tasks: map[model.Task]model.TaskRouting{
			model.TaskReconcile: {Tiers: []model.Tier{
				{Model: "naviseed-reconcile", BaseURL: srv.URL, TimeoutSeconds: 5},
			}},
		}}
		client := model.New(log.With("component", "model-compose"), routing, "", st, m)
		return reconciler.NewModelComposer(client, routing, "config/persona.md.does-not-exist")
	}

	// The no-composer body, which is the template, and the baseline every
	// other case is measured against.
	templated, err := run(nil)
	if err != nil {
		return err
	}
	fmt.Printf("  no composer configured  %q  %s\n",
		truncate(templated, 60), verdict(templated != ""))

	// 1. The happy path: composed prose goes out, and it is not the template.
	const composed = "Haven't heard how the compose probe went today - did you get to it?"
	okSrv := composeServer(composed)
	defer okSrv.Close()
	body, err := run(newComposer(okSrv))
	if err != nil {
		return err
	}
	fmt.Printf("  composed check-in sent  %q  %s\n",
		truncate(body, 60), verdict(body == composed && body != templated))

	// 2. The provider is down. This is the assertion the session exists for:
	// the fallback is exercised by breaking the model, and the bytes that go
	// out are the template's, not a truncated error or an empty message.
	beforeCalls, err := countLLMCalls(ctx, st)
	if err != nil {
		return err
	}
	downSrv := brokenServer()
	defer downSrv.Close()
	body, err = run(newComposer(downSrv))
	if err != nil {
		return err
	}
	afterCalls, err := countLLMCalls(ctx, st)
	if err != nil {
		return err
	}
	fmt.Printf("  model down -> template  %q  llm_calls +%d  %s\n",
		truncate(body, 50), afterCalls-beforeCalls,
		verdict(body == templated && afterCalls > beforeCalls))

	// 3. A call that succeeds and says nothing. model.Complete reports this as
	// success, because adequacy is the caller's judgement - acceptComposed is
	// where that judgement is made, and this is it being made.
	emptySrv := composeServer("   ")
	defer emptySrv.Close()
	body, err = run(newComposer(emptySrv))
	if err != nil {
		return err
	}
	fmt.Printf("  empty completion        -> template  %s\n", verdict(body == templated))

	// 4. An essay. The guard falls back rather than truncating: the first 400
	// runes of a message that misunderstood the task is not better prose than
	// the template, it is the same mistake cut short.
	longSrv := composeServer(strings.Repeat("nag ", 400))
	defer longSrv.Close()
	body, err = run(newComposer(longSrv))
	if err != nil {
		return err
	}
	fmt.Printf("  over-long completion    -> template  %s\n", verdict(body == templated))

	// Every fallback above is counted, including the nil-composer one, which
	// is the case llm_calls cannot see.
	metricsRec := httptest.NewRecorder()
	m.Handler().ServeHTTP(metricsRec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	counted := strings.Contains(metricsRec.Body.String(), "navi_reconcile_fallback_total 4")
	fmt.Printf("  fallbacks counted       navi_reconcile_fallback_total 4  %s\n", verdict(counted))

	return nil
}

// reportReconcileReply is the reply path: a check-in goes out, the user answers
// it in one plain message, and three occurrences resolve in one write.
//
// What this can and cannot assert is worth being exact about, because the
// tempting version of it is dishonest. The model here is a fake that returns a
// fixed bulk_resolve call, so nothing below demonstrates a model *recognising*
// a reply - a scripted answer proves the plumbing behind the answer and nothing
// about the comprehension in front of it.
//
// So the real assertion is the first one: that the system prompt the ladder
// actually assembled and sent carries the Context ref block, naming the check-in
// and every occurrence still waiting on an answer. That is the whole mechanism.
// Recognition is not a classifier or a route - it is these lines being present,
// plus the check-in itself sitting in cross-turn history one message above the
// reply. If they are there, the model has what it needs; if they are not, no
// amount of prompt engineering downstream would help.
//
// The rest asserts the write: A8's single atomic operation, R2's skip carrying
// its reason, and the confirmation naming items rather than reading back
// occurrence ids.
func reportReconcileReply(ctx context.Context, st *store.Store, table *defaults.Table, tz string,
	defaultTZ *time.Location, log *slog.Logger) error {
	fmt.Println("\nreconcile  answering the check-in")

	zones, err := schedule.LoadZones(ctx, st, defaultTZ)
	if err != nil {
		return err
	}
	loc := zones.Local()
	m := metrics.New()
	mat := materializer.New(log.With("component", "mat-reply"), st, defaultTZ)
	tools := agent.New(st, mat, table, defaultTZ, m)

	// Three items, the three from US-5.2 by shape: two the reply says yes to
	// and one it skips with a reason.
	stretch, err := seedFireItem(ctx, st, "reply stretching", domain.NotifySilent, tz)
	if err != nil {
		return err
	}
	vitamins, err := seedFireItem(ctx, st, "reply vitamins", domain.NotifySilent, tz)
	if err != nil {
		return err
	}
	walk, err := seedFireItem(ctx, st, "reply walk", domain.NotifySilent, tz)
	if err != nil {
		return err
	}

	now := time.Now()
	local := now.In(loc)
	at := now.Add(-time.Minute)
	if midnight := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, loc); at.Before(midnight) {
		at = midnight
	}

	var occs []domain.Occurrence
	for _, it := range []domain.Item{stretch, vitamins, walk} {
		occ, err := fireOccurrence(ctx, st, it, at, nil)
		if err != nil {
			return err
		}
		occs = append(occs, occ)
	}

	// A real check-in, so reconciled_at and the context_ref row are written by
	// the code that writes them in production rather than fabricated here.
	if err := st.RecordCheckIn(ctx, nil, "", nil, time.Now()); err != nil {
		return err
	}
	nowHM := now.In(loc).Format(reconciler.LocalTimeLayout)
	rec := &recordingTransport{}
	r := reconciler.New(log.With("loop", "reconciler-reply"), st, rec, m, nil, nowHM, defaultTZ)
	checkIn, err := r.Reconcile(ctx, now)
	if err != nil {
		return err
	}
	wantRef := "reconcile:" + now.In(loc).Format(domain.DateLayout)
	fmt.Printf("  check-in sent           covering %d occurrence(s) ref=%s  %s\n",
		checkIn.Occurrences, wantRef, verdict(checkIn.Sent && checkIn.Occurrences >= 3))

	// The reply, scripted as the bulk_resolve the model would produce: two
	// completed, one skipped carrying the user's own words (R2, US-4.3).
	const reason = "I was away"
	args := fmt.Sprintf(
		`{"resolutions":[{"occurrence_id":%q,"status":"completed"},{"occurrence_id":%q,"status":"completed"},{"occurrence_id":%q,"status":"skipped","note":%q}]}`,
		occs[0].ID, occs[1].ID, occs[2].ID, reason)

	var captured []wireRequest
	tier1 := capturingToolCallServer("bulk_resolve", args, &captured)
	defer tier1.Close()

	routing := &model.Routing{Tasks: map[model.Task]model.TaskRouting{
		model.TaskCRUD: {Tiers: []model.Tier{
			{Model: "naviseed-reply", BaseURL: tier1.URL, TimeoutSeconds: 5},
		}},
	}}
	client := model.New(log.With("component", "model-reply"), routing, "", st, m)
	sender := &fakeSender{}
	ladder := conversation.New(client, tools, routing, st, table,
		"config/persona.md.does-not-exist", defaultTZ, sender)

	const replyText = "did everything except the walk, I was away"
	if err := ladder.Handle(ctx, transport.IncomingMessage{
		SenderID: "111", Text: replyText, Transport: telegram.Name,
	}); err != nil {
		return err
	}

	// --- the assertion that matters: what the model was actually given ------

	prompt := ""
	if len(captured) > 0 {
		prompt = captured[0].systemContent()
	}
	hasRef := strings.Contains(prompt, "Context ref:  "+wantRef)
	hasAll := strings.Contains(prompt, occs[0].ID) &&
		strings.Contains(prompt, occs[1].ID) &&
		strings.Contains(prompt, occs[2].ID)
	// Titles too, not only ids: past midnight the awaiting block is the one
	// place the title-to-id mapping survives, since Today's occurrences has
	// rolled over.
	hasTitles := strings.Contains(prompt, stretch.Title) &&
		strings.Contains(prompt, vitamins.Title) &&
		strings.Contains(prompt, walk.Title)
	fmt.Printf("  context ref in prompt   ref=%v ids=%v titles=%v  %s\n",
		hasRef, hasAll, hasTitles, verdict(hasRef && hasAll && hasTitles))

	// The check-in text itself is in cross-turn history, one message above the
	// reply. That is the other half of recognition and it costs nothing - the
	// reconciler writes the row in persistAssistantProse's shape precisely so
	// that seedHistory can replay it.
	inHistory := false
	for _, msg := range captured[0].Messages {
		if msg.Role == "assistant" && strings.Contains(msg.Content, stretch.Title) {
			inHistory = true
		}
	}
	fmt.Printf("  check-in in history     %v  (the question above the answer)      %s\n",
		inHistory, verdict(inHistory))

	// --- and what the write did --------------------------------------------

	s0, err := statusOf(ctx, st, occs[0].ID)
	if err != nil {
		return err
	}
	s1, err := statusOf(ctx, st, occs[1].ID)
	if err != nil {
		return err
	}
	walkRow, err := st.GetOccurrence(ctx, occs[2].ID)
	if err != nil {
		return err
	}
	allThree := s0 == domain.StatusCompleted && s1 == domain.StatusCompleted &&
		walkRow.Status == domain.StatusSkipped
	fmt.Printf("  three resolved, one call  %s/%s/%s  %s\n",
		s0, s1, walkRow.Status, verdict(allThree))

	// R2: a skip carries its reason, and it is a skip rather than a miss. The
	// note is the user's own words, stored verbatim.
	noteOK := walkRow.ResolutionNote != nil && *walkRow.ResolutionNote == reason
	fmt.Printf("  skip carries its reason  note=%q  %s\n",
		noteOf(walkRow.ResolutionNote), verdict(noteOK && walkRow.Status != domain.StatusMissed))

	// The confirmation names items. Before this session bulk_resolve fell
	// through buildConfirmation's default case and answered `Done - "" is set.`
	// - three correct writes and a reply that named none of them.
	reply := sender.last()
	namesItems := strings.Contains(reply, stretch.Title) && strings.Contains(reply, walk.Title)
	fmt.Printf("  confirmation names them  %q  %s\n",
		truncate(reply, 70), verdict(namesItems && !strings.Contains(reply, `"" is set`)))

	// The agent's own transition series, which was a registered zero from
	// session 15 until this one: bulk_resolve wrote and counted nothing.
	metricsRec := httptest.NewRecorder()
	m.Handler().ServeHTTP(metricsRec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	body := metricsRec.Body.String()
	wantCompleted := `navi_occurrence_transitions_total{from="pending",source="agent",to="completed"} 2`
	wantSkipped := `navi_occurrence_transitions_total{from="pending",source="agent",to="skipped"} 1`
	fmt.Printf("  agent transitions counted  %s  %s\n",
		wantCompleted, verdict(strings.Contains(body, wantCompleted) && strings.Contains(body, wantSkipped)))

	// Q-4, and the answer to "what happens to what the reply did not mention":
	// nothing is re-asked and nothing is concluded. An item the reply skipped
	// over stays awaiting until grace, which reportGrace picks up from here.
	return nil
}

// reportGrace is K6, K7 and D-008 arriving together: missed, assigned only
// after a check-in asked and the grace window closed on the silence.
//
// The four cases are the four things that have to be true at once. A row inside
// its window is untouched. A row past it is missed, and carries 'reconciler' -
// the value migration 0004 added rather than borrowing 'sweeper'. A row nobody
// ever asked about is invisible no matter how old, which is the whole of "not
// by the clock passing midnight". And a reply landing inside the window
// prevents the miss outright.
//
// Instants are fabricated against the device zone, not DEFAULT_TZ: the deadline
// for a NULL grace_period_minutes is the end of the local day containing
// reconciled_at, and reportZones has already moved that zone five hours away.
func reportGrace(ctx context.Context, st *store.Store, tz string, fallback *time.Location, log *slog.Logger) error {
	fmt.Println("\ngrace  and then missed")

	zones, err := schedule.LoadZones(ctx, st, fallback)
	if err != nil {
		return err
	}
	loc := zones.Local()
	m := metrics.New()

	now := time.Now()
	r := reconciler.New(log.With("loop", "reconciler-grace"), st, nil, m, nil,
		now.In(loc).Format(reconciler.LocalTimeLayout), fallback)

	// Drain first, against a throwaway registry. Earlier sections have asked
	// about plenty of rows, and reportReconcileTiming deliberately fabricates
	// its passes on yesterday - so those rows are already past their end-of-day
	// deadline and would be missed by the first pass here, inflating every
	// count below. Clearing them up front is what makes "exactly two" a
	// statement about this section rather than about everything that ran before
	// it.
	drain := reconciler.New(log.With("loop", "reconciler-grace-drain"), st, nil, metrics.New(), nil,
		now.In(loc).Format(reconciler.LocalTimeLayout), fallback)
	if _, err := drain.ExpireGrace(ctx, now); err != nil {
		return err
	}

	// seed makes one item with one occurrence and stamps reconciled_at by hand
	// through RecordCheckIn, which is the same write a real pass makes. grace
	// is the item's grace_period_minutes; nil means "end of local day" (K7).
	seed := func(title string, grace *int, askedAt time.Time) (domain.Occurrence, error) {
		item, err := seedFireItem(ctx, st, title, domain.NotifySilent, tz)
		if err != nil {
			return domain.Occurrence{}, err
		}
		if grace != nil {
			if _, _, err := st.UpdateItemAndMaterialize(ctx, item.ID,
				store.ItemPatch{GracePeriodMinutes: grace}, time.Now(), nil); err != nil {
				return domain.Occurrence{}, err
			}
		}
		occ, err := fireOccurrence(ctx, st, item, askedAt.Add(-time.Hour), nil)
		if err != nil {
			return domain.Occurrence{}, err
		}
		// The latch value is irrelevant here and deliberately not advanced to
		// a real slot: this writes reconciled_at, which is the only input the
		// grace pass reads.
		if err := st.RecordCheckIn(ctx, []string{occ.ID}, "", nil, askedAt); err != nil {
			return domain.Occurrence{}, err
		}
		return occ, nil
	}

	// Inside its window: asked five minutes ago with two hours of grace.
	inside, err := seed("grace inside window", ptr(120), now.Add(-5*time.Minute))
	if err != nil {
		return err
	}
	// Past its window: asked an hour ago with five minutes of grace.
	expired, err := seed("grace expired", ptr(5), now.Add(-time.Hour))
	if err != nil {
		return err
	}
	// No grace configured, asked yesterday: the default is the end of the
	// local day the question was asked on, which is behind us.
	yesterday := now.AddDate(0, 0, -1)
	endOfDay, err := seed("grace end of day", nil, yesterday)
	if err != nil {
		return err
	}
	// Never asked about at all. Older than any of the above and still not
	// missable, because nothing ever put a question to it (K6).
	neverItem, err := seedFireItem(ctx, st, "grace never asked", domain.NotifySilent, tz)
	if err != nil {
		return err
	}
	neverAsked, err := fireOccurrence(ctx, st, neverItem, yesterday, nil)
	if err != nil {
		return err
	}

	// The deadline rule itself, before any pass runs: reconciled_at plus the
	// item's grace, or the end of that day. Read straight off the store method
	// both the grace pass and the agent's context block share.
	awaiting, err := st.ListAwaitingReconciliation(ctx, loc)
	if err != nil {
		return err
	}
	deadlines := map[string]time.Time{}
	for _, a := range awaiting {
		deadlines[a.ID] = a.Deadline
	}
	_, sawNever := deadlines[neverAsked.ID]
	fmt.Printf("  never asked is invisible  in awaiting set=%v  %s\n",
		sawNever, verdict(!sawNever))

	insideLive := deadlines[inside.ID].After(now)
	expiredDue := !deadlines[expired.ID].After(now)
	fmt.Printf("  deadlines resolved      inside=%s expired=%s  %s\n",
		deadlines[inside.ID].In(loc).Format("15:04"),
		deadlines[expired.ID].In(loc).Format("15:04"),
		verdict(insideLive && expiredDue))

	// --- a reply inside the window prevents the miss entirely ---------------

	// Resolved before the pass runs, the way an answer at 21:20 beats a
	// deadline at 21:30. Nothing about it is special-cased: it simply leaves
	// the candidate set by being terminal.
	answered, err := seed("grace answered in time", ptr(5), now.Add(-time.Hour))
	if err != nil {
		return err
	}
	if _, err := st.ResolveOccurrence(ctx, answered.ID, domain.StatusCompleted,
		ptr("answered the check-in"), domain.ResolvedByAgent, time.Now()); err != nil {
		return err
	}

	exp, err := r.ExpireGrace(ctx, now)
	if err != nil {
		return err
	}

	insideStatus, err := statusOf(ctx, st, inside.ID)
	if err != nil {
		return err
	}
	expiredRow, err := st.GetOccurrence(ctx, expired.ID)
	if err != nil {
		return err
	}
	endOfDayStatus, err := statusOf(ctx, st, endOfDay.ID)
	if err != nil {
		return err
	}
	neverStatus, err := statusOf(ctx, st, neverAsked.ID)
	if err != nil {
		return err
	}
	answeredStatus, err := statusOf(ctx, st, answered.ID)
	if err != nil {
		return err
	}

	fmt.Printf("  inside grace untouched  %s  %s\n",
		insideStatus, verdict(insideStatus != domain.StatusMissed))
	fmt.Printf("  past grace -> missed    %s / %s  %s\n",
		expiredRow.Status, endOfDayStatus,
		verdict(expiredRow.Status == domain.StatusMissed && endOfDayStatus == domain.StatusMissed))
	fmt.Printf("  never asked untouched   %s  (K6: not by the clock)              %s\n",
		neverStatus, verdict(neverStatus != domain.StatusMissed))
	fmt.Printf("  reply inside grace wins  %s  (answered before the deadline)      %s\n",
		answeredStatus, verdict(answeredStatus == domain.StatusCompleted))

	// The source is the reconciler's own, which is what migration 0004 was
	// for. Writing 'sweeper' here would have passed every other assertion on
	// this page and quietly corrupted the one column that answers Q-15.
	sourceOK := expiredRow.ResolutionSource != nil &&
		*expiredRow.ResolutionSource == domain.ResolvedByReconciler
	fmt.Printf("  resolution_source       %s  %s\n",
		sourceOf(expiredRow.ResolutionSource), verdict(sourceOK))

	// A miss carries no note. A note is why something did not happen, which is
	// what makes a skip a skip - and the defining property of a miss is that
	// nobody said anything.
	fmt.Printf("  missed carries no note  %s  %s\n",
		noteOf(expiredRow.ResolutionNote), verdict(expiredRow.ResolutionNote == nil))

	metricsRec := httptest.NewRecorder()
	m.Handler().ServeHTTP(metricsRec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	wantSeries := `navi_occurrence_transitions_total{from="pending",source="reconciler",to="missed"} 2`
	fmt.Printf("  transitions metric      %s  missed=%d  %s\n",
		wantSeries, exp.Missed,
		verdict(strings.Contains(metricsRec.Body.String(), wantSeries) && exp.Missed == 2))

	// --- idempotence, and the concurrent-resolution claim -------------------

	// A second pass writes nothing: the rows it missed are terminal now and
	// have left the candidate set, which is also exactly why an abort caused by
	// a concurrent resolution self-heals on the next tick rather than latching.
	again, err := r.ExpireGrace(ctx, now)
	if err != nil {
		return err
	}
	fmt.Printf("  second pass is a no-op  candidates=%d missed=%d  %s\n",
		again.Candidates, again.Missed, verdict(again.Missed == 0))

	// --- a global pause suppresses the whole pass (I6) ----------------------

	paused, err := seed("grace under pause", ptr(5), now.Add(-time.Hour))
	if err != nil {
		return err
	}
	if err := st.SetGlobalPauseUntil(ctx, now.Add(72*time.Hour)); err != nil {
		return err
	}
	pausedExp, err := r.ExpireGrace(ctx, now)
	if err != nil {
		return err
	}
	pausedStatus, err := statusOf(ctx, st, paused.ID)
	if err != nil {
		return err
	}
	fmt.Printf("  global pause suppresses  suppressed=%v missed=%d status=%s  %s\n",
		pausedExp.Paused, pausedExp.Missed, pausedStatus,
		verdict(pausedExp.Paused && pausedExp.Missed == 0 && pausedStatus != domain.StatusMissed))

	// Lifted, or reportPauseEndpoints inherits a pause it did not set.
	if err := st.ClearGlobalPause(ctx); err != nil {
		return err
	}

	// And once it lifts, the row that waited is missed - it was asked, and the
	// pause was a reason not to conclude anything yet rather than a reason to
	// forgive the question.
	after, err := r.ExpireGrace(ctx, now)
	if err != nil {
		return err
	}
	afterStatus, err := statusOf(ctx, st, paused.ID)
	if err != nil {
		return err
	}
	fmt.Printf("  and missed once lifted  %s  missed=%d  %s\n",
		afterStatus, after.Missed, verdict(afterStatus == domain.StatusMissed))

	return nil
}

// reportBriefing drives the morning briefing through all three of its phases -
// compose, send, evaluate - against a recording transport, the same no-network
// way reportReconcile drives the check-in.
//
// The things that have to be true at once: the text is composed ahead of the
// send and a restart in between loses none of it; a second pass after the send
// sends nothing; killing the model still produces a briefing, as the plain
// template; any inbound message clears the awaiting marker; and a window that
// closes with none is recorded unanswered without anything being marked missed.
//
// Clocks are fabricated against the device zone so every phase happens on one
// run whatever the wall clock says - reportZones has already moved that zone,
// so a fabricated instant has to be built in loc, not in DEFAULT_TZ.
func reportBriefing(ctx context.Context, st *store.Store, tz string, fallback *time.Location, log *slog.Logger) error {
	fmt.Println("\nbriefing  the morning briefing")

	zones, err := schedule.LoadZones(ctx, st, fallback)
	if err != nil {
		return err
	}
	loc := zones.Local()
	m := metrics.New()

	// The layout the briefing compares its send time with has to be the one
	// config validates BRIEFING_AT against - separate constants, same reason
	// reconciler.LocalTimeLayout duplicates it, same honesty check.
	fmt.Printf("  slot layout             config=%q briefing=%q  %s\n",
		config.LocalTimeLayout, briefing.LocalTimeLayout,
		verdict(config.LocalTimeLayout == briefing.LocalTimeLayout))

	if err := st.ClearGlobalPause(ctx); err != nil {
		return err
	}
	if err := st.ClearBriefingState(ctx); err != nil {
		return err
	}

	const at = "07:00"

	// A loud item and a silent one, plus one occurrence of each today, so the
	// briefing has something to say and "silent, no ping" is a real line.
	loud, err := seedFireItem(ctx, st, "briefing vitamins", domain.NotifyAtTime, tz)
	if err != nil {
		return err
	}
	silent, err := seedFireItem(ctx, st, "briefing stretch", domain.NotifySilent, tz)
	if err != nil {
		return err
	}
	base := time.Now().In(loc)
	noon := time.Date(base.Year(), base.Month(), base.Day(), 12, 0, 0, 0, loc)
	if _, err := fireOccurrence(ctx, st, loud, noon, ptr("take em")); err != nil {
		return err
	}
	if _, err := fireOccurrence(ctx, st, silent, noon, nil); err != nil {
		return err
	}

	day := base.Format(domain.DateLayout)
	composeNow := time.Date(base.Year(), base.Month(), base.Day(), 6, 40, 0, 0, loc)
	sendNow := time.Date(base.Year(), base.Month(), base.Day(), 7, 1, 0, 0, loc)

	// runCompose stages a briefing with one composer and returns the staged
	// text. Each call clears the slot first so the bodies are comparable.
	runCompose := func(c briefing.Composer) (string, error) {
		if err := st.ClearBriefingState(ctx); err != nil {
			return "", err
		}
		br := briefing.New(log.With("loop", "briefing-compose"), st, &recordingTransport{}, m, c, at, fallback)
		res, err := br.Run(ctx, composeNow)
		if err != nil {
			return "", err
		}
		if res.Phase != briefing.PhaseCompose {
			return "", fmt.Errorf("expected compose phase, got %q", res.Phase)
		}
		_, text, ok, err := st.BriefingPending(ctx)
		if err != nil || !ok {
			return "", fmt.Errorf("nothing staged: ok=%v err=%w", ok, err)
		}
		return text, nil
	}

	newComposer := func(srv *httptest.Server) briefing.Composer {
		routing := &model.Routing{Tasks: map[model.Task]model.TaskRouting{
			model.TaskBriefing: {Tiers: []model.Tier{
				{Model: "naviseed-briefing", BaseURL: srv.URL, TimeoutSeconds: 5},
			}},
		}}
		client := model.New(log.With("component", "model-briefing"), routing, "", st, m)
		return briefing.NewModelComposer(client, routing, "config/persona.md.does-not-exist")
	}

	// --- composition: model prose, and the fallback to template --------------

	templateText, err := runCompose(nil)
	if err != nil {
		return err
	}
	fmt.Printf("  no composer -> template  %q  %s\n",
		truncate(templateText, 56), verdict(templateText != ""))

	const composed = "Wednesday. Vitamins is on deck at noon; stretch is silent. Anything else for today?"
	okSrv := composeServer(composed)
	defer okSrv.Close()
	proseText, err := runCompose(newComposer(okSrv))
	if err != nil {
		return err
	}
	fmt.Printf("  composed briefing staged  %q  %s\n",
		truncate(proseText, 56), verdict(proseText == composed && proseText != templateText))

	downSrv := brokenServer()
	defer downSrv.Close()
	brokenText, err := runCompose(newComposer(downSrv))
	if err != nil {
		return err
	}
	fmt.Printf("  model down -> template   %q  %s\n",
		truncate(brokenText, 56), verdict(brokenText == templateText))

	// --- compose ahead of send, then a restart-safe send --------------------

	if err := st.ClearBriefingState(ctx); err != nil {
		return err
	}
	rec := &recordingTransport{}
	br := briefing.New(log.With("loop", "briefing"), st, rec, m, nil, at, fallback)

	cRes, err := br.Run(ctx, composeNow)
	if err != nil {
		return err
	}
	pDate, pText, pOK, err := st.BriefingPending(ctx)
	if err != nil {
		return err
	}
	lastAfterCompose, _, err := st.LastBriefingDate(ctx)
	if err != nil {
		return err
	}
	composedAhead := cRes.Phase == briefing.PhaseCompose && pOK && pDate == day &&
		len(rec.sent) == 0 && lastAfterCompose == ""
	fmt.Printf("  composed ~30m ahead     staged=%v sent=%d latch=%q  %s\n",
		pOK && pDate == day, len(rec.sent), lastAfterCompose, verdict(composedAhead))

	// The send phase, with the staged text left exactly as the compose phase
	// wrote it - this is the restart, minus the restart.
	sRes, err := br.Run(ctx, sendNow)
	if err != nil {
		return err
	}
	var body string
	if len(rec.sent) == 1 {
		body = rec.sent[0].Body
	}
	lastNow, _, err := st.LastBriefingDate(ctx)
	if err != nil {
		return err
	}
	awDate, _, awOK, err := st.BriefingAwaiting(ctx)
	if err != nil {
		return err
	}
	_, _, pendLeft, err := st.BriefingPending(ctx)
	if err != nil {
		return err
	}
	ref, refOK, err := st.LatestContextRef(ctx, store.ContextRefBriefing)
	if err != nil {
		return err
	}
	sentClean := sRes.Sent && len(rec.sent) == 1 && body == pText &&
		lastNow == day && awOK && awDate == day && !pendLeft &&
		refOK && ref == "briefing:"+day
	fmt.Printf("  sent the staged text    body==staged=%v latch=%q awaiting=%v pending-cleared=%v ref=%q  %s\n",
		body == pText, lastNow, awOK, !pendLeft, ref, verdict(sentClean))

	// A second pass on the same day sends nothing: last_briefing_date is the
	// latch, and it is durable.
	before := len(rec.sent)
	d3, err := br.Run(ctx, sendNow.Add(2*time.Minute))
	if err != nil {
		return err
	}
	fmt.Printf("  no double send          phase=%q sent=%d  %s\n",
		d3.Phase, len(rec.sent)-before, verdict(!d3.Sent && len(rec.sent) == before))

	// --- the compose window was missed entirely: template at send time ------

	if err := st.ClearBriefingState(ctx); err != nil {
		return err
	}
	rec2 := &recordingTransport{}
	br2 := briefing.New(log.With("loop", "briefing-missed"), st, rec2, m, nil, at, fallback)
	mRes, err := br2.Run(ctx, sendNow)
	if err != nil {
		return err
	}
	missBody := ""
	if len(rec2.sent) == 1 {
		missBody = rec2.sent[0].Body
	}
	fmt.Printf("  window missed -> sent    sent=%d fallback=%v names item=%v  %s\n",
		len(rec2.sent), mRes.Fallback, strings.Contains(missBody, loud.Title),
		verdict(mRes.Sent && mRes.Fallback && strings.Contains(missBody, loud.Title)))

	// --- no reply, grace window closes: recorded unanswered, nothing missed -
	//
	// This runs before the answered case on purpose: that case is the only
	// thing in this section that writes an inbound row, so running it second
	// keeps "no message has arrived since sent_at" true here without needing a
	// future-dated marker (which would sort to the top of the history the
	// conversation ladder loads later in the same run - the from_date-shaped
	// trap CLAUDE.md records).

	if err := st.ClearBriefingState(ctx); err != nil {
		return err
	}
	sentAt := time.Now()
	sentDay := sentAt.In(loc).Format(domain.DateLayout)
	if err := st.RecordBriefing(ctx, sentDay, "the briefing", "briefing:"+sentDay, sentAt); err != nil {
		return err
	}
	missedBefore, err := countAllMissed(ctx, st)
	if err != nil {
		return err
	}
	// A day and change past sent_at: past the end-of-local-day deadline K7 gives
	// a briefing with no per-item grace of its own.
	evUnans, err := br.EvaluateResponse(ctx, sentAt.Add(25*time.Hour))
	if err != nil {
		return err
	}
	_, _, stillMarked, err := st.BriefingAwaiting(ctx)
	if err != nil {
		return err
	}
	missedAfter, err := countAllMissed(ctx, st)
	if err != nil {
		return err
	}
	fmt.Printf("  no reply -> unanswered   unanswered=%v marker-cleared=%v occurrences-missed+%d  %s\n",
		evUnans.Unanswered, !stillMarked, missedAfter-missedBefore,
		verdict(evUnans.Unanswered && !stillMarked && missedAfter == missedBefore))

	// --- any inbound message clears the awaiting marker -------------------

	if err := st.ClearBriefingState(ctx); err != nil {
		return err
	}
	t1 := time.Now()
	if err := st.RecordBriefing(ctx, day, "today's briefing", "briefing:"+day, t1); err != nil {
		return err
	}
	if _, _, err := st.CreateConversation(ctx, domain.NewConversation{
		Role: domain.RoleUser, Content: "also remind me to call the dentist",
	}); err != nil {
		return err
	}
	evAns, err := br.EvaluateResponse(ctx, t1.Add(time.Minute))
	if err != nil {
		return err
	}
	_, _, stillAwaiting, err := st.BriefingAwaiting(ctx)
	if err != nil {
		return err
	}
	fmt.Printf("  any reply clears it      answered=%v unanswered=%v marker-cleared=%v  %s\n",
		evAns.Answered, evAns.Unanswered, !stillAwaiting,
		verdict(evAns.Answered && !evAns.Unanswered && !stillAwaiting))

	// --- a global pause suppresses every phase ----------------------------

	if err := st.ClearBriefingState(ctx); err != nil {
		return err
	}
	if err := st.SetGlobalPauseUntil(ctx, time.Now().Add(72*time.Hour)); err != nil {
		return err
	}
	recP := &recordingTransport{}
	brP := briefing.New(log.With("loop", "briefing-paused"), st, recP, m, nil, at, fallback)
	pRun, err := brP.Run(ctx, composeNow)
	if err != nil {
		return err
	}
	pEval, err := brP.EvaluateResponse(ctx, composeNow)
	if err != nil {
		return err
	}
	_, _, pStaged, err := st.BriefingPending(ctx)
	if err != nil {
		return err
	}
	fmt.Printf("  global pause suppresses  run-paused=%v eval-paused=%v staged=%v sent=%d  %s\n",
		pRun.Paused, pEval.Paused, pStaged, len(recP.sent),
		verdict(pRun.Paused && pEval.Paused && !pStaged && len(recP.sent) == 0))
	if err := st.ClearGlobalPause(ctx); err != nil {
		return err
	}

	// --- the two counters, mirroring the reconciler's --------------------

	metricsRec := httptest.NewRecorder()
	m.Handler().ServeHTTP(metricsRec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	out := metricsRec.Body.String()
	wantSent := strings.Contains(out, "navi_briefing_sent_total 2")
	wantFallback := strings.Contains(out, "navi_briefing_fallback_total 4")
	wantUnanswered := strings.Contains(out, "navi_briefing_unanswered_total 1")
	fmt.Printf("  metrics                 sent=2 fallback=4 unanswered=1  %s\n",
		verdict(wantSent && wantFallback && wantUnanswered))

	if err := st.ClearBriefingState(ctx); err != nil {
		return err
	}
	return nil
}

// countAllMissed is the occurrence-missed total across the whole database, so
// the briefing's unanswered evaluation can be shown to add nothing to it.
func countAllMissed(ctx context.Context, st *store.Store) (int, error) {
	items, err := st.ListActiveItems(ctx)
	if err != nil {
		return 0, err
	}
	ids := make([]string, 0, len(items))
	for _, it := range items {
		ids = append(ids, it.ID)
	}
	return countStatus(ctx, st, ids, domain.StatusMissed)
}

// reportPauseEndpoints drives POST /api/items/{id}/pause and POST /api/pause
// through a real httpapi server, the way reportResolve drives resolution.
//
// The state they write is the state reportPause above already checked by
// setting it by hand; what is new here is the way to set it, and the two things
// worth asserting are that a pause clears the window's pending rows through the
// endpoint rather than through a second code path, and that lifting one brings
// them back.
func reportPauseEndpoints(ctx context.Context, st *store.Store, tz string, fallback *time.Location, log *slog.Logger) error {
	fmt.Println("\npause  POST /api/items/{id}/pause and POST /api/pause")

	m := metrics.New()
	mat := materializer.New(log.With("component", "mat-pause"), st, fallback)
	srv := httpapi.New(config.HTTP{Addr: ":0"}, log.With("component", "httpapi"),
		health.New(), m, st, mat, time.Time{}, fallback, nil)

	post := func(path, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		srv.Handler.ServeHTTP(rec, req.WithContext(ctx))
		return rec
	}

	// A daily item, so there is a fortnight of pending rows for a pause to have
	// an effect on. The one-off probes the fire path uses would show nothing.
	item, err := seedPauseItem(ctx, st, tz)
	if err != nil {
		return err
	}
	if _, err := mat.Item(ctx, item.ID); err != nil {
		return err
	}

	// The window the request asks for and the window this counts have to be the
	// same instants, or a row on the boundary day reads as a pause that did not
	// work. The endpoint resolves the date to local midnight in the *device*
	// zone, so this builds it the same way rather than formatting an instant in
	// DEFAULT_TZ and hoping the two agree - reportZones has already moved the
	// device zone five hours away from it.
	zones, err := schedule.LoadZones(ctx, st, fallback)
	if err != nil {
		return err
	}
	loc := zones.Local()

	now := time.Now()
	local := now.In(loc)
	until := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, loc).AddDate(0, 0, 7)
	untilDate := until.Format(domain.DateLayout)
	window := func() (int, error) { return countBetween(ctx, st, item, now, until) }

	before, err := window()
	if err != nil {
		return err
	}

	// A resolved row in the past and an override in the window, both of which
	// the pause must leave alone: history is immutable (invariant 2) and an
	// override is a deliberate exception the materializer never touches.
	history, err := fireOccurrence(ctx, st, item, now.Add(-48*time.Hour), nil)
	if err != nil {
		return err
	}
	if _, err := st.ResolveOccurrence(ctx, history.ID, domain.StatusCompleted, nil,
		domain.ResolvedByWeb, time.Now()); err != nil {
		return err
	}
	override, err := fireOccurrence(ctx, st, item, now.Add(72*time.Hour), nil)
	if err != nil {
		return err
	}

	paused := post("/api/items/"+item.ID+"/pause", `{"until":"`+untilDate+`"}`)
	during, err := window()
	if err != nil {
		return err
	}
	fmt.Printf("  item paused until %s  status %d  window %d -> %d  deleted=%d  %s\n",
		untilDate, paused.Code, before, during, pausedDeleted(paused.Body.Bytes()),
		verdict(paused.Code == http.StatusOK && before > 0 && during == 0))

	historyStatus, err := statusOf(ctx, st, history.ID)
	if err != nil {
		return err
	}
	overrideStatus, err := statusOf(ctx, st, override.ID)
	if err != nil {
		return err
	}
	fmt.Printf("  history and overrides   completed row=%s override row=%s  %s\n",
		historyStatus, overrideStatus,
		verdict(historyStatus == domain.StatusCompleted && overrideStatus == domain.StatusPending))

	lifted := post("/api/items/"+item.ID+"/pause", `{"until":null}`)
	after, err := window()
	if err != nil {
		return err
	}
	fmt.Printf("  item unpaused           status %d  window %d  %s   (redrawn: D-005)\n",
		lifted.Code, after, verdict(lifted.Code == http.StatusOK && after > 0))

	// Global, and then the fire path, which is the criterion the roadmap
	// actually states: pausing suppresses notifications.
	global := post("/api/pause", `{"until":"`+untilDate+`"}`)
	globalWindow, err := window()
	if err != nil {
		return err
	}
	due, err := fireOccurrence(ctx, st, item, now.Add(-time.Minute), ptr("paused body"))
	if err != nil {
		return err
	}
	rec := &recordingTransport{}
	sched := scheduler.New(log.With("component", "scheduler-pause"), st, rec, m, time.Now())
	fired, err := sched.Fire(ctx)
	if err != nil {
		return err
	}
	dueStatus, err := statusOf(ctx, st, due.ID)
	if err != nil {
		return err
	}
	fmt.Printf("  global pause            status %d  window %d  fire paused=%v sent=%d row still %s  %s\n",
		global.Code, globalWindow, fired.Paused, len(rec.sent), dueStatus,
		verdict(global.Code == http.StatusOK && globalWindow == 0 && fired.Paused &&
			len(rec.sent) == 0 && dueStatus == domain.StatusPending))

	unpaused := post("/api/pause", `{"until":null}`)
	globalAfter, err := window()
	if err != nil {
		return err
	}
	fmt.Printf("  global unpaused         status %d  window %d  %s\n",
		unpaused.Code, globalAfter,
		verdict(unpaused.Code == http.StatusOK && globalAfter > 0))

	bad := post("/api/pause", `{"until":"next monday"}`)
	fmt.Printf("  malformed until         status %d  %s\n",
		bad.Code, verdict(bad.Code == http.StatusBadRequest))
	return nil
}

// seedPauseItem creates a plain daily reminder on first run and reuses it
// afterwards - something with enough future rows for a pause window to empty.
func seedPauseItem(ctx context.Context, st *store.Store, tz string) (domain.Item, error) {
	const title = "pause endpoint probe"

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
		Schedule: json.RawMessage(`{"kind":"fixed","rrule":"FREQ=DAILY","at":"07:30"}`),
		TZ:       tz,
	})
}

// pausedDeleted reads occurrences_deleted off a pause response.
func pausedDeleted(body []byte) int {
	var out struct {
		OccurrencesDeleted int `json:"occurrences_deleted"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return -1
	}
	return out.OccurrencesDeleted
}

// reconciledAt reports whether the check-in has already asked about a row.
func reconciledAt(ctx context.Context, st *store.Store, id string) (bool, error) {
	occ, err := st.GetOccurrence(ctx, id)
	if err != nil {
		return false, err
	}
	return occ.ReconciledAt != nil, nil
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
		routingPath, taskCount, verdict(loadErr == nil && taskCount == 6))

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
	m := metrics.New()
	tools := agent.New(st, mat, table, defaultTZ, m)
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

	// Nil registry, which is the case agent.Metrics is nil-able for: this
	// section drives the catalog and the validation layers, and none of what it
	// asserts is a counter. reportBulkResolve is where the transitions are
	// checked.
	t := agent.New(st, mat, table, defaultTZ, nil)

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
	if err := reportPauseTool(ctx, t, st); err != nil {
		return err
	}
	return reportDeleteItem(ctx, t, st)
}

// reportPauseTool drives the pause tool at both scopes, plus the two Layer 2
// rejections that are the whole of its semantic validation.
//
// The tool and the endpoint reach the same two materializer entry points, so
// what is worth checking here is the argument handling the endpoint does not
// share: that scope and item_id have to agree, and that a global pause records
// no last touched item because it resolved to none.
func reportPauseTool(ctx context.Context, t *agent.Tools, st *store.Store) error {
	fmt.Println("\nagent pause")

	item, err := callTool(ctx, t, "create_item", agent.CreateItemArgs{
		Title:    "agent pause probe",
		Schedule: schedule.Schedule{Kind: schedule.KindFixed, RRule: ptr("FREQ=DAILY"), At: ptr("08:15")},
	})
	if err != nil {
		return err
	}
	id := item.Item.ID

	until := time.Now().AddDate(0, 0, 5).Format(domain.DateLayout)
	scoped, err := callTool(ctx, t, "pause", agent.PauseArgs{Scope: "item", Until: until, ItemID: &id})
	if err != nil {
		return err
	}
	paused, err := st.GetItem(ctx, id)
	if err != nil {
		return err
	}
	fmt.Printf("  scope=item until %s   paused_until=%s deleted=%d  %s\n",
		until, presence(paused.PausedUntil != nil), scoped.Applied.Deleted,
		verdict(paused.PausedUntil != nil && scoped.Applied.Deleted > 0))

	touched, _, err := st.LastTouchedItemID(ctx)
	if err != nil {
		return err
	}
	fmt.Printf("  last touched recorded  %v  %s\n", touched == id, verdict(touched == id))

	if _, err := callTool(ctx, t, "pause", agent.PauseArgs{Scope: "item", ItemID: &id}); err != nil {
		return err
	}
	lifted, err := st.GetItem(ctx, id)
	if err != nil {
		return err
	}
	fmt.Printf("  empty until resumes    paused_until=%s  %s\n",
		presence(lifted.PausedUntil != nil), verdict(lifted.PausedUntil == nil))

	// scope=global writes kv rather than a column, and touches no item.
	if _, err := callTool(ctx, t, "pause", agent.PauseArgs{Scope: "global", Until: until}); err != nil {
		return err
	}
	globalUntil, set, err := st.GlobalPauseUntil(ctx)
	if err != nil {
		return err
	}
	afterGlobal, _, err := st.LastTouchedItemID(ctx)
	if err != nil {
		return err
	}
	fmt.Printf("  scope=global until %s  set=%v last touched unchanged=%v  %s\n",
		until, set, afterGlobal == touched,
		verdict(set && globalUntil.After(time.Now()) && afterGlobal == touched))

	if _, err := callTool(ctx, t, "pause", agent.PauseArgs{Scope: "global"}); err != nil {
		return err
	}
	_, stillSet, err := st.GlobalPauseUntil(ctx)
	if err != nil {
		return err
	}
	fmt.Printf("  global resumed         key cleared=%v  %s\n", !stillSet, verdict(!stillSet))

	// The two Layer 2 rejections. Neither reaches a write.
	_, missingID := callTool(ctx, t, "pause", agent.PauseArgs{Scope: "item", Until: until})
	_, strayID := callTool(ctx, t, "pause", agent.PauseArgs{Scope: "global", Until: until, ItemID: &id})
	_, badDate := callTool(ctx, t, "pause", agent.PauseArgs{Scope: "global", Until: "next monday"})
	var ve *domain.ValidationError
	rejected := errors.As(missingID, &ve) && errors.As(strayID, &ve) && errors.As(badDate, &ve)
	fmt.Printf("  layer 2 rejections     item without id, global with id, unparseable date  %s\n",
		verdict(rejected))
	if errors.As(missingID, &ve) {
		fmt.Printf("    %s\n", ve.Message)
	}
	return nil
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

// reportGoals is P3.5: item-linked progress read live from the chains view,
// freestanding progress as the newest goal_updates row, every validation-table
// row, and the sweeper's period-end pass. Every number it checks is derived, so
// the section is mostly "make something true in the data, read it back the long
// way, and confirm it agrees."
func reportGoals(ctx context.Context, st *store.Store, table *defaults.Table, tz string, defaultTZ *time.Location, log *slog.Logger) error {
	fmt.Println("\ngoals (P3.5)")

	mat := materializer.New(log.With("component", "materializer-goals"), st, defaultTZ)
	t := agent.New(st, mat, table, defaultTZ, nil)

	// The person's zone, resolved exactly as create_goal and the sweeper resolve
	// it. reportZones has already moved kv.current_tz to Europe/Lisbon, so an
	// expected period date computed here must use this and not DEFAULT_TZ - the
	// shape of the from_date bug that survived twelve sessions.
	zones, err := schedule.LoadZones(ctx, st, defaultTZ)
	if err != nil {
		return err
	}
	loc := zones.Local()
	now := time.Now()
	today := now.In(loc)
	mondayOffset := (int(today.Weekday()) + 6) % 7

	// ---- item-linked: "gym four times this week" ----
	gym, err := callTool(ctx, t, "create_item", agent.CreateItemArgs{
		Title:    "gym (goal probe)",
		Schedule: schedule.Schedule{Kind: schedule.KindFixed, RRule: ptr("FREQ=DAILY"), At: ptr("18:00")},
	})
	if err != nil {
		return err
	}
	gymID := gym.Item.ID

	created, err := callTool(ctx, t, "create_goal", agent.CreateGoalArgs{
		Title:       "gym four times this week",
		PeriodKind:  "week",
		ItemID:      &gymID,
		TargetCount: ptr(4),
	})
	if err != nil {
		return fmt.Errorf("naviseed: create_goal item-linked: %w", err)
	}
	goal := created.Goal
	if goal == nil || created.GoalProgress == nil {
		fmt.Printf("  create item-linked week goal   %s\n", verdict(false))
		return fmt.Errorf("naviseed: create_goal returned no goal/progress")
	}

	monday := today.AddDate(0, 0, -mondayOffset).Format(domain.DateLayout)
	sunday := today.AddDate(0, 0, 6-mondayOffset).Format(domain.DateLayout)
	periodOK := goal.PeriodStart == monday && goal.PeriodEnd == sunday &&
		goal.IsItemLinked() && goal.TargetCount != nil && *goal.TargetCount == 4
	fmt.Printf("  create item-linked week goal   period %s..%s target 4  %s\n",
		goal.PeriodStart, goal.PeriodEnd, verdict(periodOK))
	fmt.Printf("  progress reads from chains     %d/%d, no goal_updates rows  %s\n",
		created.GoalProgress.Completed, created.GoalProgress.Target,
		verdict(created.GoalProgress.Completed == 0 && created.GoalProgress.Target == 4))

	fromUTC, toExclUTC, err := domain.GoalPeriodBounds(goal.PeriodStart, goal.PeriodEnd, loc)
	if err != nil {
		return err
	}
	occs, err := st.ListOccurrencesForItem(ctx, gymID)
	if err != nil {
		return err
	}
	completed := 0
	for _, o := range occs {
		if completed == 2 {
			break
		}
		if o.Status != domain.StatusPending || o.StartsAt.Before(fromUTC) || !o.StartsAt.Before(toExclUTC) {
			continue
		}
		if _, err := st.ResolveOccurrence(ctx, o.ID, domain.StatusCompleted, nil, domain.ResolvedByAgent, time.Now()); err != nil {
			return err
		}
		completed++
	}

	goalAfter, err := st.GetGoal(ctx, goal.ID)
	if err != nil {
		return err
	}
	progAfter, err := st.GoalProgressFor(ctx, goalAfter, loc)
	if err != nil {
		return err
	}
	noGoalWrite := goalAfter.UpdatedAt.Equal(goal.UpdatedAt)
	fmt.Printf("  completing occurrences moves it %d/%d, goals.updated_at unchanged=%v  %s\n",
		progAfter.Completed, progAfter.Target, noGoalWrite,
		verdict(progAfter.Completed == completed && noGoalWrite))

	// Independent hand count over the same window, re-reading the rows: no snooze
	// chains here, so a completed chain is just a completed occurrence.
	fresh, err := st.ListOccurrencesForItem(ctx, gymID)
	if err != nil {
		return err
	}
	hand := 0
	for _, o := range fresh {
		if o.Status == domain.StatusCompleted && !o.StartsAt.Before(fromUTC) && o.StartsAt.Before(toExclUTC) {
			hand++
		}
	}
	fmt.Printf("  progress matches a hand count  chains=%d hand=%d  %s\n",
		progAfter.Completed, hand, verdict(progAfter.Completed == hand))

	vel := progAfter.Velocity(now, loc)
	velVal := -1.0
	if vel != nil {
		velVal = *vel
	}
	fmt.Printf("  velocity per elapsed week      %.1f (hand count %d)  %s\n",
		velVal, hand, verdict(vel != nil && *vel == float64(hand)))

	// ---- freestanding: "ship the report by Friday" ----
	fridayStr := today.AddDate(0, 0, (int(time.Friday)-int(today.Weekday())+7)%7).Format(domain.DateLayout)
	if fridayStr == today.Format(domain.DateLayout) {
		fridayStr = today.AddDate(0, 0, 7).Format(domain.DateLayout)
	}
	fs, err := callTool(ctx, t, "create_goal", agent.CreateGoalArgs{
		Title:      "ship the report by Friday",
		PeriodKind: "custom",
		PeriodEnd:  &fridayStr,
	})
	if err != nil {
		return fmt.Errorf("naviseed: create_goal freestanding: %w", err)
	}
	fsGoal := fs.Goal
	if fsGoal == nil {
		return fmt.Errorf("naviseed: create_goal freestanding returned no goal")
	}
	fmt.Printf("  create freestanding goal       custom ..%s, no target  %s\n",
		fridayStr, verdict(!fsGoal.IsItemLinked() && fsGoal.TargetCount == nil))

	if _, err := callTool(ctx, t, "log_goal_progress", agent.LogGoalProgressArgs{
		GoalID: fsGoal.ID, ProgressPct: ptr(40), Note: ptr("outline done"),
	}); err != nil {
		return fmt.Errorf("naviseed: log_goal_progress 40: %w", err)
	}
	loggedRes, err := callTool(ctx, t, "log_goal_progress", agent.LogGoalProgressArgs{
		GoalID: fsGoal.ID, ProgressPct: ptr(60),
	})
	if err != nil {
		return fmt.Errorf("naviseed: log_goal_progress 60: %w", err)
	}
	lp := loggedRes.GoalProgress
	fmt.Printf("  conversational update appends   current = newest of two rows = %s%%  %s\n",
		goalPctText(lp), verdict(lp != nil && lp.LatestPct != nil && *lp.LatestPct == 60))

	// Current progress is the newest row, never a stored max: a third, lower
	// update becomes current immediately.
	if _, err := st.AppendGoalUpdate(ctx, domain.NewGoalUpdate{
		GoalID: fsGoal.ID, ProgressPct: ptr(55), Source: domain.GoalUpdateByWeb,
	}, time.Now()); err != nil {
		return err
	}
	reread, err := st.GoalProgressFor(ctx, *fsGoal, loc)
	if err != nil {
		return err
	}
	fmt.Printf("  newest row wins even when lower  55%% after 60%%  %s\n",
		verdict(reread.LatestPct != nil && *reread.LatestPct == 55))

	// ---- validation table: every row rejects with a *domain.ValidationError ----
	fmt.Println("  validation table")
	var ve *domain.ValidationError
	badStart := today.AddDate(0, 0, 3).Format(domain.DateLayout)
	badEnd := today.Format(domain.DateLayout)
	wed := today.AddDate(0, 0, (int(time.Wednesday)-int(today.Weekday())+7)%7).Format(domain.DateLayout)
	if wed == monday {
		wed = today.AddDate(0, 0, 2).Format(domain.DateLayout)
	}
	badItem := "itm_does_not_exist"
	rows := []struct {
		name string
		run  func() error
	}{
		{"period unordered (custom)", func() error {
			_, e := callTool(ctx, t, "create_goal", agent.CreateGoalArgs{Title: "x", PeriodKind: "custom", PeriodStart: &badStart, PeriodEnd: &badEnd})
			return e
		}},
		{"week range misaligned", func() error {
			_, e := callTool(ctx, t, "create_goal", agent.CreateGoalArgs{Title: "x", PeriodKind: "week", PeriodStart: &wed})
			return e
		}},
		{"item-linked, no target", func() error {
			_, e := callTool(ctx, t, "create_goal", agent.CreateGoalArgs{Title: "x", PeriodKind: "week", ItemID: &gymID})
			return e
		}},
		{"freestanding, with target", func() error {
			_, e := callTool(ctx, t, "create_goal", agent.CreateGoalArgs{Title: "x", PeriodKind: "week", TargetCount: ptr(3)})
			return e
		}},
		{"item_id does not resolve", func() error {
			_, e := callTool(ctx, t, "create_goal", agent.CreateGoalArgs{Title: "x", PeriodKind: "week", ItemID: &badItem, TargetCount: ptr(2)})
			return e
		}},
		{"custom without period_end", func() error {
			_, e := callTool(ctx, t, "create_goal", agent.CreateGoalArgs{Title: "x", PeriodKind: "custom"})
			return e
		}},
		{"log_goal_progress, neither field", func() error {
			_, e := callTool(ctx, t, "log_goal_progress", agent.LogGoalProgressArgs{GoalID: fsGoal.ID})
			return e
		}},
	}
	for _, r := range rows {
		e := r.run()
		ok := errors.As(e, &ve)
		msg := ""
		if ok {
			msg = ve.Rule + ": " + ve.Message
		}
		fmt.Printf("    %-28s %s  %s\n", r.name, verdict(ok), msg)
	}

	// terminal-goal guard: abandon a goal, then confirm both writers bounce.
	ab, err := callTool(ctx, t, "create_goal", agent.CreateGoalArgs{Title: "to abandon", PeriodKind: "week"})
	if err != nil {
		return err
	}
	if _, err := callTool(ctx, t, "update_goal", agent.UpdateGoalArgs{
		GoalID: ab.Goal.ID, Changes: agent.GoalChanges{Status: ptr("abandoned")},
	}); err != nil {
		return fmt.Errorf("naviseed: abandon goal: %w", err)
	}
	_, uErr := callTool(ctx, t, "update_goal", agent.UpdateGoalArgs{GoalID: ab.Goal.ID, Changes: agent.GoalChanges{Title: ptr("nope")}})
	_, lErr := callTool(ctx, t, "log_goal_progress", agent.LogGoalProgressArgs{GoalID: ab.Goal.ID, Note: ptr("still going")})
	fmt.Printf("    %-28s %s\n", "update_goal on terminal", verdict(errors.As(uErr, &ve)))
	fmt.Printf("    %-28s %s\n", "log_goal_progress on terminal", verdict(errors.As(lErr, &ve)))

	// ---- period-end evaluation: the sweeper's pass, not the clock ----
	fmt.Println("  period-end evaluation")
	lastMon := today.AddDate(0, 0, -7-mondayOffset)
	lwStart := lastMon.Format(domain.DateLayout)
	lwEnd := lastMon.AddDate(0, 0, 6).Format(domain.DateLayout)

	missGoal, err := callTool(ctx, t, "create_goal", agent.CreateGoalArgs{
		Title: "gym last week", PeriodKind: "custom",
		PeriodStart: &lwStart, PeriodEnd: &lwEnd, ItemID: &gymID, TargetCount: ptr(3),
	})
	if err != nil {
		return err
	}

	metItem, err := seedFireItem(ctx, st, "gym met probe", domain.NotifySilent, tz)
	if err != nil {
		return err
	}
	metItemID := metItem.ID
	pastAt := time.Date(lastMon.Year(), lastMon.Month(), lastMon.Day()+2, 12, 0, 0, 0, loc)
	pastOcc, err := fireOccurrence(ctx, st, metItem, pastAt, nil)
	if err != nil {
		return err
	}
	if _, err := st.ResolveOccurrence(ctx, pastOcc.ID, domain.StatusCompleted, nil, domain.ResolvedByAgent, time.Now()); err != nil {
		return err
	}
	metGoal, err := callTool(ctx, t, "create_goal", agent.CreateGoalArgs{
		Title: "met last week", PeriodKind: "custom",
		PeriodStart: &lwStart, PeriodEnd: &lwEnd, ItemID: &metItemID, TargetCount: ptr(1),
	})
	if err != nil {
		return err
	}

	fsMiss, err := callTool(ctx, t, "create_goal", agent.CreateGoalArgs{
		Title: "unreported last week", PeriodKind: "custom", PeriodStart: &lwStart, PeriodEnd: &lwEnd,
	})
	if err != nil {
		return err
	}
	fsMet, err := callTool(ctx, t, "create_goal", agent.CreateGoalArgs{
		Title: "done last week", PeriodKind: "custom", PeriodStart: &lwStart, PeriodEnd: &lwEnd,
	})
	if err != nil {
		return err
	}
	if _, err := st.AppendGoalUpdate(ctx, domain.NewGoalUpdate{
		GoalID: fsMet.Goal.ID, ProgressPct: ptr(100), Source: domain.GoalUpdateByAgent,
	}, time.Now()); err != nil {
		return err
	}

	sw := sweeper.New(log.With("component", "sweeper-goals"), st, mat, defaultTZ)
	if err := sw.Tick(ctx); err != nil {
		return err
	}

	check := func(label, id string, want domain.GoalStatus) {
		g, e := st.GetGoal(ctx, id)
		fmt.Printf("    %-28s %-7s (want %s)  %s\n", label, g.Status, want, verdict(e == nil && g.Status == want))
	}
	check("item-linked short", missGoal.Goal.ID, domain.GoalMissed)
	check("item-linked reached", metGoal.Goal.ID, domain.GoalMet)
	check("freestanding, no updates", fsMiss.Goal.ID, domain.GoalMissed)
	check("freestanding 100pct", fsMet.Goal.ID, domain.GoalMet)
	check("current-week goal untouched", goal.ID, domain.GoalActive)

	eval2, err := st.EvaluateDueGoals(ctx, time.Now(), loc)
	if err != nil {
		return err
	}
	fmt.Printf("    %-28s considered=%d met=%d missed=%d  %s\n", "second pass is a no-op",
		eval2.Considered, eval2.Met, eval2.Missed, verdict(eval2.Met == 0 && eval2.Missed == 0))

	return nil
}

// goalPctText renders a store.GoalProgress' latest percent for a log line.
func goalPctText(p *store.GoalProgress) string {
	if p == nil || p.LatestPct == nil {
		return "?"
	}
	return fmt.Sprintf("%d", *p.LatestPct)
}

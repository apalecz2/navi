package schedule

import (
	"context"
	"time"

	"github.com/aidenpaleczny/navi/internal/domain"
)

// Zones answers one question: which time.Location does this item's wall clock
// mean (C5, C6).
//
// Only which zone. Turning a local wall clock into a UTC instant, and the two
// DST edge cases docs/05-schedule-spec.md#dst spells out, are Instant's in
// dst.go — this answers where, that answers when.
//
// It is a value rather than a lookup against the database because the
// materializer resolves a zone once per run for every item, and reading
// kv.current_tz once at the top of the run is both cheaper and more correct: a
// run that straddled a timezone change would otherwise place half its
// occurrences in each.
type Zones struct {
	// Device is kv.current_tz, the zone the user is currently in. Nil when the
	// key has never been set, which is the state of a fresh database.
	Device *time.Location

	// Fallback is the deployment default, cfg.Schedule.DefaultTZ. It is never
	// nil in a running process: config.Load requires DEFAULT_TZ and fails
	// startup without it.
	Fallback *time.Location
}

// For returns the location an item's wall-clock times resolve against.
//
// A fixed item is always its own zone: that is what fixed means, and it is
// correct for a standing call with someone in another country.
//
// A floating item follows the device, which is the default and the right
// behaviour for stretching, meals and medication. The chain when the device
// zone is unknown runs device, then the item's own zone, then the deployment
// default — and the middle rung is not a consolation prize, because an item's tz
// column was written from the device zone at creation, so it is the last known
// good answer to the same question.
func (z Zones) For(item domain.Item) (*time.Location, error) {
	if item.TZMode == domain.TZModeFixed {
		return z.load(item.TZ)
	}

	if z.Device != nil {
		return z.Device, nil
	}
	if item.TZ != "" {
		return z.load(item.TZ)
	}
	if z.Fallback != nil {
		return z.Fallback, nil
	}
	return nil, domain.Invalid("timezone_valid", "tz",
		"item %s is floating and no timezone is known: current_tz is unset, the item has none, and there is no default", item.ID)
}

// load resolves an IANA name, reporting the failure in the form the escalation
// ladder wants rather than as the tzdata package's wording.
func (z Zones) load(name string) (*time.Location, error) {
	if name == "" {
		return nil, domain.Invalid("timezone_valid", "tz", "timezone is required")
	}
	loc, err := time.LoadLocation(name)
	if err != nil {
		return nil, domain.Invalid("timezone_valid", "tz",
			"timezone %q is not an IANA location", name)
	}
	return loc, nil
}

// LoadLocation resolves an IANA name for a caller that has a name rather than
// an item — the set_timezone tool in P1, and the store when it reads
// kv.current_tz. Same message, one implementation.
func LoadLocation(name string) (*time.Location, error) {
	return Zones{}.load(name)
}

// TZReader is the one thing LoadZones needs from a database: kv.current_tz, and
// whether it has ever been set. Declared here rather than imported so this
// package keeps its direction of dependency — store reaches schedule, never the
// other way.
type TZReader interface {
	CurrentTZ(ctx context.Context) (string, bool, error)
}

// LoadZones reads the device zone once and returns the value every wall clock in
// this run resolves against.
//
// It is the beginning the materializer already writes by hand and that both
// snooze surfaces need: kv.current_tz if it is set, the deployment default
// underneath, and the item's own zone in between — that last rung being For's
// business and not this function's. Reading it once, here, before any write
// transaction opens, is what stops a run that straddled a timezone change from
// placing half its rows in each.
func LoadZones(ctx context.Context, r TZReader, fallback *time.Location) (Zones, error) {
	z := Zones{Fallback: fallback}

	name, ok, err := r.CurrentTZ(ctx)
	if err != nil {
		return Zones{}, err
	}
	if !ok {
		return z, nil
	}

	device, err := LoadLocation(name)
	if err != nil {
		return Zones{}, err
	}
	z.Device = device
	return z, nil
}

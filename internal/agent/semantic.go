package agent

import (
	"context"
	"time"

	"github.com/aidenpaleczny/navi/internal/defaults"
	"github.com/aidenpaleczny/navi/internal/domain"
	"github.com/aidenpaleczny/navi/internal/schedule"
)

// Layer 2, the semantic pass (docs/06-agent-spec.md#validation): the table
// in docs/05-schedule-spec.md#validation, wired in rather than reimplemented,
// plus the one check that table cannot cover on its own - a referenced
// item_id or occurrence_id resolves to a live row - which lives in
// internal/store (store.LiveItem, store.LiveOccurrence) because
// domain.ValidationError's own doc comment already names the store as one of
// the three places this error type comes from.

// resolveSchedule runs the write path's Resolve-then-Validate pair directly
// rather than through schedule.Prepare. Every caller here already holds a
// parsed schedule.Schedule - decoded through the tool argument's own
// json.Unmarshaler at Layer 1 - so re-marshaling it back to json.RawMessage
// just to have Prepare's Parse step decode it again would be pure overhead.
// Same two exported functions Prepare composes, in the same order, with the
// same result.
func resolveSchedule(s schedule.Schedule, table *defaults.Table, ref time.Time, loc *time.Location) (schedule.Schedule, []schedule.Inference, error) {
	resolved, inferences, err := schedule.Resolve(s, table)
	if err != nil {
		return schedule.Schedule{}, nil, err
	}
	if err := schedule.Validate(resolved, ref, loc); err != nil {
		return schedule.Schedule{}, inferences, err
	}
	return resolved, inferences, nil
}

// resolveCreateZone answers which location a new item's wall clock means,
// before the item exists to ask schedule.Zones.For about. TZ named
// explicitly wins; otherwise the device zone (kv.current_tz) is read the
// same way materializer.begin reads it for a floating item, since a device
// zone is the tool-catalog's best guess at "the current device timezone" the
// doc's tz field comment promises; the deployment default is the last rung.
func (t *Tools) resolveCreateZone(ctx context.Context, args CreateItemArgs) (*time.Location, string, error) {
	if args.TZ != nil && *args.TZ != "" {
		loc, err := schedule.LoadLocation(*args.TZ)
		if err != nil {
			return nil, "", err
		}
		return loc, *args.TZ, nil
	}
	if name, ok, err := t.store.CurrentTZ(ctx); err == nil && ok {
		if loc, err := schedule.LoadLocation(name); err == nil {
			return loc, name, nil
		}
	}
	return t.defaultTZ, t.defaultTZ.String(), nil
}

// zoneFor answers the same question for a live item, matching
// materializer.begin's own resolution exactly - device zone first, item's
// own tz next, deployment default last (schedule.Zones.For's ladder) - so
// Layer 2 validates a schedule against the same zone the materializer will
// actually place it in.
func (t *Tools) zoneFor(ctx context.Context, item domain.Item) (*time.Location, error) {
	zones := schedule.Zones{Fallback: t.defaultTZ}
	if name, ok, err := t.store.CurrentTZ(ctx); err == nil && ok {
		if loc, err := schedule.LoadLocation(name); err == nil {
			zones.Device = loc
		}
	}
	return zones.For(item)
}

// validateSingleRetime checks a scope=single schedule fragment as a one-off
// instant - "push Thursday's to Friday" - reusing schedule.Validate's
// existing future/bounded checks (checkOneOff) rather than reimplementing
// them. Only the at field is read: scope=single retimes one occurrence, it
// does not carry a recurrence rule to validate.
func validateSingleRetime(at *string, ref time.Time, loc *time.Location) error {
	frag := schedule.Schedule{Kind: schedule.KindOneOff, At: at}
	return schedule.Validate(frag, ref, loc)
}

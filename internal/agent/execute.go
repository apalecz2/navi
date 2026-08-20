package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/aidenpaleczny/navi/internal/domain"
	"github.com/aidenpaleczny/navi/internal/schedule"
	"github.com/aidenpaleczny/navi/internal/store"
)

// Layer 3, transactional (docs/06-agent-spec.md#validation): exactly one
// store call per handler, below. Layers 1 and 2 have already run by the time
// any of these reach a store method, so nothing invalid ever opens a
// transaction; the wrapValidation helper below is defence for the one
// pre-existing non-*domain.ValidationError source (domain.NewItem.Validate,
// session 2) rather than a path this session expects to exercise.

// wrapValidation confines the "generic message" risk the session brief warns
// about to this one line: every handler's write returns either the original
// *domain.ValidationError unchanged, or - for the rare error that is not one
// - a single named fallback rule, so a caller checking with errors.As never
// silently loses the retry rung.
func wrapValidation(err error) error {
	if err == nil {
		return nil
	}
	var ve *domain.ValidationError
	if errors.As(err, &ve) {
		return err
	}
	return domain.Invalid("write_rejected", "", "%s", err)
}

func handleListItems(ctx context.Context, t *Tools, raw json.RawMessage) (Result, error) {
	args, err := decode[ListItemsArgs](raw)
	if err != nil {
		return Result{}, err
	}

	filter := store.ItemFilter(args.Filter)
	if filter == "" {
		filter = store.FilterActive
	}

	items, err := t.store.ListItems(ctx, filter, time.Now())
	if err != nil {
		return Result{}, err
	}
	return Result{Items: items}, nil
}

func handleCreateItem(ctx context.Context, t *Tools, raw json.RawMessage) (Result, error) {
	args, err := decode[CreateItemArgs](raw)
	if err != nil {
		return Result{}, err
	}

	loc, tzName, err := t.resolveCreateZone(ctx, args)
	if err != nil {
		return Result{}, err
	}

	r, err := t.mat.Begin(ctx)
	if err != nil {
		return Result{}, err
	}
	now := r.Now()

	resolved, inferred, err := resolveSchedule(args.Schedule, t.defaults, now, loc)
	if err != nil {
		return Result{}, err
	}
	scheduleJSON, err := resolved.Marshal()
	if err != nil {
		return Result{}, fmt.Errorf("agent: marshal schedule: %w", err)
	}

	n := domain.NewItem{
		Title:              args.Title,
		Schedule:           scheduleJSON,
		TZ:                 tzName,
		Notes:              args.Notes,
		Priority:           args.Priority,
		GracePeriodMinutes: args.GracePeriodMinutes,
		ReconcileAt:        args.ReconcileAt,
	}
	if args.Kind != "" {
		k := domain.Kind(args.Kind)
		n.Kind = k
	}
	if args.TZMode != "" {
		m := domain.TZMode(args.TZMode)
		n.TZMode = &m
	}
	if args.NotifyPolicy != "" {
		p := domain.NotifyPolicy(args.NotifyPolicy)
		n.NotifyPolicy = &p
	}

	item, applied, err := t.store.CreateItemAndMaterialize(ctx, n, now,
		func(item domain.Item, existing []domain.Occurrence) (store.Plan, error) {
			return t.mat.PlanFor(item, resolved, r, existing)
		})
	if err != nil {
		return Result{}, wrapValidation(err)
	}

	next, err := nextOccurrences(ctx, t.store, item.ID, now)
	if err != nil {
		return Result{}, err
	}
	if err := t.store.SetLastTouchedItem(ctx, item.ID); err != nil {
		return Result{}, err
	}
	return Result{Item: &item, NextOccurrences: next, Inferred: inferred, Applied: applied}, nil
}

func handleUpdateItem(ctx context.Context, t *Tools, raw json.RawMessage) (Result, error) {
	args, err := decode[UpdateItemArgs](raw)
	if err != nil {
		return Result{}, err
	}

	scope := args.Scope
	if scope == "" {
		scope = ScopeFutureAll
	}

	item, err := t.store.LiveItem(ctx, args.ItemID)
	if err != nil {
		return Result{}, err
	}
	loc, err := t.zoneFor(ctx, item)
	if err != nil {
		return Result{}, err
	}
	r, err := t.mat.Begin(ctx)
	if err != nil {
		return Result{}, err
	}
	now := r.Now()

	if scope == ScopeSingle {
		return t.updateSingle(ctx, item, args, loc, now)
	}

	// future_all and from_date share future_all's own machinery
	// (05-schedule-spec.md#edit-scope): from_date is that same machinery with
	// the floor moved forward via WithFloor, not a second code path, so
	// nothing before from_date is read, planned, or touched by either the
	// existing-row filter or expand()'s own window check.
	materializeAt := now
	if scope == ScopeFromDate {
		if args.FromDate == nil || *args.FromDate == "" {
			return Result{}, domain.Invalid("from_date_required", "from_date",
				"scope=from_date requires from_date")
		}
		from, err := time.ParseInLocation(domain.DateLayout, *args.FromDate, loc)
		if err != nil {
			return Result{}, domain.Invalid("from_date_format", "from_date",
				"from_date %q is not a date like 2006-01-02", *args.FromDate)
		}
		r = r.WithFloor(from)
		materializeAt = from
	}

	patch := patchFrom(args.Changes)

	var inferred []schedule.Inference
	var rematerialize func(domain.Item, []domain.Occurrence) (store.Plan, error)
	if needsRematerialize(args.Changes) {
		var effective schedule.Schedule
		if args.Changes.Schedule != nil {
			resolved, inf, err := resolveSchedule(*args.Changes.Schedule, t.defaults, now, loc)
			if err != nil {
				return Result{}, err
			}
			inferred = inf
			effective = resolved
			sj, err := resolved.Marshal()
			if err != nil {
				return Result{}, fmt.Errorf("agent: marshal schedule: %w", err)
			}
			patch.Schedule = &sj
		} else {
			// A non-schedule, non-exempt field changed (tz_mode, kind, ...):
			// still needs a re-plan, against the schedule as it already is.
			// Parse and not Prepare - this is the read path re-reading a
			// stored value, not the write path improving it.
			parsed, err := schedule.Parse(item.Schedule)
			if err != nil {
				return Result{}, err
			}
			effective = parsed
		}
		rematerialize = func(it domain.Item, existing []domain.Occurrence) (store.Plan, error) {
			return t.mat.PlanFor(it, effective, r, existing)
		}
	}

	updated, applied, err := t.store.UpdateItemAndMaterialize(ctx, item.ID, patch, materializeAt, rematerialize)
	if err != nil {
		return Result{}, wrapValidation(err)
	}

	next, err := nextOccurrences(ctx, t.store, updated.ID, now)
	if err != nil {
		return Result{}, err
	}
	if err := t.store.SetLastTouchedItem(ctx, updated.ID); err != nil {
		return Result{}, err
	}
	return Result{Item: &updated, NextOccurrences: next, Inferred: inferred, Applied: applied}, nil
}

// updateSingle is scope=single: retime exactly one occurrence
// (docs/05-schedule-spec.md#edit-scope). Any other, non-schedule field named
// in Changes is applied to the item without a re-plan - a single-occurrence
// edit never triggers one, by definition of what "single" means.
func (t *Tools) updateSingle(ctx context.Context, item domain.Item, args UpdateItemArgs, loc *time.Location, now time.Time) (Result, error) {
	if args.OccurrenceID == nil || *args.OccurrenceID == "" {
		return Result{}, domain.Invalid("occurrence_id_required", "occurrence_id",
			"scope=single requires occurrence_id")
	}
	occ, err := t.store.LiveOccurrence(ctx, *args.OccurrenceID, item.ID)
	if err != nil {
		return Result{}, err
	}

	if args.Changes.Schedule != nil {
		if err := validateSingleRetime(args.Changes.Schedule.At, now, loc); err != nil {
			return Result{}, err
		}
		ldt, err := (schedule.Schedule{Kind: schedule.KindOneOff, At: args.Changes.Schedule.At}).OneOffAt()
		if err != nil {
			return Result{}, err
		}
		at, _ := schedule.Instant(ldt, loc)
		if occ, err = t.store.UpdateOccurrenceOverride(ctx, occ.ID, item.ID, at, now); err != nil {
			return Result{}, wrapValidation(err)
		}
	}

	patch := patchFrom(args.Changes)
	patch.Schedule = nil // scope=single never touches the item's recurring definition
	updated := item
	if hasNonScheduleChange(args.Changes) {
		var err error
		// rematerialize is always nil here: a single-occurrence edit never
		// triggers a re-plan, by definition of what "single" means, whether
		// or not the changed field is on materializationExempt's list.
		updated, _, err = t.store.UpdateItemAndMaterialize(ctx, item.ID, patch, now, nil)
		if err != nil {
			return Result{}, wrapValidation(err)
		}
	}

	next, err := nextOccurrences(ctx, t.store, item.ID, now)
	if err != nil {
		return Result{}, err
	}
	if err := t.store.SetLastTouchedItem(ctx, item.ID); err != nil {
		return Result{}, err
	}
	_ = occ // read back for symmetry with the other scopes; not part of Result this session
	return Result{Item: &updated, NextOccurrences: next}, nil
}

func handleDeleteItem(ctx context.Context, t *Tools, raw json.RawMessage) (Result, error) {
	args, err := decode[DeleteItemArgs](raw)
	if err != nil {
		return Result{}, err
	}
	// Checked before any store round trip, per A7: a write is never
	// attempted to find out whether it would have been allowed.
	if !args.Confirmed {
		return Result{}, domain.Invalid("confirmation_required", "confirmed",
			"delete_item requires confirmed=true")
	}

	item, err := t.store.LiveItem(ctx, args.ItemID)
	if err != nil {
		return Result{}, err
	}

	r, err := t.mat.Begin(ctx)
	if err != nil {
		return Result{}, err
	}
	now := r.Now()

	archived, applied, err := t.store.ArchiveItem(ctx, item.ID, now,
		func(it domain.Item, existing []domain.Occurrence) (store.Plan, error) {
			return t.mat.PlanFor(it, schedule.Schedule{}, r, existing)
		})
	if err != nil {
		return Result{}, wrapValidation(err)
	}
	// Recorded even though the item is now archived: a later reference to it
	// fails store.LiveItem's own "item is archived" check rather than needing
	// a separate invalidation path (internal/store/kv.go's own doc comment on
	// LastTouchedItemID).
	if err := t.store.SetLastTouchedItem(ctx, archived.ID); err != nil {
		return Result{}, err
	}
	return Result{Item: &archived, Applied: applied}, nil
}

// handlePause suspends everything, or one item, until a date (I6).
//
// It is the tool 06-agent-spec tells the model to prefer over a run of skips,
// and the reason that preference matters is statistical rather than
// conversational: eighteen skips are eighteen decisions in the completion
// record, and a week away is one.
//
// Layer 2 is the scope/item_id pairing and the date format. Layer 3 is
// materializer.Pause or PauseAll, which own the write and the re-plan together
// — the same two entry points POST /api/items/{id}/pause and POST /api/pause
// reach, so a pause set by conversation and one set by the web app cannot come
// out meaning different things.
//
// The global scope makes two store calls rather than the usual one, which is
// the one place this file's "exactly one store call per handler" shape does not
// hold. It cannot: the kv write has to land before the re-plan reads it, and
// putting a full materialization inside one transaction would hold the single
// writer connection for the length of a nightly run. See PauseAll.
func handlePause(ctx context.Context, t *Tools, raw json.RawMessage) (Result, error) {
	args, err := decode[PauseArgs](raw)
	if err != nil {
		return Result{}, err
	}

	global := args.Scope == "global"
	switch {
	case global && args.ItemID != nil:
		return Result{}, domain.Invalid("scope_item_id", "item_id",
			"scope=global pauses everything and takes no item_id")
	case !global && (args.ItemID == nil || *args.ItemID == ""):
		return Result{}, domain.Invalid("scope_item_id", "item_id",
			"scope=item requires item_id")
	}

	// The date resolves in the device zone rather than the item's, on the same
	// argument the check-in's clock does: "until Monday" is a statement about
	// where the user will be, not about where a reminder is anchored.
	zones := schedule.Zones{Fallback: t.defaultTZ}
	if name, ok, err := t.store.CurrentTZ(ctx); err == nil && ok {
		if loc, err := schedule.LoadLocation(name); err == nil {
			zones.Device = loc
		}
	}

	var until *time.Time
	if args.Until != "" {
		at, err := time.ParseInLocation(domain.DateLayout, args.Until, zones.Local())
		if err != nil {
			return Result{}, domain.Invalid("pause_until_format", "until",
				"until %q is not a date like 2006-01-02", args.Until)
		}
		until = &at
	}

	if global {
		res, err := t.mat.PauseAll(ctx, until)
		if err != nil {
			return Result{}, wrapValidation(err)
		}
		// No SetLastTouchedItem: a global pause resolves to no item, so there
		// is nothing for a follow-up "it" to mean.
		return Result{PausedUntil: formatTimePtr(until), Applied: res.Applied}, nil
	}

	item, err := t.store.LiveItem(ctx, *args.ItemID)
	if err != nil {
		return Result{}, err
	}

	paused, applied, err := t.mat.Pause(ctx, item.ID, until)
	if err != nil {
		return Result{}, wrapValidation(err)
	}
	if err := t.store.SetLastTouchedItem(ctx, paused.ID); err != nil {
		return Result{}, err
	}
	return Result{Item: &paused, PausedUntil: formatTimePtr(paused.PausedUntil), Applied: applied}, nil
}

// formatTimePtr renders an optional instant through domain.FormatTime, keeping
// nil nil - the layout every timestamp this package hands outward uses.
func formatTimePtr(t *time.Time) *string {
	if t == nil {
		return nil
	}
	s := domain.FormatTime(*t)
	return &s
}

// handleBulkResolve records a batch of outcomes in one transaction.
//
// It is the tool behind "stretching and vitamins yes, skipped the walk" and
// behind "did my stretching already" — the second being a batch of one, which
// resolves a pending occurrence early and thereby cancels its notification (R3).
// That cancellation needs no code: both ListDueOccurrences and ClaimOccurrence
// filter status = 'pending', so a row this moves to a terminal status has
// already left the fire path by construction.
//
// Layer 2 is existence plus uniqueness. It deliberately does not use
// store.LiveOccurrence, which requires a pending, non-override row belonging to
// a named item — every one of those restrictions is wrong here, since a notified
// occurrence and a snooze child are both perfectly resolvable.
func handleBulkResolve(ctx context.Context, t *Tools, raw json.RawMessage) (Result, error) {
	args, err := decode[BulkResolveArgs](raw)
	if err != nil {
		return Result{}, err
	}

	rows := make([]store.BulkResolution, 0, len(args.Resolutions))
	seen := make(map[string]int, len(args.Resolutions))

	// itemOf is filled from the existence check below, which already loads
	// every occurrence. The confirmation names items rather than ids - "marked
	// stretching and vitamins done" - and this is where the mapping is free.
	itemOf := make(map[string]string, len(args.Resolutions))

	for i, r := range args.Resolutions {
		field := fmt.Sprintf("resolutions[%d].occurrence_id", i)

		// Two rows naming one occurrence would resolve it and then meet
		// themselves coming back — a no-op if the statuses agree and a rejected
		// transition if they do not. Neither is what the caller meant, and the
		// batch is atomic, so saying so is better than applying half a wish.
		if first, dup := seen[r.OccurrenceID]; dup {
			return Result{}, domain.Invalid("occurrence_unique", field,
				"occurrence %q appears twice, at resolutions[%d] and resolutions[%d]",
				r.OccurrenceID, first, i)
		}
		seen[r.OccurrenceID] = i

		occ, err := t.store.GetOccurrence(ctx, r.OccurrenceID)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				return Result{}, domain.Invalid("occurrence_exists", field,
					"occurrence %q does not exist", r.OccurrenceID)
			}
			return Result{}, err
		}
		itemOf[r.OccurrenceID] = occ.ItemID

		// Layer 1's enum tag has already rejected anything outside the three,
		// so this cannot fail for a decoded argument struct. It is here because
		// the closed set lives in one place and this is how a caller reaches it.
		status, ok := domain.ParseResolvableStatus(r.Status)
		if !ok {
			return Result{}, domain.Invalid("enum", fmt.Sprintf("resolutions[%d].status", i),
				"status %q is not one of completed, skipped, missed", r.Status)
		}

		rows = append(rows, store.BulkResolution{
			OccurrenceID: r.OccurrenceID,
			Status:       status,
			Note:         r.Note,
		})
	}

	resolved, err := t.store.BulkResolve(ctx, rows, domain.ResolvedByAgent, time.Now())
	if err != nil {
		return Result{}, wrapValidation(err)
	}

	// last_touched_item is not written here. It points at the item a
	// conversational follow-up would mean, and a resolution has no follow-up to
	// feed — a batch spanning three items has no single answer to point at
	// anyway (internal/store/kv.go's doc comment on LastTouchedItemID).
	titles := t.titlesFor(ctx, itemOf)

	out := make([]Resolved, len(resolved))
	for i, res := range resolved {
		out[i] = Resolved{
			OccurrenceID:   res.Occurrence.ID,
			ItemTitle:      titles[res.Occurrence.ID],
			Status:         string(res.Occurrence.Status),
			Applied:        res.Outcome == domain.OutcomeApplied,
			ChainCompleted: res.Chain.WasCompleted,
		}

		// Counted at this edge, per row, and only on a real transition — the
		// same rule internal/httpapi/resolve.go follows and for the same
		// reason: a no-op is the idempotency table's second row, not a status
		// change, and counting it would inflate every completion rate by
		// however often a button got double-tapped.
		if t.metrics != nil && res.Outcome == domain.OutcomeApplied {
			t.metrics.IncTransition(string(res.Previous), string(res.Occurrence.Status),
				string(domain.ResolvedByAgent))
		}
	}
	return Result{Resolutions: out}, nil
}

// titlesFor maps occurrence id to item title, reading each distinct item once.
//
// A title that cannot be read comes back empty rather than failing the call.
// The write has already committed by the time this runs, and refusing to
// confirm a resolution that happened would be the worse of the two outcomes;
// the renderer falls back to counting.
func (t *Tools) titlesFor(ctx context.Context, itemOf map[string]string) map[string]string {
	byItem := make(map[string]string, len(itemOf))
	out := make(map[string]string, len(itemOf))
	for occID, itemID := range itemOf {
		title, ok := byItem[itemID]
		if !ok {
			item, err := t.store.GetItem(ctx, itemID)
			if err != nil {
				byItem[itemID] = ""
				continue
			}
			title = item.Title
			byItem[itemID] = title
		}
		out[occID] = title
	}
	return out
}

// patchFrom translates the tool-facing ItemChanges into the store's
// ItemPatch. The two are separate types on purpose: ItemChanges is what a
// model fills in and Layer 1 validates against its jsonschema tags,
// ItemPatch is what the store applies - keeping them distinct is what lets
// the store package stay ignorant of the agent's argument shapes.
func patchFrom(c ItemChanges) store.ItemPatch {
	p := store.ItemPatch{
		Title:              c.Title,
		Notes:              c.Notes,
		TZ:                 c.TZ,
		ReconcileAt:        c.ReconcileAt,
		Priority:           c.Priority,
		GracePeriodMinutes: c.GracePeriodMinutes,
	}
	if c.Kind != nil {
		k := domain.Kind(*c.Kind)
		p.Kind = &k
	}
	if c.TZMode != nil {
		m := domain.TZMode(*c.TZMode)
		p.TZMode = &m
	}
	if c.NotifyPolicy != nil {
		n := domain.NotifyPolicy(*c.NotifyPolicy)
		p.NotifyPolicy = &n
	}
	if len(c.Attrs) > 0 {
		a := json.RawMessage(c.Attrs)
		p.Attrs = &a
	}
	return p
}

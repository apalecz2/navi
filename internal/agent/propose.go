package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/aidenpaleczny/navi/internal/domain"
)

// handleProposeChange records a suggestion and changes nothing (D-012, A12).
//
// It writes one kv record - the proposal - and no row of items or occurrences. The
// record exists so that "yes" in the next turn has something to mean: the Context
// ref block lists it, exactly as the proactive loop's own proposals are listed
// (internal/conversation/prompt.go), and a reply that agrees reaches update_item
// through the ordinary path, with the user's instruction as the trigger.
//
// Layer 2 is what makes the later "yes" safe to act on: the item must be live, the
// proposal must change something, and a proposed schedule goes through the same
// resolve-and-validate pass update_item runs. A proposal the ordinary path would
// reject is rejected here, in front of the model that can fix it, and not after the
// user has said yes to it.
func handleProposeChange(ctx context.Context, t *Tools, raw json.RawMessage) (Result, error) {
	args, err := decode[ProposeChangeArgs](raw)
	if err != nil {
		return Result{}, err
	}
	if strings.TrimSpace(args.Rationale) == "" {
		return Result{}, domain.Invalid("rationale_required", "rationale", "rationale must say why")
	}
	if len(changedFields(args.Proposal)) == 0 {
		return Result{}, domain.Invalid("proposal_empty", "proposal",
			"proposal must change at least one field of the item")
	}

	item, err := t.store.LiveItem(ctx, args.ItemID)
	if err != nil {
		return Result{}, err
	}
	now := time.Now()

	changes := args.Proposal
	if changes.Schedule != nil {
		loc, err := t.zoneFor(ctx, item)
		if err != nil {
			return Result{}, err
		}
		resolved, _, err := resolveSchedule(*changes.Schedule, t.defaults, now, loc)
		if err != nil {
			return Result{}, err
		}
		changes.Schedule = &resolved
	}
	encoded, err := json.Marshal(changes)
	if err != nil {
		return Result{}, fmt.Errorf("agent: marshal proposal: %w", err)
	}

	p, err := t.store.AddProposal(ctx, domain.Proposal{
		Trigger:   domain.TriggerAgent,
		ItemID:    item.ID,
		ItemTitle: item.Title,
		Tool:      domain.ProposalUpdateItem,
		Changes:   encoded,
		Summary:   describeChanges(changes),
		Rationale: strings.TrimSpace(args.Rationale),
	}, now)
	if err != nil {
		return Result{}, err
	}
	return Result{Proposal: &p}, nil
}

// describeChanges renders a proposal's fields as the phrase that completes "I'd
// suggest we ...". It is code and not the model's wording, so what the confirmation
// says is what the stored changes say.
func describeChanges(c ItemChanges) string {
	var parts []string
	if c.Schedule != nil {
		parts = append(parts, "change its schedule to "+c.Schedule.String())
	}
	if c.Title != nil {
		parts = append(parts, fmt.Sprintf("rename it to %q", *c.Title))
	}
	if c.NotifyPolicy != nil {
		parts = append(parts, "make it "+strings.ReplaceAll(*c.NotifyPolicy, "_", " "))
	}
	if c.Priority != nil {
		parts = append(parts, fmt.Sprintf("set its priority to %d", *c.Priority))
	}
	for _, f := range changedFields(c) {
		switch f {
		case "schedule", "title", "notify_policy", "priority":
		default:
			parts = append(parts, "change its "+strings.ReplaceAll(f, "_", " "))
		}
	}
	return strings.Join(parts, " and ")
}

// acceptProposals closes the open proposals on an item after a write to it has
// committed, and counts them. It cannot fail the call: the write is done, and
// reporting an error would send the ladder round to apply it a second time. A
// proposal this misses stays open until the ladder passes it at the end of the
// turn, which is only a missed count.
func (t *Tools) acceptProposals(ctx context.Context, itemID string, now time.Time) {
	accepted, err := t.store.AcceptProposals(ctx, itemID, now)
	if err != nil || t.metrics == nil {
		return
	}
	for _, p := range accepted {
		t.metrics.IncProposalAccepted(p.Trigger)
	}
}

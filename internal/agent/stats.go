package agent

import (
	"context"
	"encoding/json"

	"github.com/aidenpaleczny/navi/internal/stats"
)

// GetStatsArgs is get_stats' arguments, from docs/06-agent-spec.md#tool-catalog.
//
// Both fields are optional: range defaults to month, and no item_id means every
// item. The range enum is the same set stats.NewQuery accepts, and that function
// is what the HTTP handlers validate with too.
type GetStatsArgs struct {
	Range  string  `json:"range,omitempty" jsonschema:"enum=week,enum=month,enum=quarter,enum=all,default=month"`
	ItemID *string `json:"item_id,omitempty"`
}

// handleGetStats is a read. It computes nothing: it validates through
// stats.NewQuery, asks the stats.Service the HTTP routes ask, and hands the typed
// summary back in the Result for reply.go to phrase. A number the chat states is
// therefore the number /api/stats/summary serves for the same range (V6, O4);
// the two only differ if one of them stopped calling stats.Service.
func handleGetStats(ctx context.Context, t *Tools, raw json.RawMessage) (Result, error) {
	args, err := decode[GetStatsArgs](raw)
	if err != nil {
		return Result{}, err
	}

	itemID := ""
	if args.ItemID != nil {
		itemID = *args.ItemID
	}
	q, ve := stats.NewQuery(args.Range, itemID, "")
	if ve != nil {
		return Result{}, ve
	}

	sum, err := t.stats.Summary(ctx, q)
	if err != nil {
		return Result{}, err
	}
	return Result{Stats: &sum}, nil
}

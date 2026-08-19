package httpapi

import (
	"time"

	"github.com/aidenpaleczny/navi/internal/domain"
	"github.com/aidenpaleczny/navi/internal/store"
)

// The response shapes two endpoints share. They live here rather than beside
// either one because a snooze reports an occurrence and a chain, a resolution
// reports a chain, and two renderings of the same row would be two places for
// the field names to drift.

// occurrenceResponse is one occurrence on the wire, under its column names.
//
// The field set is the one docs/07-api-spec.md's GET /api/today element uses,
// minus the item columns a caller of these endpoints already has, plus the three
// chain columns — a snooze response is unreadable without is_override,
// parent_occurrence_id and snooze_depth, since those are the whole of what it
// wrote.
type occurrenceResponse struct {
	ID       string `json:"id"`
	ItemID   string `json:"item_id"`
	StartsAt string `json:"starts_at"`
	Status   string `json:"status"`

	IsOverride         bool    `json:"is_override"`
	ParentOccurrenceID *string `json:"parent_occurrence_id"`
	SnoozeDepth        int     `json:"snooze_depth"`

	NotifiedAt       *string `json:"notified_at"`
	ResolvedAt       *string `json:"resolved_at"`
	ResolutionNote   *string `json:"resolution_note"`
	ResolutionSource *string `json:"resolution_source"`
}

// chainResponse is a snooze chain rolled up: one chain, counted once (R7,
// D-011).
//
// was_completed is the field that carries the incentive argument — any
// completed link completes the chain, so an honest snooze costs a streak
// nothing. scheduled_at is the root's original time, which snoozing never
// rewrites, so a client can show "was due 09:00, done 11:20" without walking
// anything.
type chainResponse struct {
	RootID      string `json:"root_id"`
	ScheduledAt string `json:"scheduled_at"`

	SnoozeCount  int     `json:"snooze_count"`
	WasCompleted bool    `json:"was_completed"`
	CompletedAt  *string `json:"completed_at"`
	NotifiedAt   *string `json:"notified_at"`
}

func toOccurrenceResponse(occ domain.Occurrence) occurrenceResponse {
	out := occurrenceResponse{
		ID:                 occ.ID,
		ItemID:             occ.ItemID,
		StartsAt:           domain.FormatTime(occ.StartsAt),
		Status:             string(occ.Status),
		IsOverride:         occ.IsOverride,
		ParentOccurrenceID: occ.ParentOccurrenceID,
		SnoozeDepth:        occ.SnoozeDepth,
		NotifiedAt:         formatTimePtr(occ.NotifiedAt),
		ResolvedAt:         formatTimePtr(occ.ResolvedAt),
		ResolutionNote:     occ.ResolutionNote,
	}
	if occ.ResolutionSource != nil {
		src := string(*occ.ResolutionSource)
		out.ResolutionSource = &src
	}
	return out
}

func toChainResponse(c store.Chain) chainResponse {
	return chainResponse{
		RootID:       c.RootID,
		ScheduledAt:  domain.FormatTime(c.ScheduledAt),
		SnoozeCount:  c.SnoozeCount,
		WasCompleted: c.WasCompleted,
		CompletedAt:  formatTimePtr(c.CompletedAt),
		NotifiedAt:   formatTimePtr(c.NotifiedAt),
	}
}

// formatTimePtr renders an optional instant, keeping null null. Every timestamp
// on the wire goes through domain.FormatTime, so the layout is the one the whole
// system compares as text.
func formatTimePtr(t *time.Time) *string {
	if t == nil {
		return nil
	}
	s := domain.FormatTime(*t)
	return &s
}

package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/aidenpaleczny/navi/internal/domain"
	"github.com/aidenpaleczny/navi/internal/schedule"
	"github.com/aidenpaleczny/navi/internal/store"
)

// snoozeRequest is the body from docs/07-api-spec.md#post-apioccurrencesidsnooze.
//
// source is required, and the spec's own example is one field short of this.
// Two things need it and neither can guess: the parent row's
// resolution_source column, and navi_occurrence_transitions_total's source
// label on the notified -> snoozed edge. Defaulting it would mislabel every
// call that forgot the field, which is worse than a body the doc's example does
// not show — and the doc has been updated to show it.
//
// There is no note field, on the spec's authority and on the pseudocode's: a
// snooze is not a resolution anyone attaches a reason to. The one note this
// path ever writes is the cap message, which the store composes.
type snoozeRequest struct {
	Delta  string `json:"delta"`
	Source string `json:"source"`
}

// snoozeResponse is the child, the parent, and the chain.
//
// The spec says "returns the new child occurrence", and the child alone is not
// enough for a caller to render what happened: R6's whole point is that the
// original kept its timestamp, which is only visible if the original is in the
// response. The chain is D-011 made reportable — one chain, counted once — and
// it is what the Telegram toast will read rather than re-deriving.
//
// The same shape comes back for an applied snooze and for a repeated one,
// because in both cases it is the current state. That is the reason those two
// rows of the idempotency table share a status code, and the reason
// ChildOccurrence exists.
type snoozeResponse struct {
	Occurrence occurrenceResponse `json:"occurrence"`
	Parent     occurrenceResponse `json:"parent"`
	Chain      chainResponse      `json:"chain"`
}

// handleSnoozeOccurrence is POST /api/occurrences/{id}/snooze.
//
// Like the resolution handler beside it, it decides nothing about resolution.
// Its judgement is request shape — that delta and source are members of the
// sets the spec and the schema define — and the mapping of the store's answer
// onto a status code:
//
//	OutcomeApplied   -> 200, child + parent + chain, one transition counted
//	OutcomeNoop      -> 200, the same shape, nothing written, nothing counted
//	CapReached       -> 409, the chain resolved as missed, that edge counted
//	*TransitionError -> 409 with the current state
//
// pending -> snoozed is not an edge in the table, so snoozing a reminder that
// has not fired arrives at the ordinary 409 with no special case here. That is
// the state machine's answer, and reproducing the rule at this layer is the
// divergence D-014 bought one endpoint to avoid.
func (s *Server) handleSnoozeOccurrence(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	var req snoozeRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxResolveBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeValidationError(w, s.log, domain.Invalid("request_body_valid", "",
			"request body is not a valid snooze: %s", err))
		return
	}

	delta := schedule.Delta(req.Delta)
	if !delta.Valid() {
		writeValidationError(w, s.log, domain.Invalid("snooze_delta_valid", "delta",
			"delta %q is not one of 10m, 1h, tonight, tomorrow", req.Delta))
		return
	}
	source, ok := resolutionSources[req.Source]
	if !ok {
		writeValidationError(w, s.log, domain.Invalid("resolve_source_valid", "source",
			"source %q is not one of notification, web, agent, sweeper", req.Source))
		return
	}

	// The device zone is read once, here, before the write transaction opens —
	// the same discipline schedule.Zones' own doc comment describes and the
	// materializer follows. What goes down into the transaction is then a pure
	// closure, which is what keeps internal/store free of a schedule import and
	// keeps the delta resolving against the row as it stands inside the write.
	zones, err := s.zones(r.Context())
	if err != nil {
		s.log.Error("snooze occurrence: resolve zones", "occurrence", id, "err", err)
		writeJSON(w, s.log, http.StatusInternalServerError, errorBody{
			Error:   "internal",
			Message: "the snooze could not be recorded",
		})
		return
	}

	now := time.Now()
	at := schedule.SnoozeAt(zones, delta, now,
		func(item domain.Item, loc *time.Location, startsAt time.Time, fold schedule.Fold) {
			if fold == schedule.FoldNone {
				return
			}
			// Rare, deliberate, and the only explanation for a snooze that
			// landed an hour from where it was asked for. Same line the
			// materializer logs for the same reason.
			s.log.Debug("snooze: dst boundary", "occurrence", id, "item", item.ID,
				"delta", string(delta), "zone", loc.String(), "fold", fold.String(),
				"instant", domain.FormatTime(startsAt))
		})

	res, err := s.store.SnoozeOccurrence(r.Context(), id, source, now, at)
	if err != nil {
		var te *domain.TransitionError
		var ve *domain.ValidationError
		switch {
		case errors.As(err, &te):
			writeTransitionError(w, s.log, te)
		case errors.Is(err, store.ErrNotFound):
			writeNotFound(w, s.log, "occurrence "+id+" does not exist")
		case errors.As(err, &ve):
			// The delta could not be resolved against this item — an
			// unreadable stored schedule or a timezone that no longer loads.
			// It names the field and the values, so it is worth showing.
			writeValidationError(w, s.log, ve)
		default:
			s.log.Error("snooze occurrence", "occurrence", id, "err", err)
			writeJSON(w, s.log, http.StatusInternalServerError, errorBody{
				Error:   "internal",
				Message: "the snooze could not be recorded",
			})
		}
		return
	}

	// The cap: the chain resolved as missed rather than acquiring a fourth
	// link (R8). The write happened, so the edge is counted and the 409 reports
	// the state the occurrence is actually in — which is what the adapter's
	// toast reads back.
	if res.CapReached {
		s.metrics.IncTransition(string(res.Previous), string(res.Parent.Status), string(source))
		writeSnoozeCapReached(w, s.log, res)
		return
	}

	if res.Outcome == domain.OutcomeApplied {
		s.metrics.IncTransition(string(res.Previous), string(res.Parent.Status), string(source))
	}

	writeJSON(w, s.log, http.StatusOK, snoozeResponse{
		Occurrence: toOccurrenceResponse(res.Child),
		Parent:     toOccurrenceResponse(res.Parent),
		Chain:      toChainResponse(res.Chain),
	})
}

// zones resolves which location an item's wall clocks mean, reading the device
// zone once. The reading is schedule.LoadZones', shared with the Telegram
// callback handler so that two snooze surfaces cannot resolve a delta against
// two different pictures of where the user is.
func (s *Server) zones(ctx context.Context) (schedule.Zones, error) {
	return schedule.LoadZones(ctx, s.store, s.defaultTZ)
}

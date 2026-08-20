package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/aidenpaleczny/navi/internal/domain"
	"github.com/aidenpaleczny/navi/internal/store"
)

// maxPauseBody bounds the request the same way maxResolveBody bounds a
// resolution. The body is one field.
const maxPauseBody = 4 << 10

// pauseRequest is the body from docs/07-api-spec.md#post-apiitemsidpause,
// shared by both scopes because the spec gives them one shape.
//
// until is an ISO date rather than an instant, and a null lifts the pause. The
// spec shows only the setting half; a suspension with no way out would make
// "I'm away until Monday" a worse deal than the eighteen skips I6 exists to
// replace, and the same statement writes both, so it is one field and not a
// second endpoint.
//
// A pointer to a pointer would be needed to tell "until: null" from an omitted
// until, and nothing needs that distinction: both mean "no pause", and refusing
// an empty body would only make unpausing harder to spell.
type pauseRequest struct {
	Until *string `json:"until"`
}

// pauseResponse reports the window and what re-planning it cost.
//
// occurrences_deleted is the number the spec does not ask for and the one
// worth having: it is the eighteen skips that did not happen, made visible, and
// it is how a caller can tell a pause that landed on a busy fortnight from one
// that changed nothing. It comes from the plan the materializer already
// produced rather than from a second count.
type pauseResponse struct {
	PausedUntil *string `json:"paused_until"`

	Item  *itemResponse `json:"item,omitempty"`
	Items *int          `json:"items,omitempty"`

	OccurrencesDeleted int `json:"occurrences_deleted"`
}

// itemResponse is the item the item-scoped route echoes back. Narrow on
// purpose: this endpoint changed one column, and the fields here are the ones
// that say what state the item is now in.
type itemResponse struct {
	ID          string  `json:"id"`
	Title       string  `json:"title"`
	Active      bool    `json:"active"`
	PausedUntil *string `json:"paused_until"`
}

// handlePauseItem is POST /api/items/{id}/pause.
//
// Its judgement is request shape and nothing else, the same division resolve.go
// and snooze.go keep: it turns a date into an instant in the right zone and
// hands it to the materializer, which owns both the write and the re-plan. It
// asks domain.Transition nothing, because a pause is not a resolution — the
// pending occurrences inside the window are deleted, not completed, skipped, or
// missed, and that is the entire point of I6.
func (s *Server) handlePauseItem(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	until, ok := s.decodePause(w, r)
	if !ok {
		return
	}

	item, applied, err := s.mat.Pause(r.Context(), id, until)
	if err != nil {
		s.writePauseError(w, err, "item "+id+" does not exist", "the pause could not be recorded")
		return
	}

	writeJSON(w, s.log, http.StatusOK, pauseResponse{
		PausedUntil: formatTimePtr(item.PausedUntil),
		Item: &itemResponse{
			ID:          item.ID,
			Title:       item.Title,
			Active:      item.Active,
			PausedUntil: formatTimePtr(item.PausedUntil),
		},
		OccurrencesDeleted: applied.Deleted,
	})
}

// handlePauseGlobal is POST /api/pause: vacation mode, the same body one scope
// wider.
//
// It re-plans every active item, which is what actually clears the calendar. A
// paused item is not skipped by the materializer, it is materialized to an
// empty set (05-schedule-spec), so the same pass that stops generating new rows
// deletes the pending ones already sitting inside the window.
func (s *Server) handlePauseGlobal(w http.ResponseWriter, r *http.Request) {
	until, ok := s.decodePause(w, r)
	if !ok {
		return
	}

	res, err := s.mat.PauseAll(r.Context(), until)
	if err != nil {
		s.writePauseError(w, err, "", "the pause could not be recorded")
		return
	}

	writeJSON(w, s.log, http.StatusOK, pauseResponse{
		PausedUntil:        formatTimePtr(until),
		Items:              &res.Items,
		OccurrencesDeleted: res.Applied.Deleted,
	})
}

// decodePause reads the body and resolves until into an instant, writing the
// error response itself and reporting whether the caller should continue.
//
// The date resolves to local midnight in the device zone, read once before any
// write on the same discipline the snooze handler follows. "until Monday"
// therefore means through Sunday night and back to normal at Monday 00:00 local
// — domain.Item.IsPaused compares with a strict After, and the SQL filters use
// `paused_until <= now`, so an occurrence at exactly that midnight is not
// paused. The two boundaries agree by construction rather than by both being
// remembered.
func (s *Server) decodePause(w http.ResponseWriter, r *http.Request) (*time.Time, bool) {
	var req pauseRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxPauseBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeValidationError(w, s.log, domain.Invalid("request_body_valid", "",
			"request body is not a valid pause: %s", err))
		return nil, false
	}

	if req.Until == nil || *req.Until == "" {
		return nil, true
	}

	zones, err := s.zones(r.Context())
	if err != nil {
		s.log.Error("pause: resolve zones", "err", err)
		writeJSON(w, s.log, http.StatusInternalServerError, errorBody{
			Error:   "internal",
			Message: "the pause could not be recorded",
		})
		return nil, false
	}

	until, err := time.ParseInLocation(domain.DateLayout, *req.Until, zones.Local())
	if err != nil {
		writeValidationError(w, s.log, domain.Invalid("pause_until_format", "until",
			"until %q is not a date like 2006-01-02", *req.Until))
		return nil, false
	}
	return &until, true
}

// writePauseError maps the store's answer onto a status code. There is no
// transition case: nothing on this path consults the state machine, so the
// three outcomes are a rejected request, a missing item, and a failure.
// notFound is empty for the global scope, which has no id to miss.
func (s *Server) writePauseError(w http.ResponseWriter, err error, notFound, internal string) {
	var ve *domain.ValidationError
	switch {
	case errors.As(err, &ve):
		writeValidationError(w, s.log, ve)
	case notFound != "" && errors.Is(err, store.ErrNotFound):
		writeNotFound(w, s.log, notFound)
	default:
		s.log.Error("pause", "err", err)
		writeJSON(w, s.log, http.StatusInternalServerError, errorBody{
			Error:   "internal",
			Message: internal,
		})
	}
}

package httpapi

import (
	"encoding/json"
	"log/slog"
	"net/http"

	"github.com/aidenpaleczny/navi/internal/domain"
	"github.com/aidenpaleczny/navi/internal/store"
)

// errorBody is the error envelope docs/07-api-spec.md#errors specifies, plus
// the one field a rejected transition needs that a rejected value does not.
//
// The three-field shape comes from the spec's own example; current_state is
// added because docs/07-api-spec.md#idempotency requires a 409 to report the
// state the occurrence is actually in, and the Telegram adapter's toast is
// specified to read it back. field and current_state are never both set: the
// first belongs to a *domain.ValidationError, the second to a
// *domain.TransitionError, and no error is both.
type errorBody struct {
	Error   string `json:"error"`
	Message string `json:"message"`

	Field        string `json:"field,omitempty"`
	CurrentState string `json:"current_state,omitempty"`
}

// writeJSON renders one response. An encode failure is logged rather than
// returned: the status line is already on the wire by then, so there is
// nothing left to tell the client and the only useful record is a log line.
func writeJSON(w http.ResponseWriter, log *slog.Logger, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)

	if err := json.NewEncoder(w).Encode(body); err != nil {
		log.Error("encode response", "status", status, "err", err)
	}
}

// writeValidationError renders a rejected value as 400. Message is copied
// verbatim — it names the rule and the offending values, which is what makes
// it worth showing.
func writeValidationError(w http.ResponseWriter, log *slog.Logger, ve *domain.ValidationError) {
	writeJSON(w, log, http.StatusBadRequest, errorBody{
		Error:   "validation_failed",
		Message: ve.Message,
		Field:   ve.Field,
	})
}

// writeTransitionError renders a rejected transition as 409, reporting the
// state the occurrence is in as From.
//
// Both 409 rows of the idempotency table arrive here — a different terminal
// state and an outright illegal edge — because the state machine has already
// told them apart in Message and nothing at this layer needs to know which it
// was holding.
func writeTransitionError(w http.ResponseWriter, log *slog.Logger, te *domain.TransitionError) {
	writeJSON(w, log, http.StatusConflict, errorBody{
		Error:        "transition_illegal",
		Message:      te.Message,
		CurrentState: string(te.From),
	})
}

// writeSnoozeCapReached renders an exhausted snooze chain as 409, reporting the
// state the occurrence is now in — missed, because the cap resolved the chain
// rather than refusing and leaving it hanging (R8).
//
// It is a separate writer from writeTransitionError because it is a separate
// situation: nothing illegal was asked for and nothing was rejected. The
// transition was legal, a precondition on it was not met, and the write that
// followed is the one current_state reports. The message is the one
// domain.CheckSnoozeCap composed, so it names the depth and the cap the way
// every other message field in this API names its values.
func writeSnoozeCapReached(w http.ResponseWriter, log *slog.Logger, res store.Snooze) {
	message := "the snooze cap has been reached and the chain is now missed"
	if res.Parent.ResolutionNote != nil {
		message = *res.Parent.ResolutionNote
	}

	writeJSON(w, log, http.StatusConflict, errorBody{
		Error:        "snooze_cap_reached",
		Message:      message,
		CurrentState: string(res.Parent.Status),
	})
}

func writeNotFound(w http.ResponseWriter, log *slog.Logger, message string) {
	writeJSON(w, log, http.StatusNotFound, errorBody{
		Error:   "not_found",
		Message: message,
	})
}

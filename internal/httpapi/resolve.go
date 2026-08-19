package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/aidenpaleczny/navi/internal/domain"
	"github.com/aidenpaleczny/navi/internal/store"
)

// maxResolveBody caps the request body. A resolution is three short fields;
// anything larger is a mistake or a probe, and reading it into memory first to
// find out is the part worth refusing.
const maxResolveBody = 16 << 10

// resolveRequest is the body from docs/07-api-spec.md#post-apioccurrencesidresolve.
//
// note is a pointer because absent and empty are different: omitted leaves
// resolution_note NULL, while "" is a caller that sent a blank note.
type resolveRequest struct {
	Status string  `json:"status"`
	Note   *string `json:"note"`
	Source string  `json:"source"`
}

// resolveResponse echoes the four columns a resolution writes, plus the id.
//
// The spec fixes the request body and the idempotency table but not a response
// schema, so this is designed here: the fields are exactly what was written,
// under their column names, which is what lets a caller render the outcome
// without a second GET. The same body is returned for an applied transition
// and for a no-op, because in both cases it is the current state — which is
// the whole reason those two rows of the table share a status code.
type resolveResponse struct {
	ID               string  `json:"id"`
	Status           string  `json:"status"`
	ResolvedAt       *string `json:"resolved_at"`
	ResolutionNote   *string `json:"resolution_note"`
	ResolutionSource *string `json:"resolution_source"`
}

// resolvableStatuses is the status enum from the spec's request body.
//
// missed is a member, and deliberately unguarded beyond this. Per D-008 and K6
// it means "asked and got nothing", never "the clock passed midnight", and
// nothing in this repository sends it yet: snooze-cap exhaustion is its first
// caller and the reconciler owns it from P3. The guard therefore belongs in
// those callers, not here — a rule at this layer restricting which source may
// ask for which status would be a second copy of the transition table living
// beside the real one, which is exactly the divergence D-014 bought a single
// endpoint to avoid.
var resolvableStatuses = map[string]domain.Status{
	string(domain.StatusCompleted): domain.StatusCompleted,
	string(domain.StatusSkipped):   domain.StatusSkipped,
	string(domain.StatusMissed):    domain.StatusMissed,
}

// resolutionSources mirrors the CHECK constraint on occurrences.resolution_source.
//
// All four are accepted because the column accepts all four. Only web and
// agent have a caller today; notification arrives with the callback handler,
// and sweeper with reconciliation.
var resolutionSources = map[string]domain.ResolutionSource{
	string(domain.ResolvedByNotification): domain.ResolvedByNotification,
	string(domain.ResolvedByWeb):          domain.ResolvedByWeb,
	string(domain.ResolvedByAgent):        domain.ResolvedByAgent,
	string(domain.ResolvedBySweeper):      domain.ResolvedBySweeper,
}

// handleResolveOccurrence is POST /api/occurrences/{id}/resolve — the one
// resolution endpoint every surface converges on (D-014, invariant 4).
//
// It decides nothing about resolution. Its only judgement is request shape:
// that status and source are members of the sets the spec and the schema
// define, which is a question about the body and not about the occurrence.
// Everything else is store.ResolveOccurrence's answer, and the mapping onto
// status codes is the one domain.Outcome's own doc comment specifies:
//
//	OutcomeApplied -> 200 with the new state, and one transition counted
//	OutcomeNoop    -> 200 with the current state, nothing written
//	*TransitionError -> 409 with the current state
//
// The two 409 rows of docs/07-api-spec.md#idempotency — a different terminal
// state, and an illegal edge — are one branch here on purpose. The state
// machine has already distinguished them in its message, and re-deriving the
// difference at the edge is how three surfaces end up with three answers.
//
// Authentication happens before this runs: /api/* sits behind Cloudflare
// Access at the tunnel edge (docs/07-api-spec.md#authentication), which is what
// D-014 means by the endpoint carrying three auth modes at no cost to the
// handler.
func (s *Server) handleResolveOccurrence(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	var req resolveRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxResolveBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeValidationError(w, s.log, domain.Invalid("request_body_valid", "",
			"request body is not a valid resolution: %s", err))
		return
	}

	status, ok := resolvableStatuses[req.Status]
	if !ok {
		writeValidationError(w, s.log, domain.Invalid("resolve_status_valid", "status",
			"status %q is not one of completed, skipped, missed", req.Status))
		return
	}
	source, ok := resolutionSources[req.Source]
	if !ok {
		writeValidationError(w, s.log, domain.Invalid("resolve_source_valid", "source",
			"source %q is not one of notification, web, agent, sweeper", req.Source))
		return
	}

	res, err := s.store.ResolveOccurrence(r.Context(), id, status, req.Note, source, time.Now())
	if err != nil {
		var te *domain.TransitionError
		switch {
		case errors.As(err, &te):
			writeTransitionError(w, s.log, te)
		case errors.Is(err, store.ErrNotFound):
			writeNotFound(w, s.log, "occurrence "+id+" does not exist")
		default:
			s.log.Error("resolve occurrence", "occurrence", id, "err", err)
			writeJSON(w, s.log, http.StatusInternalServerError, errorBody{
				Error:   "internal",
				Message: "the resolution could not be recorded",
			})
		}
		return
	}

	// Only a real edge is counted. A no-op changed nothing, and counting it
	// would inflate the same series the scheduler's claim-release is already
	// kept out of for the same reason.
	if res.Outcome == domain.OutcomeApplied {
		s.metrics.IncTransition(string(res.Previous), string(res.Occurrence.Status), string(source))
	}

	writeJSON(w, s.log, http.StatusOK, toResolveResponse(res.Occurrence))
}

func toResolveResponse(occ domain.Occurrence) resolveResponse {
	out := resolveResponse{
		ID:             occ.ID,
		Status:         string(occ.Status),
		ResolutionNote: occ.ResolutionNote,
	}
	if occ.ResolvedAt != nil {
		at := domain.FormatTime(*occ.ResolvedAt)
		out.ResolvedAt = &at
	}
	if occ.ResolutionSource != nil {
		src := string(*occ.ResolutionSource)
		out.ResolutionSource = &src
	}
	return out
}

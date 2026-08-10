package agent

import (
	"context"
	"encoding/json"
)

// EscalationRequested is what request_escalation returns instead of a
// Result. It is not a domain.ValidationError - the model isn't wrong about
// anything, it is reporting its own uncertainty - so the escalation ladder
// (internal/conversation) distinguishes it with errors.As and skips the
// same-tier retry entirely, moving straight to the next tier's first
// attempt: "a self-report is free compared to a failed attempt" (L4).
//
// A pointer type, on the same convention domain.ValidationError uses, so
// errors.As(&EscalationRequested{}) works without an extra indirection.
type EscalationRequested struct {
	Reason string
}

func (e *EscalationRequested) Error() string {
	return "escalation requested: " + e.Reason
}

// handleRequestEscalation decodes Layer 1 only - required Reason, nothing
// else - and returns EscalationRequested rather than a Result. It makes no
// store or materializer call, which is why it lives in its own file rather
// than execute.go: execute.go's own doc comment says "exactly one store
// call per handler," and this handler makes zero.
func handleRequestEscalation(ctx context.Context, t *Tools, raw json.RawMessage) (Result, error) {
	args, err := decode[RequestEscalationArgs](raw)
	if err != nil {
		// Malformed escalation request degrades to an ordinary same-tier
		// retry via the usual *domain.ValidationError path - there is no
		// reason to treat "the model botched its own escalation call"
		// differently from any other Layer 1 failure.
		return Result{}, err
	}
	return Result{}, &EscalationRequested{Reason: args.Reason}
}

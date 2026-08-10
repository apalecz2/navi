package model

import "fmt"

// ErrorKind distinguishes the conditions a caller needs to react to
// differently. Collapsing these into one error type would remove the
// escalation ladder's ability to do anything sensible with a failure — a
// rate limit and a malformed body are not the same problem, and they don't
// call for the same next move.
type ErrorKind int

const (
	// KindUnavailable is a transport-level failure: a connection error, or a
	// 5xx the provider returned. Worth retrying at the same tier — it may be
	// transient — and worth escalating, since a different tier may hit a
	// different, reachable provider.
	KindUnavailable ErrorKind = iota + 1

	// KindRateLimited is an HTTP 429. A same-tier retry without backoff (which
	// this session does not build) is more likely to repeat the failure than
	// fix it; escalating to a different tier sidesteps the limit instead.
	KindRateLimited

	// KindTimeout is the tier's own ctx deadline expiring. Retrying the same
	// tier spends the same budget again for the same likely outcome;
	// escalating to a different tier is the only lever this session gives
	// the ladder.
	KindTimeout

	// KindMalformed is a response that arrived but didn't decode into the
	// shape this client expects — a body that isn't the JSON an
	// OpenAI-compatible endpoint is supposed to return.
	KindMalformed

	// KindEmpty is a response that decoded fine but carries nothing usable:
	// no content and no tool calls, or an explicit refusal / content-filter
	// finish reason. This is not the same condition as "the model answered
	// with prose when a tool call was expected" — that is a well-formed,
	// successful Result, and judging it inadequate is the ladder's job (L4),
	// not this client's. KindEmpty is reserved for a completion with nothing
	// in it at all.
	KindEmpty

	// KindConfig is a caller mistake, not a provider condition: an unknown
	// Task, or a Tier index beyond what Routing configures for it (asking
	// for tier 2 of a task that only has one). Never retryable, never
	// escalatable — there is nowhere else for the ladder to go.
	KindConfig
)

func (k ErrorKind) String() string {
	switch k {
	case KindUnavailable:
		return "unavailable"
	case KindRateLimited:
		return "rate_limited"
	case KindTimeout:
		return "timeout"
	case KindMalformed:
		return "malformed"
	case KindEmpty:
		return "empty"
	case KindConfig:
		return "config"
	default:
		return "unknown"
	}
}

// Retryable reports whether the same tier is worth attempting again. Data for
// the ladder to consult, not a decision this package acts on — Complete never
// retries.
func (k ErrorKind) Retryable() bool {
	return k == KindUnavailable
}

// Escalatable reports whether moving to the next tier is worth trying. Every
// kind but KindConfig is — a bad task name or an out-of-range tier index is a
// caller bug, and no tier fixes it.
func (k ErrorKind) Escalatable() bool {
	return k != KindConfig
}

// Error is what Complete returns on any failure. Task, Tier and Model
// identify which configured endpoint was attempted, so a caller building the
// next request (or just logging) never has to thread that context through
// separately.
type Error struct {
	Kind  ErrorKind
	Task  Task
	Tier  int
	Model string
	Err   error
}

func (e *Error) Error() string {
	if e.Err != nil {
		return fmt.Sprintf("model: %s task=%s tier=%d model=%q: %v", e.Kind, e.Task, e.Tier, e.Model, e.Err)
	}
	return fmt.Sprintf("model: %s task=%s tier=%d model=%q", e.Kind, e.Task, e.Tier, e.Model)
}

func (e *Error) Unwrap() error { return e.Err }

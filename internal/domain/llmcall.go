package domain

import (
	"fmt"
	"time"
)

// LLMCall is one row of the model-call log: every attempt the model client
// makes, successful or not. Retention is 90 days, enforced by the sweeper
// rather than by this type.
type LLMCall struct {
	ID    string
	Task  string
	Tier  int
	Model string

	// PromptTokens, CompletionTokens and LatencyMS are nil when the provider
	// never returned a usable response — a malformed body or a timeout has no
	// token counts to report, but latency is always known and is set even on
	// failure.
	PromptTokens     *int
	CompletionTokens *int
	LatencyMS        *int

	// Escalated and EscalationReason are supplied by the caller, never
	// inferred here — the client that writes this row has no way to know
	// why a call was retried at a different tier, only that it was.
	Escalated        bool
	EscalationReason *string

	// Error is the classified failure, nil on a successful call.
	Error *string

	// OccurrenceID ties a call to a specific occurrence, set by the
	// copywriter. Nil for calls that concern no single occurrence.
	OccurrenceID *string

	CreatedAt time.Time
}

// NewLLMCall is what a caller supplies to record one call. The store assigns
// the id and created_at.
type NewLLMCall struct {
	Task  string
	Tier  int
	Model string

	PromptTokens     *int
	CompletionTokens *int
	LatencyMS        *int

	Escalated        bool
	EscalationReason *string

	Error *string

	OccurrenceID *string
}

// Validate checks the fields that make a row findable and meaningful: an
// empty task or model, or a tier below 1, would still satisfy the DDL's NOT
// NULL constraints while being useless to query against.
func (n NewLLMCall) Validate() error {
	if n.Task == "" {
		return fmt.Errorf("domain: llm call task is empty")
	}
	if n.Tier < 1 {
		return fmt.Errorf("domain: llm call tier %d is less than 1", n.Tier)
	}
	if n.Model == "" {
		return fmt.Errorf("domain: llm call model is empty")
	}
	return nil
}

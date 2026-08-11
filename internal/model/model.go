// Package model is the model client: one package that can call an LLM, route
// by task and tier, and record what it did (docs/06-agent-spec.md#interface).
// All providers sit behind one OpenAI-compatible HTTP client — net/http and
// encoding/json, no SDK, per D-021 — with OpenRouter as the default path
// (docs/03-architecture.md#model-access).
//
// This package is mechanism, not policy. Complete resolves one (Task, Tier)
// pair to a configured endpoint, makes exactly one HTTP round trip bound by
// that tier's timeout, classifies the outcome into an Error (error.go), logs
// exactly one llm_calls row (L6) and two metric observations, and returns.
// It never retries a failed call, never decides to move to another tier, and
// never judges whether a successful completion is adequate — a reply with
// prose instead of a tool call is a successful Result, not an error, and
// whether that counts as a failure worth escalating (L4) is the escalation
// ladder's call, not this package's. The ladder arrives with the tool
// catalog next session, built on top of Complete rather than inside it.
//
// Every call is logged whether it succeeds or not (build note 6): a forced
// failure still produces a row, because llm_calls is the whole reason the
// tiering is worth building — after two weeks it turns tier-one success rate
// into a query instead of a guess.
package model

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"github.com/aidenpaleczny/navi/internal/domain"
)

// Task names one routing bucket, matching a row of
// docs/06-agent-spec.md#model-routing.
type Task string

const (
	TaskCRUD        Task = "crud"
	TaskBulkResolve Task = "bulk_resolve"
	TaskCopywriter  Task = "copywriter"
	TaskReconcile   Task = "reconcile"
	TaskDigest      Task = "digest"
)

// Role identifies who or what produced a Message, matching the
// OpenAI-compatible chat roles.
type Role string

const (
	RoleSystem    Role = "system"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleTool      Role = "tool"
)

// Message is one turn of the conversation sent to or received from a model.
type Message struct {
	Role    Role
	Content string

	// ToolCalls is set on an assistant message that invoked one or more
	// tools instead of (or alongside) replying in prose.
	ToolCalls []ToolCall

	// ToolCallID is set on a tool-role message answering one ToolCall by id.
	ToolCallID string
}

// Tool is one entry of the catalog advertised to the model. Parameters stays
// raw JSON Schema rather than a Go type: generating it from the argument
// structs (invopop/jsonschema) is next session's job, and this session only
// needs the shape to round-trip through the wire format untouched.
type Tool struct {
	Name        string
	Description string
	Parameters  json.RawMessage
}

// ToolCall is what a model asked to invoke. Arguments stays raw JSON for the
// same reason Tool.Parameters does — decoding it into a caller's own
// argument struct is the caller's job (Layer 1 validation, next session).
type ToolCall struct {
	ID        string
	Name      string
	Arguments json.RawMessage
}

// Escalation is supplied by the caller on a retried or escalated attempt. It
// is never inferred by this package — Complete has no way to know why a call
// is being repeated, only that the caller says it is, and its only
// obligation is to record that fact faithfully in the llm_calls row.
type Escalation struct {
	Reason string
}

// Request is one attempt at one tier of one task. Folded into a struct
// rather than positional parameters both because llm_calls has a column for
// each optional piece (Escalation, OccurrenceID) and because it is what lets
// this signature absorb what the ladder needs next session without another
// reshape.
type Request struct {
	Task Task

	// Tier is 1-indexed into Routing's tier list for Task, matching the
	// "Tier 1" / "Tier 2" columns the routing table and llm_calls both use.
	Tier int

	Messages []Message
	Tools    []Tool

	// Escalation is nil on a first attempt at tier 1.
	Escalation *Escalation

	// OccurrenceID is llm_calls.occurrence_id: set by the copywriter when a
	// call concerns one specific occurrence. Nil for calls that don't.
	OccurrenceID *string
}

// Result is what a successful Complete returns. Everything the escalation
// ladder needs to decide adequacy — a missing tool call, an empty streak,
// whatever comes next — is read off this struct, not signalled as an error.
type Result struct {
	Message Message

	Tier  int
	Model string

	// PromptTokens and CompletionTokens are the usage the provider reported.
	// Both zero when the provider omitted usage, which some do.
	PromptTokens     int
	CompletionTokens int

	Latency time.Duration

	// FinishReason is the provider's own word for why the completion ended:
	// "stop", "tool_calls", "length", and so on.
	FinishReason string
}

// Store is the narrow view this package needs, on the same pattern
// internal/scheduler and internal/sweeper use: a local interface naming
// exactly the one method Complete calls, satisfied by *store.Store without
// this package importing the whole repository module's surface.
type Store interface {
	CreateLLMCall(ctx context.Context, n domain.NewLLMCall) (domain.LLMCall, error)
}

// Metrics is the narrow view of internal/metrics this package needs.
type Metrics interface {
	IncLLMCall(task string, tier int, outcome string)
	ObserveLLMLatency(task string, tier int, seconds float64)
}

// Client calls one task's configured tiers. It holds no policy: which tier
// to call, and whether to call again, is the caller's decision on every
// invocation.
type Client struct {
	log     *slog.Logger
	routing *Routing
	apiKey  string
	http    *http.Client
	store   Store
	metrics Metrics
}

// New returns a Client. routing is typically model.LoadRouting's result,
// loaded once in main; apiKey is Config.Model.APIKey and may be
// empty in development, in which case every call fails at the provider with
// an authentication error classified KindUnavailable — there is no
// preflight check, on the same reasoning telegram.New gives for not
// checking its bot token at construction.
func New(log *slog.Logger, routing *Routing, apiKey string, st Store, m Metrics) *Client {
	return &Client{
		log:     log,
		routing: routing,
		apiKey:  apiKey,
		http:    &http.Client{},
		store:   st,
		metrics: m,
	}
}

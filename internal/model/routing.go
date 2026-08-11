package model

import (
	"bytes"
	"fmt"
	"net/url"
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

// Routing is the parsed /config/model.yaml: the per-task ordered tier list
// L1 asks for. Loaded once, in main, and the resulting value is passed to
// Client — the same caching decision defaults.Load makes for the same
// reason: hot-editing the ladder is not a requirement, so there is nothing
// to re-read for.
type Routing struct {
	Tasks map[Task]TaskRouting `yaml:"tasks"`
}

// TaskRouting is one row of the routing table in
// docs/06-agent-spec.md#model-routing.
type TaskRouting struct {
	// Thinking is carried through to the provider as a best-effort request
	// for reasoning mode. It is not load-bearing for this session — nothing
	// yet depends on it being honoured — but the routing table specifies it
	// per task, so the config shape holds it now rather than needing a
	// reshape when a caller starts caring.
	Thinking bool   `yaml:"thinking"`
	Tiers    []Tier `yaml:"tiers"`
}

// Tier is one endpoint: which model, which provider, how long a call is
// allowed to run before Complete gives up on it.
type Tier struct {
	Model          string `yaml:"model"`
	BaseURL        string `yaml:"base_url"`
	TimeoutSeconds int    `yaml:"timeout_seconds"`
}

// Timeout renders TimeoutSeconds as a Duration, which is the unit Complete
// actually needs it in.
func (t Tier) Timeout() time.Duration {
	return time.Duration(t.TimeoutSeconds) * time.Second
}

// LoadRouting reads and validates the routing table. Called once, from main
// (and independently, against the same file, by naviseed to prove the
// committed config/model.yaml itself is valid).
func LoadRouting(path string) (*Routing, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("model: read %s: %w", path, err)
	}

	var r Routing
	dec := yaml.NewDecoder(bytes.NewReader(data))
	// A misspelled key is a silently absent tier, which surfaces as "task X
	// has no tiers" rather than as a typo pointing at the line it's on.
	dec.KnownFields(true)
	if err := dec.Decode(&r); err != nil {
		return nil, fmt.Errorf("model: parse %s: %w", path, err)
	}

	if err := r.Validate(); err != nil {
		return nil, fmt.Errorf("model: %s: %w", path, err)
	}
	return &r, nil
}

// Validate checks the file is usable before anything calls Complete against
// it. A malformed table is a startup failure rather than a surprise on the
// first model call.
func (r *Routing) Validate() error {
	if len(r.Tasks) == 0 {
		return fmt.Errorf("tasks is empty")
	}
	for task, tr := range r.Tasks {
		if len(tr.Tiers) == 0 {
			return fmt.Errorf("task %q has no tiers", task)
		}
		for i, tier := range tr.Tiers {
			if tier.Model == "" {
				return fmt.Errorf("task %q tier %d: model is empty", task, i+1)
			}
			if tier.BaseURL == "" {
				return fmt.Errorf("task %q tier %d: base_url is empty", task, i+1)
			}
			if tier.TimeoutSeconds <= 0 {
				return fmt.Errorf("task %q tier %d: timeout_seconds must be positive, got %d", task, i+1, tier.TimeoutSeconds)
			}
		}
	}
	return nil
}

// Provider names which upstream a routing table's base_urls are expected to
// point at. Defined here with the same string values internal/config's
// OpenRouterProvider/GeminiProvider constants use — value-aligned by
// convention rather than imported, the same way internal/transport/logging's
// Name and config.LoggingTransport agree without either package importing
// the other. internal/model cannot import internal/config without inverting
// the dependency direction the package layout requires.
type Provider string

const (
	ProviderOpenRouter Provider = "openrouter"
	ProviderGemini     Provider = "gemini"
)

// providerHost is the one host each known provider's OpenAI-compatible
// endpoint answers at. A provider with more than one valid host is not a
// case this needs to handle yet.
var providerHost = map[Provider]string{
	ProviderOpenRouter: "openrouter.ai",
	ProviderGemini:     "generativelanguage.googleapis.com",
}

// ValidateProvider checks that every configured tier's base_url resolves to
// the host provider is expected to use. MODEL_PROVIDER (internal/config)
// picks which API key gets sent; config/model.yaml's base_url picks who
// receives it — two independent places to say "which provider," and D-016
// means the second one is hand-edited without a rebuild. Without this check,
// a MODEL_PROVIDER flip that isn't paired with a model.yaml edit sends a
// live key to the wrong host silently instead of failing the boot the way
// every other configuration mismatch in this repository does.
func (r *Routing) ValidateProvider(provider Provider) error {
	host, ok := providerHost[provider]
	if !ok {
		return fmt.Errorf("unknown provider %q", provider)
	}
	for task, tr := range r.Tasks {
		for i, tier := range tr.Tiers {
			u, err := url.Parse(tier.BaseURL)
			if err != nil || u.Host != host {
				return fmt.Errorf("task %q tier %d: base_url %q does not match provider %q (want host %q) — update config/model.yaml or MODEL_PROVIDER",
					task, i+1, tier.BaseURL, provider, host)
			}
		}
	}
	return nil
}

// TierCount reports how many tiers task is configured with, 0 if the task
// is not configured at all. tierFor only answers "is this one tier index
// valid" - the escalation ladder needs to know when it has run out of tiers
// to escalate to, which is a different question.
func (r *Routing) TierCount(task Task) int {
	return len(r.Tasks[task].Tiers)
}

// tierFor resolves (task, tier) to its configuration. tier is 1-indexed, to
// match the "Tier 1" / "Tier 2" columns the routing table and llm_calls both
// use. The returned error is always a *Error with Kind KindConfig — a
// caller-side mistake, never a provider condition.
func (r *Routing) tierFor(task Task, tier int) (Tier, *Error) {
	tr, ok := r.Tasks[task]
	if !ok {
		return Tier{}, &Error{Kind: KindConfig, Task: task, Tier: tier,
			Err: fmt.Errorf("model: task %q is not configured", task)}
	}
	if tier < 1 || tier > len(tr.Tiers) {
		return Tier{}, &Error{Kind: KindConfig, Task: task, Tier: tier,
			Err: fmt.Errorf("model: task %q has %d tier(s), tier %d requested", task, len(tr.Tiers), tier)}
	}
	return tr.Tiers[tier-1], nil
}

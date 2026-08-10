// Package conversation is the escalation ladder and the bounded intake loop
// that drains it (docs/06-agent-spec.md#escalation-ladder). It is the first
// caller of model.Client.Complete for the crud task, and the first caller of
// agent.Tools.Call driven by a real model rather than the hand-written
// arguments cmd/naviseed uses to prove the tools work without one.
//
// It imports model, agent, store, defaults, transport and domain - never SQL
// directly, on the same "all SQL lives in the repository module" discipline
// every other package follows.
package conversation

import (
	"context"
	"time"

	"github.com/aidenpaleczny/navi/internal/agent"
	"github.com/aidenpaleczny/navi/internal/defaults"
	"github.com/aidenpaleczny/navi/internal/domain"
	"github.com/aidenpaleczny/navi/internal/model"
	"github.com/aidenpaleczny/navi/internal/store"
	"github.com/aidenpaleczny/navi/internal/transport"
)

// Store is the narrow slice of the repository this package needs: writing
// every conversations row a turn produces, reading the last ~20 for A10's
// cross-turn history, and reading everything docs/06-agent-spec.md#context-
// injection's block needs - active items, today's occurrences, global pause,
// last touched, one item by id - the same live lookups agent.Tools' own
// resolveCreateZone makes, so there is nothing here to cache and nothing to
// drift.
type Store interface {
	CreateConversation(ctx context.Context, n domain.NewConversation) (domain.Conversation, bool, error)
	ListRecentConversations(ctx context.Context, limit int) ([]domain.Conversation, error)
	CurrentTZ(ctx context.Context) (string, bool, error)
	GlobalPauseUntil(ctx context.Context) (time.Time, bool, error)
	ListActiveItems(ctx context.Context) ([]domain.Item, error)
	TodaysOccurrences(ctx context.Context, loc *time.Location) ([]store.TodayOccurrence, error)
	LastTouchedItemID(ctx context.Context) (string, bool, error)
	GetItem(ctx context.Context, id string) (domain.Item, error)
}

// Sender is the outbound half this package needs - Send only, satisfied
// structurally by *telegram.Transport (or any future chat adapter) without
// this package importing a concrete one.
type Sender interface {
	Send(ctx context.Context, msg transport.Outbound) (string, error)
}

// Ladder holds everything one turn needs. Built once, in main (or naviseed),
// and passed by value into Handle - the same shape agent.Tools uses.
type Ladder struct {
	client      *model.Client
	tools       *agent.Tools
	routing     *model.Routing
	store       Store
	defaultsTbl *defaults.Table
	personaPath string
	defaultTZ   *time.Location
	sender      Sender
}

// New returns a Ladder.
func New(client *model.Client, tools *agent.Tools, routing *model.Routing,
	st Store, table *defaults.Table, personaPath string,
	defaultTZ *time.Location, sender Sender) *Ladder {
	return &Ladder{
		client:      client,
		tools:       tools,
		routing:     routing,
		store:       st,
		defaultsTbl: table,
		personaPath: personaPath,
		defaultTZ:   defaultTZ,
		sender:      sender,
	}
}

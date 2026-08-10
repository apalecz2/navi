// Package agent is the tool catalog and the three validation layers
// (docs/06-agent-spec.md#tool-catalog, #validation): argument structs for
// the four P1 tools, JSON Schema generated from those same structs, and the
// transactional execution that writes an item and its occurrences together.
//
// Session 10 invokes it directly from cmd/naviseed with hand-written
// arguments - no model client, no escalation ladder, no prompt assembly, no
// conversation loop. That split is the point: the write path is verifiable
// without spending a token or debugging a prompt at the same time, and the
// escalation ladder a later session builds gets to assume the tools already
// work.
//
// It imports store, materializer, schedule, defaults, domain and model (for
// model.Tool) - never SQL directly. "All SQL lives in the repository
// module" (CLAUDE.md) applies to agent tools exactly as it does to loop
// bodies and endpoint handlers.
package agent

import (
	"time"

	"github.com/aidenpaleczny/navi/internal/defaults"
	"github.com/aidenpaleczny/navi/internal/materializer"
	"github.com/aidenpaleczny/navi/internal/store"
)

// Tools holds what every handler needs, built once by a caller (main, or
// naviseed this session) and passed by value into Call - the same shape
// config.Load's callers use, never a global.
type Tools struct {
	store     *store.Store
	mat       *materializer.Materializer
	defaults  *defaults.Table
	defaultTZ *time.Location
}

// New builds a Tools.
func New(st *store.Store, mat *materializer.Materializer, table *defaults.Table, defaultTZ *time.Location) *Tools {
	return &Tools{store: st, mat: mat, defaults: table, defaultTZ: defaultTZ}
}

package domain

import (
	"fmt"
	"time"
)

// Role identifies who or what produced a conversation row, matching the
// CHECK constraint on conversations.role.
type Role string

const (
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleTool      Role = "tool"
)

func (r Role) valid() bool {
	switch r {
	case RoleUser, RoleAssistant, RoleTool:
		return true
	default:
		return false
	}
}

// Conversation is one row of agent message history: an inbound message, an
// assistant reply, or a tool call/result. Retention is 180 days, enforced by
// the sweeper rather than by this type.
type Conversation struct {
	ID      string
	Role    Role
	Content string

	ToolCalls  *string // JSON
	ToolCallID *string

	// Transport and ExternalID identify where an inbound message came from —
	// "telegram" and the update id, for example. Both nil for a row this
	// service generated itself (an assistant reply, a tool result). Together
	// they are the update-dedup key: idx_conv_dedup guards a duplicate
	// delivery from ever producing two rows.
	Transport  *string
	ExternalID *string

	// ContextRef ties a reply to what it answers, e.g. "reconcile:2026-08-05".
	// Stays nil until P3.
	ContextRef *string

	CreatedAt time.Time
}

// NewConversation is what a caller supplies to record one message. The store
// assigns the id and created_at.
type NewConversation struct {
	Role    Role
	Content string

	ToolCalls  *string
	ToolCallID *string

	Transport  *string
	ExternalID *string
	ContextRef *string
}

// Validate checks what the schema's CHECK constraint cannot: that Role is one
// of the three the DDL allows, and that Transport and ExternalID are set
// together or not at all. A row with only one of the pair set can never be
// found again by the dedup lookup, which is the failure this guards against
// rather than a column-level constraint.
func (n NewConversation) Validate() error {
	if !n.Role.valid() {
		return fmt.Errorf("domain: role %q is not user, assistant, or tool", n.Role)
	}
	if (n.Transport == nil) != (n.ExternalID == nil) {
		return fmt.Errorf("domain: transport and external_id must both be set or both be empty")
	}
	return nil
}

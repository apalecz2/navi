package domain

import (
	"encoding/json"
	"time"
)

// Proposal is a change the agent has suggested and nothing has applied (D-012,
// A12). It is not an item and not an occurrence and it touches neither: it is a
// record that a question was asked, with the exact answer that "yes" would mean,
// so that the next turn can recognise a reply to it the way it recognises a reply
// to a check-in - by context injection, not by a classifier.
//
// Nothing in the system applies one. The only way a proposal changes anything is
// an ordinary update_item or delete_item call made on a user's instruction in a
// later turn, through the same validation as any other write.
type Proposal struct {
	ID string `json:"id"`

	// Trigger is what produced it: one of the Trigger* constants, or
	// TriggerAgent when the model proposed in the middle of a conversation.
	Trigger string `json:"trigger"`

	ItemID    string `json:"item_id"`
	ItemTitle string `json:"item_title"`

	// Tool is the tool that applying the proposal would call, and Args its
	// arguments after item_id: update_item's "changes" object, or nothing for
	// delete_item. They are computed by code at proposal time, so what a "yes"
	// means is fixed before the person is asked and the message can be checked
	// against it.
	Tool    string          `json:"tool"`
	Changes json.RawMessage `json:"changes,omitempty"`

	// Summary is the alternative in one plain phrase ("move it to 19:30"). The
	// outbound message must state it; the composer's output is refused if it does
	// not mention each of Mentions.
	Summary  string   `json:"summary"`
	Mentions []string `json:"mentions,omitempty"`

	Rationale string `json:"rationale,omitempty"`

	Status    ProposalStatus `json:"status"`
	CreatedAt time.Time      `json:"created_at"`
	ExpiresAt time.Time      `json:"expires_at"`
}

// The proposal tools. A proposal names one of two because those are the two
// writes a person can say yes to: change the item, or archive it.
const (
	ProposalUpdateItem = "update_item"
	ProposalDeleteItem = "delete_item"
)

// The triggers (06-agent-spec.md, Proactive behaviour). TriggerAgent is not a
// trigger: it marks a proposal the model made because the user asked what it
// would suggest, and it is the only kind the daily cap does not govern.
const (
	TriggerMisses  = "consecutive_misses"
	TriggerSnoozes = "snooze_repeat"
	TriggerDormant = "dormancy"
	TriggerAgent   = "agent"
)

// ProposalStatus is where a proposal stands. The open state is the only one the
// agent is told about.
type ProposalStatus string

const (
	// ProposalOpen: offered, and a reply may still accept it.
	ProposalOpen ProposalStatus = "open"

	// ProposalAccepted: an update_item or delete_item landed on its item while it
	// was open. This is "a write followed", not "the write matched" - the person may
	// have asked for something other than what was proposed, and that is still
	// their instruction being followed.
	ProposalAccepted ProposalStatus = "accepted"

	// ProposalPassed: the first inbound turn that executed a tool after the
	// proposal did not touch its item. A "yes" a turn later no longer finds it.
	ProposalPassed ProposalStatus = "passed"

	// ProposalSuperseded: a newer proposal for the same item replaced it.
	ProposalSuperseded ProposalStatus = "superseded"
)

// ProposalTTL bounds how long an unanswered proposal stays open at all.
const ProposalTTL = 24 * time.Hour

// OpenAt reports whether the proposal can still be accepted at t.
func (p Proposal) OpenAt(t time.Time) bool {
	return p.Status == ProposalOpen && t.Before(p.ExpiresAt)
}

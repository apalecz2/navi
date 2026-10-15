package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"time"

	"github.com/aidenpaleczny/navi/internal/domain"
	"github.com/aidenpaleczny/navi/internal/store/sqlc"
)

// The proactive loop's state (docs/06-agent-spec.md, Proactive behaviour). Three
// kv shapes, none of them an occurrence: a proposal is a question that was asked,
// a latch is whether an episode has been asked about, and the count is the day's
// spend against the cap (A13).
//
// proposals and proactive_latches are each one key holding a JSON value rather
// than a family of key:{id} rows. kv has no prefix read, the sets are small and
// bounded (pruned on every write), and a single key is what lets "supersede the
// older proposal for this item" and "clear this latch" be one read-modify-write
// inside a transaction instead of a scan.
const (
	// KeyProposals holds a JSON array of domain.Proposal, every status, pruned.
	KeyProposals = "proposals"

	// KeyProactiveLatches holds a JSON object of Latch, keyed by Latch.Key.
	KeyProactiveLatches = "proactive_latches"

	// KeyProactiveCountPrefix + a local date is the day's count of unprompted
	// messages sent. The date is the person's, in the device zone.
	KeyProactiveCountPrefix = "proactive_count:"
)

// ContextRefProposal is the LIKE pattern for a proposal's context_ref, beside
// ContextRefReconcile and ContextRefBriefing. The value is "proposal:{id}".
const ContextRefProposal = "proposal:%"

// ProposalRef is the context_ref a proposal's outbound conversations row carries.
func ProposalRef(id string) string { return "proposal:" + id }

// proposalKeep is how long a closed proposal is retained. Long enough for an
// accepted-after-the-fact audit; the open set is bounded by TTL on its own.
const proposalKeep = 7 * 24 * time.Hour

// LatchState is where an episode is.
type LatchState string

const (
	// LatchDetected: the trigger's condition has been seen and nothing has been
	// sent. The message is owed - held by the quiet hours or the daily cap.
	LatchDetected LatchState = "detected"

	// LatchAsked: the message went out. Stays until the condition clears or the
	// re-arm period passes, which is what stops it firing every pass for as long
	// as the condition holds.
	LatchAsked LatchState = "asked"
)

// Latch is one episode of one trigger. Key is "item:{id}" for the two
// per-item triggers, which share a latch because they ask the same question, and
// "snooze:{root occurrence id}" for the per-chain one.
type Latch struct {
	Key        string     `json:"key"`
	Trigger    string     `json:"trigger"`
	ItemID     string     `json:"item_id"`
	State      LatchState `json:"state"`
	DetectedAt time.Time  `json:"detected_at"`
	AskedAt    *time.Time `json:"asked_at,omitempty"`

	// CappedOn is the local date a deferral for the cap was last counted, so a
	// trigger held all day is one "capped" and not one per pass.
	CappedOn string `json:"capped_on,omitempty"`
}

func readJSONKV[T any](ctx context.Context, q *sqlc.Queries, key string, into *T) error {
	raw, err := q.GetKV(ctx, key)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("store: get %s: %w", key, err)
	}
	if err := json.Unmarshal([]byte(raw), into); err != nil {
		return fmt.Errorf("store: decode %s: %w", key, err)
	}
	return nil
}

func writeJSONKV(ctx context.Context, q *sqlc.Queries, key string, v any, now time.Time) error {
	raw, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("store: encode %s: %w", key, err)
	}
	return q.SetKV(ctx, sqlc.SetKVParams{Key: key, Value: string(raw), UpdatedAt: domain.FormatTime(now)})
}

// pruneProposals drops closed proposals past proposalKeep, and open ones past
// their expiry (they are closed in effect already, and keeping them open-looking
// would make the stored status lie about what OpenAt says).
func pruneProposals(ps []domain.Proposal, now time.Time) []domain.Proposal {
	out := ps[:0:0]
	for _, p := range ps {
		if p.Status == domain.ProposalOpen && !now.Before(p.ExpiresAt) {
			continue
		}
		if p.Status != domain.ProposalOpen && now.Sub(p.CreatedAt) > proposalKeep {
			continue
		}
		out = append(out, p)
	}
	return out
}

// OpenProposals returns the proposals a reply can still accept at now, oldest
// first. It is the read behind the agent's Context ref block.
func (s *Store) OpenProposals(ctx context.Context, now time.Time) ([]domain.Proposal, error) {
	raw, ok, err := s.getKV(ctx, KeyProposals)
	if err != nil || !ok {
		return nil, err
	}
	var all []domain.Proposal
	if err := json.Unmarshal([]byte(raw), &all); err != nil {
		return nil, fmt.Errorf("store: decode %s: %w", KeyProposals, err)
	}
	var open []domain.Proposal
	for _, p := range all {
		if p.OpenAt(now) {
			open = append(open, p)
		}
	}
	sort.SliceStable(open, func(i, j int) bool { return open[i].CreatedAt.Before(open[j].CreatedAt) })
	return open, nil
}

// AllProposals returns every retained proposal, oldest first. Nothing in the
// message path reads it; naviseed and an operator do.
func (s *Store) AllProposals(ctx context.Context) ([]domain.Proposal, error) {
	raw, ok, err := s.getKV(ctx, KeyProposals)
	if err != nil || !ok {
		return nil, err
	}
	var all []domain.Proposal
	if err := json.Unmarshal([]byte(raw), &all); err != nil {
		return nil, fmt.Errorf("store: decode %s: %w", KeyProposals, err)
	}
	return all, nil
}

// addProposalTx stores p as open, superseding any open proposal for the same
// item - two live offers on one item would make "yes" ambiguous, and the newer
// is the one that was most recently asked.
func addProposalTx(ctx context.Context, q *sqlc.Queries, p domain.Proposal, now time.Time) error {
	var all []domain.Proposal
	if err := readJSONKV(ctx, q, KeyProposals, &all); err != nil {
		return err
	}
	for i := range all {
		if all[i].ItemID == p.ItemID && all[i].OpenAt(now) {
			all[i].Status = domain.ProposalSuperseded
		}
	}
	all = append(pruneProposals(all, now), p)
	return writeJSONKV(ctx, q, KeyProposals, all, now)
}

// NewProposal fills the fields every proposal gets from the store rather than
// from its author: the id, the open status, and the lifetime.
func NewProposal(p domain.Proposal, now time.Time) domain.Proposal {
	if p.ID == "" {
		p.ID = domain.NewID()
	}
	p.Status = domain.ProposalOpen
	p.CreatedAt = now
	p.ExpiresAt = now.Add(domain.ProposalTTL)
	return p
}

// AddProposal records a proposal the agent made mid-conversation. It writes the
// kv record and nothing else - no item, no occurrence - which is propose_change's
// whole write (D-012). The proactive loop's proposals go through
// RecordProactiveSend instead, which adds the message, the latch and the count in
// the same transaction.
func (s *Store) AddProposal(ctx context.Context, p domain.Proposal, now time.Time) (domain.Proposal, error) {
	p = NewProposal(p, now)
	if err := s.tx(ctx, func(q *sqlc.Queries) error { return addProposalTx(ctx, q, p, now) }); err != nil {
		return domain.Proposal{}, fmt.Errorf("store: add proposal: %w", err)
	}
	return p, nil
}

// AcceptProposals marks every open proposal for itemID accepted and returns them.
// It is called after an update_item or delete_item on that item has committed, so
// "accepted" never describes a write that did not happen; it is a record that one
// followed, which is what navi_proposals_accepted_total counts.
func (s *Store) AcceptProposals(ctx context.Context, itemID string, now time.Time) ([]domain.Proposal, error) {
	var accepted []domain.Proposal
	err := s.tx(ctx, func(q *sqlc.Queries) error {
		accepted = nil
		var all []domain.Proposal
		if err := readJSONKV(ctx, q, KeyProposals, &all); err != nil {
			return err
		}
		for i := range all {
			if all[i].ItemID == itemID && all[i].OpenAt(now) {
				all[i].Status = domain.ProposalAccepted
				accepted = append(accepted, all[i])
			}
		}
		if len(accepted) == 0 {
			return nil
		}
		return writeJSONKV(ctx, q, KeyProposals, pruneProposals(all, now), now)
	})
	if err != nil {
		return nil, fmt.Errorf("store: accept proposals: %w", err)
	}
	return accepted, nil
}

// PassProposals closes the named proposals that are still open. The ladder calls
// it with the ids it showed the model, once a turn has executed a tool: a
// proposal is offered for one turn, so a "yes" two turns later - to something
// else entirely - cannot find it.
func (s *Store) PassProposals(ctx context.Context, ids []string, now time.Time) error {
	if len(ids) == 0 {
		return nil
	}
	want := make(map[string]bool, len(ids))
	for _, id := range ids {
		want[id] = true
	}
	err := s.tx(ctx, func(q *sqlc.Queries) error {
		var all []domain.Proposal
		if err := readJSONKV(ctx, q, KeyProposals, &all); err != nil {
			return err
		}
		changed := false
		for i := range all {
			if want[all[i].ID] && all[i].Status == domain.ProposalOpen {
				all[i].Status = domain.ProposalPassed
				changed = true
			}
		}
		if !changed {
			return nil
		}
		return writeJSONKV(ctx, q, KeyProposals, pruneProposals(all, now), now)
	})
	if err != nil {
		return fmt.Errorf("store: pass proposals: %w", err)
	}
	return nil
}

// Latches returns every episode the proactive loop is tracking.
func (s *Store) Latches(ctx context.Context) (map[string]Latch, error) {
	raw, ok, err := s.getKV(ctx, KeyProactiveLatches)
	if err != nil || !ok {
		return map[string]Latch{}, err
	}
	out := map[string]Latch{}
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return nil, fmt.Errorf("store: decode %s: %w", KeyProactiveLatches, err)
	}
	return out, nil
}

// UpdateLatches applies fn to the latch map in one transaction and writes it back
// when fn reports a change. The proactive loop is the only writer, so this is not
// guarding a race between writers; it is what keeps a latch change and the
// message it belongs to (RecordProactiveSend) from being able to disagree.
func (s *Store) UpdateLatches(ctx context.Context, now time.Time, fn func(map[string]Latch) bool) error {
	err := s.tx(ctx, func(q *sqlc.Queries) error {
		latches := map[string]Latch{}
		if err := readJSONKV(ctx, q, KeyProactiveLatches, &latches); err != nil {
			return err
		}
		if !fn(latches) {
			return nil
		}
		return writeJSONKV(ctx, q, KeyProactiveLatches, latches, now)
	})
	if err != nil {
		return fmt.Errorf("store: update latches: %w", err)
	}
	return nil
}

// ProactiveCount is how many unprompted messages have gone out on the local
// date. Absent is zero: a day nothing was sent has no key.
func (s *Store) ProactiveCount(ctx context.Context, date string) (int, error) {
	raw, ok, err := s.getKV(ctx, KeyProactiveCountPrefix+date)
	if err != nil || !ok {
		return 0, err
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("store: %s%s: %w", KeyProactiveCountPrefix, date, err)
	}
	return n, nil
}

// RecordProactiveSend writes everything a delivered proposal leaves behind, in
// one transaction: the outbound conversations row with its context_ref, the
// proposal, the latch moving to asked, and the day's count.
//
// After the send and not before, the reconciler's order and for its reason: a
// crash between the two costs one duplicated question on the next pass (the latch
// is still detected), where the other order would record a question nobody was
// asked and spend the day's cap on it.
//
// yesterday is the previous local date; its count key is deleted here so the
// counter does not grow a key per day for ever. A gap longer than a day leaves an
// older key behind, which is a few bytes and not worth a prefix delete.
func (s *Store) RecordProactiveSend(ctx context.Context, p domain.Proposal, text, latchKey, date, yesterday string, sentAt time.Time) error {
	ref := ProposalRef(p.ID)
	conv := domain.NewConversation{Role: domain.RoleAssistant, Content: text, ContextRef: &ref}
	if err := conv.Validate(); err != nil {
		return err
	}

	err := s.tx(ctx, func(q *sqlc.Queries) error {
		if _, err := q.CreateConversation(ctx, newConversationParams(conv, domain.FormatTime(sentAt))); err != nil {
			return err
		}
		if err := addProposalTx(ctx, q, p, sentAt); err != nil {
			return err
		}

		latches := map[string]Latch{}
		if err := readJSONKV(ctx, q, KeyProactiveLatches, &latches); err != nil {
			return err
		}
		if l, ok := latches[latchKey]; ok {
			l.State = LatchAsked
			at := sentAt
			l.AskedAt = &at
			latches[latchKey] = l
			if err := writeJSONKV(ctx, q, KeyProactiveLatches, latches, sentAt); err != nil {
				return err
			}
		}

		n := 0
		raw, err := q.GetKV(ctx, KeyProactiveCountPrefix+date)
		switch {
		case err == nil:
			if n, err = strconv.Atoi(raw); err != nil {
				return fmt.Errorf("store: %s%s: %w", KeyProactiveCountPrefix, date, err)
			}
		case !errors.Is(err, sql.ErrNoRows):
			return err
		}
		if err := q.SetKV(ctx, sqlc.SetKVParams{
			Key: KeyProactiveCountPrefix + date, Value: strconv.Itoa(n + 1), UpdatedAt: domain.FormatTime(sentAt),
		}); err != nil {
			return err
		}
		if yesterday != "" && yesterday != date {
			return q.DeleteKV(ctx, KeyProactiveCountPrefix+yesterday)
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("store: record proactive send: %w", err)
	}
	return nil
}

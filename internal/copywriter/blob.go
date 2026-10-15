package copywriter

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"time"

	"github.com/aidenpaleczny/navi/internal/domain"
	"github.com/aidenpaleczny/navi/internal/stats"
	"github.com/aidenpaleczny/navi/internal/store"
)

// Blob is the context a message is written from (06-agent-spec.md, Context blob).
// Every number in it comes from stats.ItemSignals; nothing here computes one.
type Blob struct {
	Title          string  `json:"title"`
	Notes          *string `json:"notes"`
	ScheduledLocal string  `json:"scheduled_local"`
	IsSnoozeChild  bool    `json:"is_snooze_child"`

	CurrentStreak           int      `json:"current_streak"`
	LongestStreak           int      `json:"longest_streak"`
	Last7                   []string `json:"last_7"`
	CompletionRate30d       *float64 `json:"completion_rate_30d"`
	DaysSinceLastCompletion *int     `json:"days_since_last_completion"`
	MedianLagMinutes        *float64 `json:"median_lag_minutes"`
	SnoozesToday            int      `json:"snoozes_today"`
	RecentSkipNotes         []string `json:"recent_skip_notes"`

	// RecentMessages is G6: what this reminder said last, newest first.
	RecentMessages []string `json:"recent_messages"`

	// ConsecutiveMisses decides the tone ladder and is deliberately not sent to
	// the model. The strategy it selects is; the count itself is an invitation to
	// mention it.
	ConsecutiveMisses int `json:"-"`
}

func buildBlob(item domain.Item, cand store.CopyCandidate, sig stats.ItemSignals, recent []string, loc *time.Location) Blob {
	return Blob{
		Title:                   item.Title,
		Notes:                   item.Notes,
		ScheduledLocal:          cand.StartsAt.In(loc).Format(time.RFC3339),
		IsSnoozeChild:           cand.ParentOccurrenceID != nil,
		CurrentStreak:           sig.CurrentStreak,
		LongestStreak:           sig.LongestStreak,
		Last7:                   sig.Last7,
		CompletionRate30d:       sig.CompletionRate30d,
		DaysSinceLastCompletion: sig.DaysSinceLastCompletion,
		MedianLagMinutes:        sig.MedianLagMinutes,
		SnoozesToday:            sig.SnoozesToday,
		RecentSkipNotes:         sig.RecentSkipNotes,
		RecentMessages:          recent,
		ConsecutiveMisses:       sig.ConsecutiveMisses,
	}
}

// fingerprint is what pass 2 compares: a hash of every input that can change what
// the message should say, and of nothing that cannot.
//
// In: the strategy (so a branch flip is always a change), the title and notes,
// whether the row is a snooze child, the streak numbers, the recent outcomes, the
// miss run, the snoozes today, the skip notes, and the scheduled time. Out:
// recent_messages (it changes only when something was written, and a rewrite
// must not look like a reason to rewrite again) and the clock-derived fields
// (days since the last completion rolls over at midnight, which is not a change
// in what happened; a completion moves last_7 and the streak as well).
//
// "Pause moved" needs no term. A pause deletes the pending rows inside its window
// and a lifted one re-materializes them as new rows with no text, so by the time
// a row has safety-net text and is still pending its item has not been paused
// under it; the candidate read also drops a paused item outright.
func fingerprint(b Blob, s Strategy) string {
	basis := struct {
		Strategy       Strategy
		Title          string
		Notes          *string
		ScheduledLocal string
		IsSnoozeChild  bool
		Streak         int
		Longest        int
		Last7          []string
		Misses         int
		SnoozesToday   int
		SkipNotes      []string
	}{
		s, b.Title, b.Notes, b.ScheduledLocal, b.IsSnoozeChild,
		b.CurrentStreak, b.LongestStreak, b.Last7, b.ConsecutiveMisses,
		b.SnoozesToday, b.RecentSkipNotes,
	}
	raw, _ := json.Marshal(basis)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:8])
}

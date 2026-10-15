package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/aidenpaleczny/navi/internal/briefing"
	"github.com/aidenpaleczny/navi/internal/copywriter"
	"github.com/aidenpaleczny/navi/internal/defaults"
	"github.com/aidenpaleczny/navi/internal/domain"
	"github.com/aidenpaleczny/navi/internal/metrics"
	"github.com/aidenpaleczny/navi/internal/model"
	"github.com/aidenpaleczny/navi/internal/schedule"
	"github.com/aidenpaleczny/navi/internal/scheduler"
	"github.com/aidenpaleczny/navi/internal/stats"
	"github.com/aidenpaleczny/navi/internal/store"
)

// cwModel is a scripted provider for the copywriter. It keeps every request it
// was sent, answers from a function of the request count, and can run a hook
// before it answers - which is how a write is made to land after a claim: the
// hook claims the row while the "model" is still thinking.
type cwModel struct {
	mu        sync.Mutex
	prompts   []string
	reply     func(n int) (status int, text string)
	onRequest func(n int)
	srv       *httptest.Server
}

func newCWModel(reply func(n int) (int, string)) *cwModel {
	m := &cwModel{reply: reply}
	m.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var req struct {
			Messages []map[string]any `json:"messages"`
		}
		_ = json.Unmarshal(raw, &req)
		var prompt strings.Builder
		for _, msg := range req.Messages {
			if s, ok := msg["content"].(string); ok {
				prompt.WriteString(s)
				prompt.WriteString("\n")
			}
		}

		m.mu.Lock()
		m.prompts = append(m.prompts, prompt.String())
		n := len(m.prompts)
		hook := m.onRequest
		m.mu.Unlock()
		if hook != nil {
			hook(n)
		}

		status, text := m.reply(n)
		if status != http.StatusOK {
			w.WriteHeader(status)
			fmt.Fprint(w, `{"error":{"message":"scripted failure"}}`)
			return
		}
		escaped, _ := json.Marshal(text)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"model":"naviseed-copywriter","choices":[{"index":0,"message":{"role":"assistant","content":%s},"finish_reason":"stop"}]}`, string(escaped))
	}))
	return m
}

func (m *cwModel) count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.prompts)
}

func (m *cwModel) prompt(i int) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if i < 1 || i > len(m.prompts) {
		return ""
	}
	return m.prompts[i-1]
}

// countingMetrics wraps the registry so the scheduler's fallback counter can be
// read back as a number rather than scraped.
type countingMetrics struct {
	*metrics.Metrics
	mu        sync.Mutex
	fallbacks int
}

func (c *countingMetrics) IncCopywriterFallback() {
	c.mu.Lock()
	c.fallbacks++
	c.mu.Unlock()
	c.Metrics.IncCopywriterFallback()
}

// reportCopywriter drives the copywriter against a scripted provider: the G9
// gate, both passes, the snooze child, a write landing after a claim, a broken
// model, anti-repetition, and a persona edit with no restart.
//
// Time is handled in two ways, and the comments say which. The copywriter's own
// windows take an explicit instant (Run's `now`), so passes are driven at the
// instants they are about. The scheduler's claim reads the real clock, so "the
// reminder fires" is simulated with store.ClaimDue and an upper bound pinned to
// the row's own start - the same call scheduler.Fire makes, with the clock
// replaced - and the one place the real scheduler runs, it runs against a row
// whose start was moved into the past by a fixture write.
func reportCopywriter(ctx context.Context, st *store.Store, tz string, fallback *time.Location, dbPath string, log *slog.Logger) error {
	fmt.Println("\ncopywriter  generated reminder text, ahead of time")

	zones, err := schedule.LoadZones(ctx, st, fallback)
	if err != nil {
		return err
	}
	loc := zones.Local()
	m := metrics.New()
	svc := stats.New(st, fallback)

	raw, err := sql.Open("sqlite", dbPath+"?_pragma=busy_timeout(10000)")
	if err != nil {
		return err
	}
	defer func() { _ = raw.Close() }()

	if err := st.ClearGlobalPause(ctx); err != nil {
		return err
	}

	runID := strings.ToLower(domain.NewID()[16:])
	t0 := time.Now().Truncate(time.Second)
	at := func(daysAgo, h int) time.Time {
		n := t0.In(loc)
		return time.Date(n.Year(), n.Month(), n.Day()-daysAgo, h, 0, 0, 0, loc)
	}

	// --- fixtures ------------------------------------------------------------

	// history makes resolved chains for an item, oldest first, one per day at
	// 09:00 local ending yesterday.
	history := func(item domain.Item, outcomes ...domain.Status) error {
		for i, status := range outcomes {
			d := len(outcomes) - i
			when := at(d, 9)
			occ, err := fireOccurrence(ctx, st, item, when, nil)
			if err != nil {
				return err
			}
			var note *string
			if status == domain.StatusSkipped {
				note = ptr("away")
			}
			src := domain.ResolvedByWeb
			if status == domain.StatusMissed {
				src = domain.ResolvedByReconciler
			}
			if _, err := st.ResolveOccurrence(ctx, occ.ID, status, note, src, when.Add(10*time.Minute)); err != nil {
				return err
			}
		}
		return nil
	}
	completes := func(n int) []domain.Status {
		out := make([]domain.Status, n)
		for i := range out {
			out[i] = domain.StatusCompleted
		}
		return out
	}
	newItem := func(label string, resolved []domain.Status) (domain.Item, error) {
		item, err := seedFireItem(ctx, st, fmt.Sprintf("cw %s %s", label, runID), domain.NotifyAtTime, tz)
		if err != nil {
			return domain.Item{}, err
		}
		return item, history(item, resolved...)
	}
	pending := func(item domain.Item, start time.Time) (domain.Occurrence, error) {
		return fireOccurrence(ctx, st, item, start, nil)
	}
	row := func(id string) (domain.Occurrence, error) { return st.GetOccurrence(ctx, id) }
	callsFor := func(id string) (int, error) {
		rows, err := st.ListLLMCalls(ctx, 100000)
		if err != nil {
			return 0, err
		}
		n := 0
		for _, c := range rows {
			if c.OccurrenceID != nil && *c.OccurrenceID == id && c.Task == string(model.TaskCopywriter) {
				n++
			}
		}
		return n, nil
	}
	// fire simulates the scheduler claiming exactly this row.
	fire := func(o domain.Occurrence) (store.Due, bool, error) {
		_, due, err := st.ClaimDue(ctx, o.StartsAt, o.StartsAt, 10)
		if err != nil {
			return store.Due{}, false, err
		}
		for _, d := range due {
			if d.ID == o.ID {
				return d, true, nil
			}
		}
		return store.Due{}, false, nil
	}

	// The persona the generator reads, a file in a scratch directory so editing
	// it mid-run is the hot-read being exercised and not a claim about it.
	dir, err := os.MkdirTemp("", "naviseed-persona-")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	personaPath := filepath.Join(dir, "persona.md")
	if err := os.WriteFile(personaPath, []byte("PERSONA-V1: plain and brief.\n"), 0o600); err != nil {
		return err
	}

	newGen := func(p *cwModel) *copywriter.ModelGenerator {
		routing := &model.Routing{Tasks: map[model.Task]model.TaskRouting{
			model.TaskCopywriter: {Tiers: []model.Tier{
				{Model: "naviseed-copywriter", BaseURL: p.srv.URL, TimeoutSeconds: 5},
			}},
		}}
		client := model.New(log.With("component", "model-copywriter"), routing, "", st, m)
		return copywriter.NewModelGenerator(client, routing, personaPath)
	}
	newCW := func(g copywriter.Generator) *copywriter.Copywriter {
		return copywriter.New(log.With("loop", "copywriter-seed"), st, svc, g, m, fallback)
	}

	const (
		reply1 = "Evening walk time. 12 days running, worth keeping."
		reply3 = "A short walk is waiting for you."
	)

	// --- the tone ladder, as a table ------------------------------------------

	days := func(n int) *int { return &n }
	ladder := []struct {
		name string
		b    copywriter.Blob
		want copywriter.Strategy
	}{
		{"streak of 12", copywriter.Blob{CurrentStreak: 12}, copywriter.StrategyProtectStreak},
		{"streak of 5", copywriter.Blob{CurrentStreak: 5}, copywriter.StrategyProtectStreak},
		{"streak of 4", copywriter.Blob{CurrentStreak: 4}, copywriter.StrategyLightAck},
		{"one miss, nothing else", copywriter.Blob{ConsecutiveMisses: 1}, copywriter.StrategyNeutral},
		{"two misses", copywriter.Blob{ConsecutiveMisses: 2}, copywriter.StrategyShrinkAsk},
		{"four misses", copywriter.Blob{ConsecutiveMisses: 4}, copywriter.StrategyShrinkAsk},
		{"five misses", copywriter.Blob{ConsecutiveMisses: 5}, copywriter.StrategyDormancy},
		{"14 days and 2 misses", copywriter.Blob{ConsecutiveMisses: 2, DaysSinceLastCompletion: days(14)}, copywriter.StrategyDormancy},
		{"14 days, 2 misses, a skip note", copywriter.Blob{ConsecutiveMisses: 2, DaysSinceLastCompletion: days(14), RecentSkipNotes: []string{"away"}}, copywriter.StrategyShrinkAsk},
		{"14 days, no misses (a monthly item)", copywriter.Blob{DaysSinceLastCompletion: days(14)}, copywriter.StrategyNeutral},
		{"snooze child beats a 12-streak", copywriter.Blob{IsSnoozeChild: true, CurrentStreak: 12}, copywriter.StrategySnoozeChild},
		{"snooze child beats dormancy", copywriter.Blob{IsSnoozeChild: true, ConsecutiveMisses: 6}, copywriter.StrategySnoozeChild},
	}
	ladderOK := true
	for _, c := range ladder {
		if got := copywriter.Choose(c.b); got != c.want {
			ladderOK = false
			fmt.Printf("  ladder %-34s got %s want %s  FAILED\n", c.name, got, c.want)
		}
	}
	fmt.Printf("  tone ladder             %d states chosen by code  %s\n", len(ladder), verdict(ladderOK))

	// --- over the threshold: pass 1, written ahead of time ---------------------

	prov := newCWModel(func(n int) (int, string) {
		if n == 1 {
			return http.StatusOK, reply1
		}
		// Every later answer is different from every other, so "the text changed"
		// is a statement about the copywriter and not about the script.
		return http.StatusOK, fmt.Sprintf("Walk variant %d, whenever you're ready.", n)
	})
	defer prov.srv.Close()
	cw := newCW(newGen(prov))

	walk, err := newItem("walk", completes(12))
	if err != nil {
		return err
	}
	a, err := pending(walk, t0.Add(20*time.Minute))
	if err != nil {
		return err
	}

	res, err := cw.Run(ctx, t0)
	if err != nil {
		return err
	}
	aRow, err := row(a.ID)
	if err != nil {
		return err
	}
	aCalls, err := callsFor(a.ID)
	if err != nil {
		return err
	}
	pass1OK := res.Pass1 >= 1 && aRow.MessageText != nil && *aRow.MessageText == reply1 &&
		aRow.GenerationPass == domain.GenerationPassSafety && aRow.GenerationAttempts == 0 &&
		aRow.MessageBasis != nil && aRow.MessageGeneratedAt != nil && aRow.Status == domain.StatusPending &&
		aRow.StartsAt.After(time.Now())
	fmt.Printf("  pass 1, T-20m           text=%q pass=%d attempts=%d basis=%v still pending  %s\n",
		truncate(strOrNull(aRow.MessageText), 40), aRow.GenerationPass, aRow.GenerationAttempts,
		aRow.MessageBasis != nil, verdict(pass1OK))
	fmt.Printf("  llm_calls.occurrence_id %d row(s) name this occurrence, task copywriter  %s\n",
		aCalls, verdict(aCalls == 1))

	p1 := prov.prompt(1)
	fmt.Printf("  prompt that went out    persona=%v hard rule=%v strategy=%v streak 12=%v recent_messages key=%v  %s\n",
		strings.Contains(p1, "PERSONA-V1"), strings.Contains(p1, "Never guilt, never scold"),
		strings.Contains(p1, "State the current_streak"), strings.Contains(p1, `"current_streak": 12`),
		strings.Contains(p1, `"recent_messages"`),
		verdict(strings.Contains(p1, "PERSONA-V1") && strings.Contains(p1, "Never guilt, never scold") &&
			strings.Contains(p1, "State the current_streak") && strings.Contains(p1, `"current_streak": 12`) &&
			strings.Contains(p1, `"recent_messages"`) &&
			strings.Index(p1, "PERSONA-V1") < strings.Index(p1, "Hard rules")))

	// V6: the number in the prompt is the number on the stats page.
	sum, err := svc.Summary(ctx, stats.Query{Range: stats.RangeMonth, ItemID: walk.ID, Bucket: stats.BucketDay})
	if err != nil {
		return err
	}
	streakOK := len(sum.Items) == 1 && sum.Items[0].CurrentStreak == 12
	fmt.Printf("  streak agrees with stats  summary says %d in a row, prompt says 12  %s\n",
		func() int {
			if len(sum.Items) == 1 {
				return sum.Items[0].CurrentStreak
			}
			return -1
		}(), verdict(streakOK))

	due, claimed, err := fire(a)
	if err != nil {
		return err
	}
	body := domain.NotificationBody(due.MessageText, due.Title)
	fmt.Printf("  fires with the text     claimed=%v body=%q  %s\n", claimed, truncate(body, 50),
		verdict(claimed && body == reply1))

	// --- under the threshold: G9 ------------------------------------------------

	fresh, err := newItem("fresh", completes(3))
	if err != nil {
		return err
	}
	f, err := pending(fresh, t0.Add(21*time.Minute))
	if err != nil {
		return err
	}
	res, err = cw.Run(ctx, t0)
	if err != nil {
		return err
	}
	fRow, err := row(f.ID)
	if err != nil {
		return err
	}
	fCalls, err := callsFor(f.ID)
	if err != nil {
		return err
	}
	fDue, fClaimed, err := fire(f)
	if err != nil {
		return err
	}
	gateOK := res.Gated >= 1 && fRow.MessageText == nil && fRow.GenerationAttempts == 0 &&
		fRow.GenerationPass == 0 && fCalls == 0 && fClaimed &&
		domain.NotificationBody(fDue.MessageText, fDue.Title) == fresh.Title && fDue.GenerationAttempts == 0
	fmt.Printf("  3 resolved chains (<%d)  gated=%d text=%v attempts=%d llm_calls=%d fires as %q  %s\n",
		copywriter.MinResolvedChains, res.Gated, fRow.MessageText != nil, fRow.GenerationAttempts, fCalls,
		truncate(domain.NotificationBody(fDue.MessageText, fDue.Title), 30), verdict(gateOK))

	// Exactly at the threshold: 9 is gated, 10 is not. Counted in chains, so a
	// snoozed chain is one.
	edge, err := newItem("edge", completes(copywriter.MinResolvedChains-1))
	if err != nil {
		return err
	}
	e, err := pending(edge, t0.Add(23*time.Minute))
	if err != nil {
		return err
	}
	if _, err := cw.Run(ctx, t0); err != nil {
		return err
	}
	e9, err := row(e.ID)
	if err != nil {
		return err
	}
	if _, err := fireOccurrence(ctx, st, edge, at(0, 0).Add(time.Second), nil); err != nil {
		return err
	}
	last, err := st.ListOccurrencesForItem(ctx, edge.ID)
	if err != nil {
		return err
	}
	for _, o := range last {
		if o.StartsAt.Equal(at(0, 0).Add(time.Second)) {
			if _, err := st.ResolveOccurrence(ctx, o.ID, domain.StatusCompleted, nil, domain.ResolvedByWeb, o.StartsAt.Add(time.Minute)); err != nil {
				return err
			}
		}
	}
	if _, err := cw.Run(ctx, t0); err != nil {
		return err
	}
	e10, err := row(e.ID)
	if err != nil {
		return err
	}
	fmt.Printf("  the threshold itself    9 chains: text=%v   10 chains: text=%v  %s\n",
		e9.MessageText != nil, e10.MessageText != nil, verdict(e9.MessageText == nil && e10.MessageText != nil))

	// --- pass 2: only when state changed ----------------------------------------

	// The claim above moved A to notified, so pass 2 needs a fresh row; reuse the
	// same item so the history and the recent messages carry over.
	b, err := pending(walk, t0.Add(40*time.Minute))
	if err != nil {
		return err
	}
	t30 := b.StartsAt.Add(-25 * time.Minute)
	if _, err := cw.Run(ctx, t30); err != nil {
		return err
	}
	bRow, err := row(b.ID)
	if err != nil {
		return err
	}
	callsAfter1 := prov.count()
	t4 := b.StartsAt.Add(-3 * time.Minute)
	res, err = cw.Run(ctx, t4)
	if err != nil {
		return err
	}
	bUnchanged, err := row(b.ID)
	if err != nil {
		return err
	}
	noChange := res.Unchanged >= 1 && prov.count() == callsAfter1 && bUnchanged.GenerationPass == 1 &&
		bUnchanged.MessageText != nil && bRow.MessageText != nil && *bUnchanged.MessageText == *bRow.MessageText
	fmt.Printf("  pass 2, nothing changed unchanged=%d new model calls=%d pass still %d  %s\n",
		res.Unchanged, prov.count()-callsAfter1, bUnchanged.GenerationPass, verdict(noChange))

	// A sibling occurrence of the same item, earlier today, ends missed. That is
	// a change in what the message is written from: the streak is gone.
	sib, err := pending(walk, t0.Add(-30*time.Minute))
	if err != nil {
		return err
	}
	if _, err := st.ResolveOccurrence(ctx, sib.ID, domain.StatusMissed, nil, domain.ResolvedByReconciler, t0); err != nil {
		return err
	}
	if err := os.WriteFile(personaPath, []byte("PERSONA-V2: warm and brief.\n"), 0o600); err != nil {
		return err
	}
	before2 := prov.count()
	res, err = cw.Run(ctx, t4)
	if err != nil {
		return err
	}
	bFresh, err := row(b.ID)
	if err != nil {
		return err
	}
	bCalls, err := callsFor(b.ID)
	if err != nil {
		return err
	}
	changed := res.Pass2 >= 1 && prov.count() == before2+1 && bFresh.GenerationPass == domain.GenerationPassFresh &&
		bFresh.MessageText != nil && *bFresh.MessageText != *bRow.MessageText &&
		bFresh.MessageBasis != nil && *bFresh.MessageBasis != *bRow.MessageBasis && bCalls == 2
	fmt.Printf("  pass 2, state changed   pass=%d text changed=%v basis changed=%v llm_calls for row=%d  %s\n",
		bFresh.GenerationPass, bFresh.MessageText != nil && *bFresh.MessageText != *bRow.MessageText,
		bFresh.MessageBasis != nil && *bFresh.MessageBasis != *bRow.MessageBasis, bCalls, verdict(changed))

	p2 := prov.prompt(prov.count())
	personaOK := strings.Contains(p2, "PERSONA-V2") && !strings.Contains(p2, "PERSONA-V1")
	fmt.Printf("  persona.md edited, no restart  next prompt carries V2=%v and not V1=%v  %s\n",
		strings.Contains(p2, "PERSONA-V2"), !strings.Contains(p2, "PERSONA-V1"), verdict(personaOK))

	// The same function serves the other readers, so the same edit reaches them.
	// The briefing composer is driven for real; the agent prompt and the
	// reconciler composer call the identical defaults.GetPersona(path).
	viaGet, err := defaults.GetPersona(personaPath)
	if err != nil {
		return err
	}
	bp := newCWModelBriefing()
	defer bp.srv.Close()
	routing := &model.Routing{Tasks: map[model.Task]model.TaskRouting{
		model.TaskBriefing: {Tiers: []model.Tier{{Model: "naviseed-briefing", BaseURL: bp.srv.URL, TimeoutSeconds: 5}}},
	}}
	bc := briefing.NewModelComposer(model.New(log.With("component", "model-persona"), routing, "", st, m), routing, personaPath)
	if _, err := bc.Compose(ctx, briefing.Context{Local: t0}); err != nil {
		return err
	}
	fmt.Printf("  one persona, every reader  get_persona sees V2=%v, briefing composer's prompt carries V2=%v  %s\n",
		strings.Contains(viaGet, "PERSONA-V2"), strings.Contains(bp.prompt(1), "PERSONA-V2"),
		verdict(strings.Contains(viaGet, "PERSONA-V2") && strings.Contains(bp.prompt(1), "PERSONA-V2")))

	// --- anti-repetition ---------------------------------------------------------

	rep := newCWModel(func(n int) (int, string) {
		if n == 1 {
			return http.StatusOK, *bFresh.MessageText // the very text it just wrote
		}
		return http.StatusOK, reply3
	})
	defer rep.srv.Close()
	cwRep := newCW(newGen(rep))
	c, err := pending(walk, t0.Add(26*time.Minute))
	if err != nil {
		return err
	}
	if _, err := cwRep.Run(ctx, t0); err != nil {
		return err
	}
	c1, err := row(c.ID)
	if err != nil {
		return err
	}
	repeatRejected := c1.MessageText == nil && c1.GenerationAttempts == 1
	if _, err := cwRep.Run(ctx, t0); err != nil {
		return err
	}
	c2, err := row(c.ID)
	if err != nil {
		return err
	}
	pc := rep.prompt(1)
	plumbing := strings.Contains(pc, *bFresh.MessageText) && strings.Contains(pc, `"recent_messages": [`)
	fmt.Printf("  recent_messages         in the prompt that went out=%v; a verbatim repeat was rejected=%v, then %q stored  %s\n",
		plumbing, repeatRejected, truncate(strOrNull(c2.MessageText), 36),
		verdict(plumbing && repeatRejected && c2.MessageText != nil && *c2.MessageText == reply3 && c2.GenerationAttempts == 0))

	// --- a snooze child ------------------------------------------------------------

	snoozeProv := newCWModel(func(n int) (int, string) { return http.StatusOK, "Still here: the walk, when you're ready." })
	defer snoozeProv.srv.Close()
	cwSnooze := newCW(newGen(snoozeProv))
	root, err := pending(walk, t0.Add(55*time.Minute))
	if err != nil {
		return err
	}
	if _, ok, err := fire(root); err != nil || !ok {
		return fmt.Errorf("snooze fixture: claiming the root: ok=%v err=%v", ok, err)
	}
	snz, err := st.SnoozeOccurrence(ctx, root.ID, domain.ResolvedByWeb, t0,
		func(domain.Item, domain.Occurrence) (time.Time, error) { return t0.Add(10 * time.Minute), nil })
	if err != nil {
		return err
	}
	// The child's own T-30 passed before it existed. It is inside the window, so
	// the very next pass picks it up.
	res, err = cwSnooze.Run(ctx, t0)
	if err != nil {
		return err
	}
	child, err := row(snz.Child.ID)
	if err != nil {
		return err
	}
	cp := snoozeProv.prompt(1)
	childOK := snz.Child.ParentOccurrenceID != nil && child.MessageText != nil && child.GenerationPass == 1 &&
		strings.Contains(cp, "This is a second attempt") && strings.Contains(cp, `"is_snooze_child": true`)
	fmt.Printf("  snooze child, 10m       generated on the next tick: text=%v pass=%d, snooze-child strategy in prompt=%v  %s\n",
		child.MessageText != nil, child.GenerationPass, strings.Contains(cp, "This is a second attempt"), verdict(childOK))

	// A child with less than MinLead left is not worth a call: it fires as its title.
	soon, err := pending(walk, t0.Add(copywriter.MinLead-10*time.Second))
	if err != nil {
		return err
	}
	callsBefore := snoozeProv.count()
	if _, err := cwSnooze.Run(ctx, t0); err != nil {
		return err
	}
	soonRow, err := row(soon.ID)
	if err != nil {
		return err
	}
	fmt.Printf("  row starting in %ds    no call, no attempt, fires as its title  %s\n",
		int(copywriter.MinLead.Seconds())-10,
		verdict(soonRow.MessageText == nil && soonRow.GenerationAttempts == 0 && snoozeProv.count() == callsBefore))

	// --- a write landing after the scheduler's claim --------------------------------

	lateProv := newCWModel(func(n int) (int, string) { return http.StatusOK, "A message that arrives too late." })
	defer lateProv.srv.Close()
	cwLate := newCW(newGen(lateProv))
	lateItem, err := newItem("late", completes(11))
	if err != nil {
		return err
	}
	late, err := pending(lateItem, t0.Add(22*time.Minute+17*time.Second))
	if err != nil {
		return err
	}
	var claimedMidCall bool
	lateProv.mu.Lock()
	lateProv.onRequest = func(int) {
		// The scheduler wins the race while the model is still answering.
		_, claimedMidCall, _ = fire(late)
	}
	lateProv.mu.Unlock()
	res, err = cwLate.Run(ctx, t0)
	if err != nil {
		return err
	}
	lateRow, err := row(late.ID)
	if err != nil {
		return err
	}
	lateOK := claimedMidCall && res.Late >= 1 && lateRow.Status == domain.StatusNotified &&
		lateRow.MessageText == nil && lateRow.GenerationPass == 0
	fmt.Printf("  late write              claimed mid-call=%v late=%d row now %s, text=%v pass=%d  %s\n",
		claimedMidCall, res.Late, lateRow.Status, lateRow.MessageText != nil, lateRow.GenerationPass, verdict(lateOK))

	// And directly: a write at the store against a row that is no longer pending
	// changes nothing, whatever pass it names.
	landed, err := st.WriteGeneratedMessage(ctx, late.ID, store.Generated{
		Text: "overwrite", Model: "x", At: time.Now(), Pass: domain.GenerationPassSafety, Basis: "x",
	})
	if err != nil {
		return err
	}
	again, err := row(late.ID)
	if err != nil {
		return err
	}
	fmt.Printf("  write to a notified row landed=%v, text=%v  %s\n",
		landed, again.MessageText != nil, verdict(!landed && again.MessageText == nil))

	// --- a broken model: plain titles, bounded attempts -------------------------------

	down := newCWModel(func(n int) (int, string) { return http.StatusInternalServerError, "" })
	defer down.srv.Close()
	cwDown := newCW(newGen(down))
	downItem, err := newItem("down", completes(11))
	if err != nil {
		return err
	}
	d, err := pending(downItem, t0.Add(24*time.Minute))
	if err != nil {
		return err
	}
	for i := 0; i < 3; i++ {
		if _, err := cwDown.Run(ctx, t0); err != nil {
			return err
		}
	}
	dRow, err := row(d.ID)
	if err != nil {
		return err
	}
	dCalls, err := callsFor(d.ID)
	if err != nil {
		return err
	}
	fmt.Printf("  model down, 3 passes    calls=%d (cap %d) attempts=%d text=%v  %s\n",
		dCalls, copywriter.MaxAttempts, dRow.GenerationAttempts, dRow.MessageText != nil,
		verdict(dCalls == copywriter.MaxAttempts && dRow.GenerationAttempts == copywriter.MaxAttempts && dRow.MessageText == nil))

	// The real scheduler, against rows moved into the past: the failed one is a
	// counted fallback, the gated one is a plain title that is not.
	if _, err := raw.ExecContext(ctx, `UPDATE occurrences SET starts_at = ? WHERE id IN (?, ?)`,
		domain.FormatTime(time.Now().Add(-20*time.Second)), d.ID, f.ID); err != nil {
		return err
	}
	// f was already claimed above; reopen it as pending for this pass.
	if _, err := raw.ExecContext(ctx, `UPDATE occurrences SET status = 'pending', notified_at = NULL WHERE id = ?`, f.ID); err != nil {
		return err
	}
	cm := &countingMetrics{Metrics: m}
	rec := &recordingTransport{}
	sched := scheduler.New(log.With("loop", "scheduler-copywriter"), st, rec, cm, time.Now())
	sres, err := sched.Fire(ctx)
	if err != nil {
		return err
	}
	fmt.Printf("  scheduler, model down   both fire as titles (%v, %v); fallback counter +%d (only the failed one)  %s\n",
		rec.count(downItem.Title) == 1, rec.count(fresh.Title) == 1, cm.fallbacks,
		verdict(rec.count(downItem.Title) == 1 && rec.count(fresh.Title) == 1 && sres.FailedGenerations == 1 && cm.fallbacks == 1))

	// A 401 is a deleted or revoked key. Same outcome, and the reminders still go.
	keyGone := newCWModel(func(n int) (int, string) { return http.StatusUnauthorized, "" })
	defer keyGone.srv.Close()
	cwKey := newCW(newGen(keyGone))
	keyItem, err := newItem("key", completes(11))
	if err != nil {
		return err
	}
	k, err := pending(keyItem, t0.Add(27*time.Minute))
	if err != nil {
		return err
	}
	if _, err := cwKey.Run(ctx, t0); err != nil {
		return err
	}
	kRow, err := row(k.ID)
	if err != nil {
		return err
	}
	kDue, kOK, err := fire(k)
	if err != nil {
		return err
	}
	fmt.Printf("  model key rejected      text=%v, fires as %q  %s\n", kRow.MessageText != nil,
		truncate(domain.NotificationBody(kDue.MessageText, kDue.Title), 30),
		verdict(kOK && kRow.MessageText == nil && domain.NotificationBody(kDue.MessageText, kDue.Title) == keyItem.Title))

	// No generator at all: nothing runs and nothing is logged.
	none := newCW(nil)
	nRes, err := none.Run(ctx, t0)
	if err != nil {
		return err
	}
	fmt.Printf("  no model configured     disabled=%v  %s\n", nRes.Disabled, verdict(nRes.Disabled))

	// --- vacation mode ------------------------------------------------------------------

	pausedProv := newCWModel(func(n int) (int, string) { return http.StatusOK, "should not be asked" })
	defer pausedProv.srv.Close()
	pr, err := newItem("paused", completes(11))
	if err != nil {
		return err
	}
	if _, err := pending(pr, t0.Add(28*time.Minute)); err != nil {
		return err
	}
	if err := st.SetGlobalPauseUntil(ctx, t0.Add(48*time.Hour)); err != nil {
		return err
	}
	pRes, err := newCW(newGen(pausedProv)).Run(ctx, t0)
	if err != nil {
		return err
	}
	if err := st.ClearGlobalPause(ctx); err != nil {
		return err
	}
	fmt.Printf("  global pause            paused=%v model calls=%d  %s\n", pRes.Paused, pausedProv.count(),
		verdict(pRes.Paused && pausedProv.count() == 0))

	return nil
}

// newCWModelBriefing is a provider that answers a briefing request, so the
// persona edit can be seen reaching a second reader.
func newCWModelBriefing() *cwModel {
	return newCWModel(func(n int) (int, string) { return http.StatusOK, "Today looks light. Anything to add?" })
}

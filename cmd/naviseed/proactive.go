package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/aidenpaleczny/navi/internal/agent"
	"github.com/aidenpaleczny/navi/internal/conversation"
	"github.com/aidenpaleczny/navi/internal/copywriter"
	"github.com/aidenpaleczny/navi/internal/defaults"
	"github.com/aidenpaleczny/navi/internal/domain"
	"github.com/aidenpaleczny/navi/internal/materializer"
	"github.com/aidenpaleczny/navi/internal/metrics"
	"github.com/aidenpaleczny/navi/internal/model"
	"github.com/aidenpaleczny/navi/internal/proactive"
	"github.com/aidenpaleczny/navi/internal/schedule"
	"github.com/aidenpaleczny/navi/internal/stats"
	"github.com/aidenpaleczny/navi/internal/store"
	"github.com/aidenpaleczny/navi/internal/transport"
	"github.com/aidenpaleczny/navi/internal/transport/telegram"
)

// paMetrics reads the proactive counters back as numbers instead of scraping
// them. It implements proactive.Metrics and agent.Metrics, so one value is
// handed to both and "accepted" is counted at the edge production counts it.
type paMetrics struct {
	*metrics.Metrics
	mu        sync.Mutex
	triggers  map[string]int
	sent      map[string]int
	accepted  map[string]int
	fallbacks int
	capped    int
}

func newPAMetrics() *paMetrics {
	return &paMetrics{
		Metrics:  metrics.New(),
		triggers: map[string]int{}, sent: map[string]int{}, accepted: map[string]int{},
	}
}

func (p *paMetrics) IncTrigger(k string) {
	p.mu.Lock()
	p.triggers[k]++
	p.mu.Unlock()
	p.Metrics.IncTrigger(k)
}

func (p *paMetrics) IncProposalSent(k string) {
	p.mu.Lock()
	p.sent[k]++
	p.mu.Unlock()
	p.Metrics.IncProposalSent(k)
}

func (p *paMetrics) IncProposalFallback() {
	p.mu.Lock()
	p.fallbacks++
	p.mu.Unlock()
	p.Metrics.IncProposalFallback()
}

func (p *paMetrics) IncProposalCapped() {
	p.mu.Lock()
	p.capped++
	p.mu.Unlock()
	p.Metrics.IncProposalCapped()
}

func (p *paMetrics) IncProposalAccepted(k string) {
	p.mu.Lock()
	p.accepted[k]++
	p.mu.Unlock()
	p.Metrics.IncProposalAccepted(k)
}

func (p *paMetrics) get(m map[string]int, k string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return m[k]
}

func (p *paMetrics) total(m map[string]int) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	n := 0
	for _, v := range m {
		n += v
	}
	return n
}

// paStore and paSignals restrict the loop to this section's fixtures. By the time
// this runs, earlier sections have left dozens of items in the database, several
// built to have a five-miss run (the copywriter's, the statistics'), and a loop
// that read them all would be asserting about the whole file and not about the
// section. The loop takes interfaces for exactly this.
type paStore struct {
	*store.Store
	ids map[string]bool
}

func (s paStore) ListActiveItems(ctx context.Context) ([]domain.Item, error) {
	all, err := s.Store.ListActiveItems(ctx)
	if err != nil {
		return nil, err
	}
	out := all[:0:0]
	for _, it := range all {
		if s.ids[it.ID] {
			out = append(out, it)
		}
	}
	return out, nil
}

type paSignals struct {
	*stats.Service
	ids map[string]bool
}

func (s paSignals) SnoozedChains(ctx context.Context) ([]store.StatsChain, error) {
	all, err := s.Service.SnoozedChains(ctx)
	if err != nil {
		return nil, err
	}
	out := all[:0:0]
	for _, c := range all {
		if s.ids[c.ItemID] {
			out = append(out, c)
		}
	}
	return out, nil
}

// reportProactive drives the proactive loop through its triggers, its latches, its
// quiet hours and its cap, and then drives replies to what it sent through a real
// conversation.Ladder.
//
// Time is handled in two ways, and the comments say which. The loop takes an
// explicit now, so the quiet hours, the cap and "the next day" are checked at
// instants constructed in the PAST (two days ago, yesterday): a message sent at a
// constructed future instant would leave a future-dated conversations row at the
// top of every later history read, and naviseed runs in a shared database. The
// passes that must be at the real clock - the ones a reply is then driven against,
// because a proposal expires 24 hours after it was sent - use a loop whose window
// is opened with WithWindow, so they work at whatever hour this is run.
func reportProactive(ctx context.Context, st *store.Store, table *defaults.Table, tz string, fallback *time.Location, dbPath string, log *slog.Logger) error {
	fmt.Println("\nproactive  triggers, latches, quiet hours, the cap, and propose_change")

	zones, err := schedule.LoadZones(ctx, st, fallback)
	if err != nil {
		return err
	}
	loc := zones.Local()
	if err := st.ClearGlobalPause(ctx); err != nil {
		return err
	}

	// A clean slate for the proactive loop's three kv shapes. The one raw write in
	// this section, on a second connection, as the copywriter's and the statistics'
	// sections do for the same reason: the store deliberately has no surface that
	// forgets the day's count or the proposals, and without this a second run on
	// the same database would start with today's cap already spent and report the
	// cap working as a failure of everything after it.
	raw, err := sql.Open("sqlite", dbPath+"?_pragma=busy_timeout(10000)")
	if err != nil {
		return err
	}
	defer func() { _ = raw.Close() }()
	if _, err := raw.ExecContext(ctx, `DELETE FROM kv WHERE key IN (?, ?) OR key LIKE ?`,
		store.KeyProposals, store.KeyProactiveLatches, store.KeyProactiveCountPrefix+"%"); err != nil {
		return err
	}

	runID := strings.ToLower(domain.NewID()[16:])
	now := time.Now().Truncate(time.Second)

	// --- fixtures ------------------------------------------------------------

	local := now.In(loc)
	daysAgo := func(d, h, m int) time.Time {
		return time.Date(local.Year(), local.Month(), local.Day()-d, h, m, 0, 0, loc)
	}

	newItem := func(label, sched string) (domain.Item, error) {
		policy := domain.NotifyAtTime
		return st.CreateItem(ctx, domain.NewItem{
			Title:        fmt.Sprintf("pa %s %s", label, runID),
			Schedule:     json.RawMessage(sched),
			TZ:           tz,
			NotifyPolicy: &policy,
		})
	}
	type outcome struct {
		daysAgo int
		status  domain.Status
	}
	history := func(item domain.Item, os ...outcome) error {
		for _, o := range os {
			when := daysAgo(o.daysAgo, 9, 0)
			occ, err := fireOccurrence(ctx, st, item, when, nil)
			if err != nil {
				return err
			}
			var note *string
			if o.status == domain.StatusSkipped {
				note = ptr("away")
			}
			src := domain.ResolvedByWeb
			if o.status == domain.StatusMissed {
				src = domain.ResolvedByReconciler
			}
			if _, err := st.ResolveOccurrence(ctx, occ.ID, o.status, note, src, when.Add(10*time.Minute)); err != nil {
				return err
			}
		}
		return nil
	}
	const (
		daily    = `{"kind":"fixed","rrule":"FREQ=DAILY","at":"18:00"}`
		weekdays = `{"kind":"fixed","rrule":"FREQ=WEEKLY;BYDAY=MO,TU,WE,TH,FR","at":"18:00"}`
	)
	done, miss, skip := domain.StatusCompleted, domain.StatusMissed, domain.StatusSkipped

	// A: five misses after a completion. The 5-miss trigger.
	itemA, err := newItem("misses", daily)
	if err != nil {
		return err
	}
	if err := history(itemA, outcome{9, done}, outcome{5, miss}, outcome{4, miss}, outcome{3, miss}, outcome{2, miss}, outcome{1, miss}); err != nil {
		return err
	}
	// B: a completion twenty days ago and three misses since. Dormant, and not a
	// five-miss run: the days rule is the one that applies.
	itemB, err := newItem("dormant", weekdays)
	if err != nil {
		return err
	}
	if err := history(itemB, outcome{20, done}, outcome{3, miss}, outcome{2, miss}, outcome{1, miss}); err != nil {
		return err
	}
	// D: three misses, a skip, two misses. Five in the streak rule's sense, because
	// a skip is neutral - it neither ends a run nor extends one. If the trigger and
	// the statistics page disagreed about this, D would not fire.
	itemD, err := newItem("skip between", daily)
	if err != nil {
		return err
	}
	if err := history(itemD, outcome{8, miss}, outcome{7, miss}, outcome{6, miss}, outcome{5, skip}, outcome{4, miss}, outcome{3, miss}); err != nil {
		return err
	}
	// E: four misses and a skip: one short, and never completed, so not dormant.
	itemE, err := newItem("four and a skip", daily)
	if err != nil {
		return err
	}
	if err := history(itemE, outcome{5, miss}, outcome{4, miss}, outcome{3, miss}, outcome{2, miss}, outcome{1, skip}); err != nil {
		return err
	}
	// F: four misses, a completion, two misses. The completion ended the run.
	itemF, err := newItem("run reset", daily)
	if err != nil {
		return err
	}
	if err := history(itemF, outcome{8, miss}, outcome{7, miss}, outcome{6, miss}, outcome{5, miss}, outcome{4, done}, outcome{2, miss}, outcome{1, miss}); err != nil {
		return err
	}
	// C: one occurrence snoozed three times. Each snooze goes through the real
	// store surface, and each pending child is claimed the way the scheduler
	// claims it, because only a notified row can be snoozed.
	itemC, err := newItem("snoozed", daily)
	if err != nil {
		return err
	}
	rootStart := daysAgo(1, 18, 0)
	cur, err := fireOccurrence(ctx, st, itemC, rootStart, nil)
	if err != nil {
		return err
	}
	for i := 0; i < 3; i++ {
		if _, _, err := st.ClaimDue(ctx, cur.StartsAt, cur.StartsAt, 10); err != nil {
			return err
		}
		sn, err := st.SnoozeOccurrence(ctx, cur.ID, domain.ResolvedByWeb, time.Now(),
			func(_ domain.Item, occ domain.Occurrence) (time.Time, error) { return occ.StartsAt.Add(time.Hour), nil })
		if err != nil {
			return err
		}
		cur = sn.Child
	}

	ids := map[string]bool{}
	for _, it := range []domain.Item{itemA, itemB, itemC, itemD, itemE, itemF} {
		ids[it.ID] = true
	}

	pm := newPAMetrics()
	svc := stats.New(st, fallback)
	scoped := paStore{st, ids}
	sig := paSignals{svc, ids}
	rec := &recordingTransport{}
	newLoop := func(c proactive.Composer) *proactive.Proactive {
		return proactive.New(log.With("loop", "proactive-seed"), scoped, sig, rec, pm, c, fallback)
	}
	loop := newLoop(nil)                              // the production window, nil composer
	open := newLoop(nil).WithWindow("00:00", "23:59") // the real clock, whatever hour it is

	// Agreement with the statistics, stated: the run the trigger sees is the run the
	// page's streak logic sees, for every fixture, from the same function.
	agree := true
	var misses []string
	for _, c := range []struct {
		item domain.Item
		want int
	}{{itemA, 5}, {itemB, 3}, {itemD, 5}, {itemE, 4}, {itemF, 2}} {
		sg, err := svc.ItemSignals(ctx, c.item.ID)
		if err != nil {
			return err
		}
		misses = append(misses, fmt.Sprintf("%d", sg.ConsecutiveMisses))
		agree = agree && sg.ConsecutiveMisses == c.want
	}
	fmt.Printf("  stats agree on the run  A/B/D/E/F = %s (want 5/3/5/4/2; a skip is neutral, a completion ends it)  %s\n",
		strings.Join(misses, "/"), verdict(agree))

	// What a proposal is allowed to touch: nothing. Snapshot before the first pass,
	// compare after the last proposal of S4.
	snapshot := func() (string, error) {
		var b strings.Builder
		for _, it := range []domain.Item{itemA, itemB, itemC, itemD, itemE, itemF} {
			cur, err := st.GetItem(ctx, it.ID)
			if err != nil {
				return "", err
			}
			occs, err := st.ListOccurrencesForItem(ctx, it.ID)
			if err != nil {
				return "", err
			}
			fmt.Fprintf(&b, "%s|%s|%s|%v|%d|", cur.ID, cur.Schedule, domain.FormatTime(cur.UpdatedAt), cur.Active, len(occs))
			for _, o := range occs {
				fmt.Fprintf(&b, "%s:%s,", o.ID, o.Status)
			}
			b.WriteString("\n")
		}
		return b.String(), nil
	}
	before, err := snapshot()
	if err != nil {
		return err
	}

	// --- global pause --------------------------------------------------------

	if err := st.SetGlobalPauseUntil(ctx, now.Add(24*time.Hour)); err != nil {
		return err
	}
	// Run's own now decides the pause test, so this pass is at the real clock.
	res, err := open.Run(ctx, now)
	if err != nil {
		return err
	}
	if err := st.ClearGlobalPause(ctx); err != nil {
		return err
	}
	fmt.Printf("  global pause            paused=%v detected=%d sent=%v  %s\n",
		res.Paused, res.Detected, res.Sent, verdict(res.Paused && res.Detected == 0 && !res.Sent && len(rec.sent) == 0))

	// --- S1: detection at any hour, sending only inside the window ----------------

	hours := []struct {
		label string
		at    time.Time
	}{
		{"03:00", daysAgo(2, 3, 0)},
		{"08:59", daysAgo(2, 8, 59)},
		{"20:00", daysAgo(2, 20, 0)},
		{"21:30", daysAgo(2, 21, 30)},
	}
	quietOK := true
	var firstDetected int
	for i, h := range hours {
		r, err := loop.Run(ctx, h.at)
		if err != nil {
			return err
		}
		if i == 0 {
			firstDetected = r.Detected
		}
		quietOK = quietOK && r.Quiet && !r.Sent && len(rec.sent) == 0
	}
	fmt.Printf("  quiet hours             03:00/08:59/20:00/21:30 sent nothing, detected %d at 03:00 (want 4)  %s\n",
		firstDetected, verdict(quietOK && firstDetected == 4))

	latches, err := st.Latches(ctx)
	if err != nil {
		return err
	}
	owedKept := 0
	for _, l := range latches {
		if l.State == store.LatchDetected {
			owedKept++
		}
	}
	fmt.Printf("  held, not dropped       %d latches still detected after four quiet passes  %s\n",
		owedKept, verdict(owedKept == 4))

	// --- S2/S3: the window opens, one per pass, the cap stops the fourth -----------

	type step struct {
		at   time.Time
		want string // title fragment of what must go out, or "" for nothing
	}
	plan := []step{
		{daysAgo(2, 9, 0), itemA.Title},  // the window's start is inclusive; five misses first, oldest key first
		{daysAgo(2, 9, 5), itemD.Title},  // the other five-miss run
		{daysAgo(2, 9, 10), itemC.Title}, // then the repeated snooze
		{daysAgo(2, 9, 15), ""},          // the fourth is B, and the day's three are spent
		{daysAgo(2, 9, 20), ""},
	}
	stepsOK := true
	var sentTitles []string
	cappedSeen := 0
	for _, s := range plan {
		n := len(rec.sent)
		r, err := loop.Run(ctx, s.at)
		if err != nil {
			return err
		}
		cappedSeen += r.Capped
		switch {
		case s.want != "":
			ok := r.Sent && len(rec.sent) == n+1 && strings.Contains(rec.sent[n].Body, s.want)
			stepsOK = stepsOK && ok
			if ok {
				sentTitles = append(sentTitles, r.Trigger)
			}
		default:
			stepsOK = stepsOK && !r.Sent && len(rec.sent) == n
		}
	}
	fmt.Printf("  one per pass, in order  sent %s; two more passes sent nothing  %s\n",
		strings.Join(sentTitles, ", "), verdict(stepsOK))

	count, err := st.ProactiveCount(ctx, daysAgo(2, 9, 0).Format(domain.DateLayout))
	if err != nil {
		return err
	}
	bSent := strings.Contains(strings.Join(bodies(rec), "\n"), itemB.Title)
	fmt.Printf("  the cap                 count=%d sent=%d, the fourth (dormancy) not sent=%v, counted capped %d time (want 1, once per day)  %s\n",
		count, len(rec.sent), !bSent, cappedSeen, verdict(count == 3 && len(rec.sent) == 3 && !bSent && cappedSeen == 1 && pm.capped == 1))

	// What the three proposals say and would do, read before S4 prunes them.
	props, err := st.AllProposals(ctx)
	if err != nil {
		return err
	}
	byItem := map[string]domain.Proposal{}
	for _, p := range props {
		byItem[p.ItemID] = p
	}
	var schedA struct {
		Schedule schedule.Schedule `json:"schedule"`
	}
	_ = json.Unmarshal(byItem[itemA.ID].Changes, &schedA)
	pa, pd, pc := byItem[itemA.ID], byItem[itemD.ID], byItem[itemC.ID]
	altOK := pa.Tool == domain.ProposalUpdateItem && schedA.Schedule.RRule != nil &&
		*schedA.Schedule.RRule == "FREQ=WEEKLY;BYDAY=MO,WE,FR" && pa.Trigger == domain.TriggerMisses &&
		pd.Trigger == domain.TriggerMisses &&
		pc.Trigger == domain.TriggerSnoozes && pc.Summary == "move it to 21:00"
	fmt.Printf("  specific alternatives   A=%q  D=%q  C=%q  %s\n", pa.Summary, pd.Summary, pc.Summary, verdict(altOK))

	// Every message states what a yes would mean; the guard is the exported one.
	statesAlt := true
	for _, p := range []domain.Proposal{pa, pd, pc} {
		var body string
		for _, b := range bodies(rec) {
			if strings.Contains(b, p.ItemTitle) {
				body = b
			}
		}
		_, err := proactive.Accept(body, proactive.Candidate{
			Item: domain.Item{Title: p.ItemTitle},
			Alt:  proactive.Alternative{Summary: p.Summary, Mentions: p.Mentions},
		})
		statesAlt = statesAlt && err == nil
	}
	fmt.Printf("  message states the offer  every template names its item and its alternative  %s\n", verdict(statesAlt))

	// The recorded row a reply is recognised by.
	convs, err := st.ListRecentConversations(ctx, 200)
	if err != nil {
		return err
	}
	refs := 0
	for _, c := range convs {
		if c.ContextRef != nil && (*c.ContextRef == store.ProposalRef(pa.ID) || *c.ContextRef == store.ProposalRef(pd.ID) || *c.ContextRef == store.ProposalRef(pc.ID)) &&
			c.Role == domain.RoleAssistant {
			refs++
		}
	}
	fmt.Printf("  context_ref recorded    %d assistant rows carry proposal:{id}  %s\n", refs, verdict(refs == 3))

	// --- S4: the next day, the held one goes -------------------------------------

	n := len(rec.sent)
	r, err := loop.Run(ctx, daysAgo(1, 10, 0))
	if err != nil {
		return err
	}
	again, err := loop.Run(ctx, daysAgo(1, 10, 5))
	if err != nil {
		return err
	}
	fmt.Printf("  the next day            held dormancy goes out (%v), %d total, then nothing owed (%v)  %s\n",
		r.Sent && r.Trigger == domain.TriggerDormant, len(rec.sent), !again.Sent && again.Detected == 0,
		verdict(r.Sent && r.Trigger == domain.TriggerDormant && len(rec.sent) == n+1 && !again.Sent && again.Detected == 0 &&
			strings.Contains(rec.sent[n].Body, itemB.Title)))

	// Once per episode: four episodes, four triggers, over seven passes that could
	// have fired them again.
	fmt.Printf("  once per episode        triggers misses=%d snooze=%d dormancy=%d (want 2/1/1), sent total %d (want 4), fallbacks %d (no composer)  %s\n",
		pm.get(pm.triggers, domain.TriggerMisses), pm.get(pm.triggers, domain.TriggerSnoozes),
		pm.get(pm.triggers, domain.TriggerDormant), pm.total(pm.sent), pm.fallbacks,
		verdict(pm.get(pm.triggers, domain.TriggerMisses) == 2 && pm.get(pm.triggers, domain.TriggerSnoozes) == 1 &&
			pm.get(pm.triggers, domain.TriggerDormant) == 1 && pm.total(pm.sent) == 4 && pm.fallbacks == 4))

	after, err := snapshot()
	if err != nil {
		return err
	}
	fmt.Printf("  proposals change nothing  items and occurrences identical after four proposals; accepted=%d  %s\n",
		pm.total(pm.accepted), verdict(before == after && pm.total(pm.accepted) == 0))

	// --- S6: what resets a latch -----------------------------------------------

	// A completion ends A's episode; the pass after it clears the latch.
	completion, err := fireOccurrence(ctx, st, itemA, now.Add(-8*time.Hour), nil)
	if err != nil {
		return err
	}
	if _, err := st.ResolveOccurrence(ctx, completion.ID, done, nil, domain.ResolvedByWeb, now.Add(-8*time.Hour+time.Minute)); err != nil {
		return err
	}
	if _, err := open.Run(ctx, now); err != nil {
		return err
	}
	latches, err = st.Latches(ctx)
	if err != nil {
		return err
	}
	_, aStill := latches["item:"+itemA.ID]
	_, dStill := latches["item:"+itemD.ID]
	fmt.Printf("  a completion resets     A's latch gone=%v, D's (no completion) kept=%v  %s\n", !aStill, dStill, verdict(!aStill && dStill))

	// Five more misses after it are a new episode, which is asked about again.
	for i := 5; i >= 1; i-- {
		when := now.Add(-time.Duration(i) * time.Hour)
		occ, err := fireOccurrence(ctx, st, itemA, when, nil)
		if err != nil {
			return err
		}
		if _, err := st.ResolveOccurrence(ctx, occ.ID, miss, nil, domain.ResolvedByReconciler, when.Add(time.Minute)); err != nil {
			return err
		}
	}
	// This pass is at the real clock and sends: it is the proposal the replies below
	// are driven against.
	nBefore := len(rec.sent)
	r, err = open.Run(ctx, time.Now())
	if err != nil {
		return err
	}
	fmt.Printf("  a new episode           detected=%d sent=%v trigger=%s  %s\n", r.Detected, r.Sent, r.Trigger,
		verdict(r.Detected == 1 && r.Sent && r.Trigger == domain.TriggerMisses && len(rec.sent) == nBefore+1))

	// --- replies: "yes" ----------------------------------------------------------

	mat := materializer.New(log.With("component", "materializer"), st, fallback)
	tools := agent.New(st, mat, table, fallback, pm)
	newLadder := func(srvURL string) (*conversation.Ladder, *fakeSender) {
		routing := &model.Routing{Tasks: map[model.Task]model.TaskRouting{
			model.TaskCRUD: {Tiers: []model.Tier{{Model: "naviseed-proposal-reply", BaseURL: srvURL, TimeoutSeconds: 5}}},
		}}
		client := model.New(log.With("component", "model-proposal"), routing, "", st, pm.Metrics)
		sender := &fakeSender{}
		return conversation.New(client, tools, routing, st, table, "config/persona.md.does-not-exist", fallback, sender), sender
	}
	updateCalls := func() (int, error) {
		rows, err := st.ListRecentConversations(ctx, 5000)
		if err != nil {
			return 0, err
		}
		k := 0
		for _, c := range rows {
			if c.ToolCalls != nil && strings.Contains(*c.ToolCalls, "update_item") {
				k++
			}
		}
		return k, nil
	}

	open1, err := st.OpenProposals(ctx, time.Now())
	if err != nil {
		return err
	}
	var live domain.Proposal
	for _, p := range open1 {
		if p.ItemID == itemA.ID {
			live = p
		}
	}
	args := fmt.Sprintf(`{"item_id":%q,"changes":%s}`, live.ItemID, live.Changes)
	var captured []wireRequest
	srv := capturingToolCallServer("update_item", args, &captured)
	defer srv.Close()
	ladder, sender := newLadder(srv.URL)
	callsBefore, err := updateCalls()
	if err != nil {
		return err
	}
	if err := ladder.Handle(ctx, transport.IncomingMessage{SenderID: "111", Text: "yes, do it", Transport: telegram.Name}); err != nil {
		return err
	}
	prompt := ""
	inHistory := false
	if len(captured) > 0 {
		prompt = captured[0].systemContent()
		for _, msg := range captured[0].Messages {
			if msg.Role == "assistant" && strings.Contains(msg.Content, itemA.Title) {
				inHistory = true
			}
		}
	}
	inPrompt := strings.Contains(prompt, "Context ref:  "+store.ProposalRef(live.ID)) &&
		strings.Contains(prompt, "on yes:") && strings.Contains(prompt, live.ItemID)
	fmt.Printf("  recognised by context   block in prompt=%v, the offer in history=%v  %s\n", inPrompt, inHistory, verdict(inPrompt && inHistory))

	gotA, err := st.GetItem(ctx, itemA.ID)
	if err != nil {
		return err
	}
	parsedA, _ := schedule.Parse(gotA.Schedule)
	callsAfter, err := updateCalls()
	if err != nil {
		return err
	}
	closed, err := st.OpenProposals(ctx, time.Now())
	if err != nil {
		return err
	}
	stillOpen := false
	for _, p := range closed {
		if p.ID == live.ID {
			stillOpen = true
		}
	}
	yesOK := callsAfter-callsBefore == 1 && parsedA.RRule != nil && *parsedA.RRule == "FREQ=WEEKLY;BYDAY=MO,WE,FR" &&
		pm.get(pm.accepted, domain.TriggerMisses) == 1 && !stillOpen
	fmt.Printf("  \"yes\" applies it       update_item calls +%d, schedule now %q, accepted=%d, proposal closed=%v  %s\n",
		callsAfter-callsBefore, deref(parsedA.RRule), pm.get(pm.accepted, domain.TriggerMisses), !stillOpen, verdict(yesOK))
	_ = sender

	// --- rearm, composed, and a reply that is not a yes ---------------------------

	// Time passing is the third thing that ends an episode. D's latch is made to
	// have been asked 31 days ago - through the store's own latch surface - and D's
	// condition still holds, so the next pass asks again. This pass has a composer,
	// scripted to word the offer, so the model path and its guard are exercised too.
	var composeSrv *cwModel
	composeSrv = newCWModel(func(n int) (int, string) {
		p := composeSrv.prompt(n)
		title, alt := lineAfter(p, "Reminder: "), lineAfter(p, "Proposal: ")
		return 200, fmt.Sprintf("Quick thought on %s: want me to %s? Or we can leave it be.", title, alt)
	})
	defer composeSrv.srv.Close()
	routing := &model.Routing{Tasks: map[model.Task]model.TaskRouting{
		model.TaskProposal: {Tiers: []model.Tier{{Model: "naviseed-proposal", BaseURL: composeSrv.srv.URL, TimeoutSeconds: 5}}},
	}}
	composer := proactive.NewModelComposer(model.New(log.With("component", "model-compose"), routing, "", st, pm.Metrics),
		routing, "config/persona.md.does-not-exist")
	composed := newLoop(composer).WithWindow("00:00", "23:59")

	old := time.Now().Add(-31 * 24 * time.Hour)
	if err := st.UpdateLatches(ctx, time.Now(), func(m map[string]store.Latch) bool {
		l := m["item:"+itemD.ID]
		l.AskedAt = &old
		m["item:"+itemD.ID] = l
		return true
	}); err != nil {
		return err
	}
	fbBefore, sentBefore := pm.fallbacks, len(rec.sent)
	r, err = composed.Run(ctx, time.Now())
	if err != nil {
		return err
	}
	composedBody := ""
	if len(rec.sent) > sentBefore {
		composedBody = rec.sent[len(rec.sent)-1].Body
	}
	fmt.Printf("  time re-arms it         asked 31 days ago, still true: detected=%d sent=%v composed=%v (fallbacks +%d)  %s\n",
		r.Detected, r.Sent, strings.HasPrefix(composedBody, "Quick thought"), pm.fallbacks-fbBefore,
		verdict(r.Detected == 1 && r.Sent && !r.Fallback && strings.HasPrefix(composedBody, "Quick thought") && pm.fallbacks == fbBefore))

	// The guard: a completion that does not state the alternative, and a broken
	// provider, both become the template and never a wrong offer.
	candD := proactive.Candidate{Trigger: domain.TriggerMisses, Item: domain.Item{Title: "Evening walk"},
		Alt: proactive.Alternative{Tool: domain.ProposalUpdateItem, Summary: "move it to 19:30", Mentions: []string{"19:30|7:30pm|7:30 pm"}}}
	_, e1 := proactive.Accept("Evening walk is not working, want to change it?", candD)
	_, e2 := proactive.Accept("Want to move Evening walk to 7:30pm instead? Or leave it.", candD)
	_, e3 := proactive.Accept("want to move it to 19:30?", candD)
	badSrv := newCWModel(func(int) (int, string) { return 500, "" })
	defer badSrv.srv.Close()
	badRouting := &model.Routing{Tasks: map[model.Task]model.TaskRouting{
		model.TaskProposal: {Tiers: []model.Tier{{Model: "naviseed-proposal", BaseURL: badSrv.srv.URL, TimeoutSeconds: 5}}},
	}}
	_, e4 := proactive.NewModelComposer(model.New(log.With("component", "model-bad"), badRouting, "", st, pm.Metrics), badRouting, "x").
		Compose(ctx, candD)
	noTiers := &model.Routing{Tasks: map[model.Task]model.TaskRouting{}}
	_, e5 := proactive.NewModelComposer(model.New(log.With("component", "model-none"), noTiers, "", st, pm.Metrics), noTiers, "x").
		Compose(ctx, candD)
	fmt.Printf("  the guard               silent on the offer refused=%v, 12-hour spelling ok=%v, no item name refused=%v, 500 refused=%v, no tiers refused=%v  %s\n",
		e1 != nil, e2 == nil, e3 != nil, e4 != nil, e5 != nil, verdict(e1 != nil && e2 == nil && e3 != nil && e4 != nil && e5 != nil))

	// Something unrelated, against D's open proposal: nothing is applied.
	openD, err := st.OpenProposals(ctx, time.Now())
	if err != nil {
		return err
	}
	var liveD domain.Proposal
	for _, p := range openD {
		if p.ItemID == itemD.ID {
			liveD = p
		}
	}
	scheduleDBefore := string(mustItem(ctx, st, itemD.ID).Schedule)
	var capturedD []wireRequest
	srvD := capturingToolCallServer("list_items", `{}`, &capturedD)
	defer srvD.Close()
	ladderD, _ := newLadder(srvD.URL)
	callsBefore, _ = updateCalls()
	if err := ladderD.Handle(ctx, transport.IncomingMessage{SenderID: "111", Text: "what do I have on today?", Transport: telegram.Name}); err != nil {
		return err
	}
	callsAfter, _ = updateCalls()
	shown := len(capturedD) > 0 && strings.Contains(capturedD[0].systemContent(), "Context ref:  "+store.ProposalRef(liveD.ID))
	allP, err := st.AllProposals(ctx)
	if err != nil {
		return err
	}
	statusD := domain.ProposalStatus("")
	for _, p := range allP {
		if p.ID == liveD.ID {
			statusD = p.Status
		}
	}
	// A turn later the offer is spent: a model that heard a stray "yes" now has no
	// block to point at, however it is worded.
	var capturedD2 []wireRequest
	srvD2 := capturingToolCallServer("list_items", `{}`, &capturedD2)
	defer srvD2.Close()
	ladderD2, _ := newLadder(srvD2.URL)
	if err := ladderD2.Handle(ctx, transport.IncomingMessage{SenderID: "111", Text: "yes", Transport: telegram.Name}); err != nil {
		return err
	}
	gone := len(capturedD2) > 0 && !strings.Contains(capturedD2[0].systemContent(), "Context ref:  "+store.ProposalRef(liveD.ID))
	unrelatedOK := shown && callsAfter == callsBefore && string(mustItem(ctx, st, itemD.ID).Schedule) == scheduleDBefore &&
		statusD == domain.ProposalPassed && pm.get(pm.accepted, domain.TriggerMisses) == 1 && gone
	fmt.Printf("  unrelated changes none  offer shown=%v, update_item calls +%d, schedule unchanged, status=%s, accepted still %d, a later \"yes\" finds no block=%v  %s\n",
		shown, callsAfter-callsBefore, statusD, pm.get(pm.accepted, domain.TriggerMisses), gone, verdict(unrelatedOK))

	// --- propose_change, the agent's own -------------------------------------------

	empty, perr := tools.Call(ctx, "propose_change",
		json.RawMessage(fmt.Sprintf(`{"item_id":%q,"proposal":{},"rationale":"x"}`, itemF.ID)))
	_ = empty
	proposeArgs := fmt.Sprintf(`{"item_id":%q,"proposal":{"schedule":{"kind":"fixed","rrule":"FREQ=WEEKLY;BYDAY=MO,TH","at":"18:00"}},"rationale":"Two days a week may suit it better."}`, itemF.ID)
	scheduleFBefore := string(mustItem(ctx, st, itemF.ID).Schedule)
	srvP := toolCallServer("propose_change", proposeArgs)
	defer srvP.Close()
	ladderP, senderP := newLadder(srvP.URL)
	if err := ladderP.Handle(ctx, transport.IncomingMessage{SenderID: "111", Text: "what would you change about the run reset one?", Transport: telegram.Name}); err != nil {
		return err
	}
	openAfter, err := st.OpenProposals(ctx, time.Now())
	if err != nil {
		return err
	}
	var agentProp domain.Proposal
	for _, p := range openAfter {
		if p.ItemID == itemF.ID {
			agentProp = p
		}
	}
	refRows, _ := st.ListRecentConversations(ctx, 50)
	tagged := false
	for _, c := range refRows {
		if c.Role == domain.RoleAssistant && c.ContextRef != nil && *c.ContextRef == store.ProposalRef(agentProp.ID) &&
			strings.Contains(c.Content, "Nothing changes unless you say yes") {
			tagged = true
		}
	}
	proposeOK := perr != nil && agentProp.Trigger == domain.TriggerAgent &&
		string(mustItem(ctx, st, itemF.ID).Schedule) == scheduleFBefore && tagged &&
		strings.Contains(senderP.last(), "Nothing changes unless you say yes")
	fmt.Printf("  propose_change          empty rejected=%v, recorded as agent=%v, item untouched, reply tagged=%v  %s\n",
		perr != nil, agentProp.Trigger == domain.TriggerAgent, tagged, verdict(proposeOK))

	// ...and a yes to it goes through the ordinary path once.
	yesArgs := fmt.Sprintf(`{"item_id":%q,"changes":%s}`, itemF.ID, agentProp.Changes)
	srvY := toolCallServer("update_item", yesArgs)
	defer srvY.Close()
	ladderY, _ := newLadder(srvY.URL)
	callsBefore, _ = updateCalls()
	if err := ladderY.Handle(ctx, transport.IncomingMessage{SenderID: "111", Text: "yes please", Transport: telegram.Name}); err != nil {
		return err
	}
	callsAfter, _ = updateCalls()
	parsedF, _ := schedule.Parse(mustItem(ctx, st, itemF.ID).Schedule)
	fmt.Printf("  yes to the agent's own  update_item calls +%d, schedule %q, accepted{agent}=%d  %s\n",
		callsAfter-callsBefore, deref(parsedF.RRule), pm.get(pm.accepted, domain.TriggerAgent),
		verdict(callsAfter-callsBefore == 1 && parsedF.RRule != nil && *parsedF.RRule == "FREQ=WEEKLY;BYDAY=MO,TH" &&
			pm.get(pm.accepted, domain.TriggerAgent) == 1))

	// --- the day's budget, and what the dormancy overlap left behind -----------------

	// The copywriter's dormancy branch no longer asks anything: the question is the
	// proactive loop's, and a reminder asking it too would put it to the person
	// twice, once with no record of having been asked.
	dormancy := strings.ToLower(copywriter.StrategyDormancy.Instruction())
	asks := strings.Contains(dormancy, "rescheduled") || strings.Contains(dormancy, "dropped") ||
		strings.Contains(dormancy, "ask one")
	fmt.Printf("  one owner for the question  the dormancy reminder asks nothing (reschedule/shrink/drop absent)=%v  %s\n",
		!asks, verdict(!asks))

	todayCount, err := st.ProactiveCount(ctx, time.Now().In(loc).Format(domain.DateLayout))
	if err != nil {
		return err
	}
	fmt.Printf("  today's budget          %d of %d spent by the two real-clock proposals  %s\n",
		todayCount, proactive.DailyCap, verdict(todayCount == 2 && todayCount <= proactive.DailyCap))
	return nil
}

func bodies(r *recordingTransport) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, len(r.sent))
	for i, m := range r.sent {
		out[i] = m.Body
	}
	return out
}

func mustItem(ctx context.Context, st *store.Store, id string) domain.Item {
	it, err := st.GetItem(ctx, id)
	if err != nil {
		return domain.Item{}
	}
	return it
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// lineAfter returns the rest of the first line of s that starts with prefix.
func lineAfter(s, prefix string) string {
	for _, line := range strings.Split(s, "\n") {
		if strings.HasPrefix(line, prefix) {
			return strings.TrimPrefix(line, prefix)
		}
	}
	return ""
}

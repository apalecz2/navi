package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/aidenpaleczny/navi/internal/agent"
	"github.com/aidenpaleczny/navi/internal/config"
	"github.com/aidenpaleczny/navi/internal/conversation"
	"github.com/aidenpaleczny/navi/internal/defaults"
	"github.com/aidenpaleczny/navi/internal/digest"
	"github.com/aidenpaleczny/navi/internal/domain"
	"github.com/aidenpaleczny/navi/internal/materializer"
	"github.com/aidenpaleczny/navi/internal/metrics"
	"github.com/aidenpaleczny/navi/internal/model"
	"github.com/aidenpaleczny/navi/internal/schedule"
	"github.com/aidenpaleczny/navi/internal/stats"
	"github.com/aidenpaleczny/navi/internal/store"
	"github.com/aidenpaleczny/navi/internal/transport"
	"github.com/aidenpaleczny/navi/internal/transport/telegram"
	"github.com/aidenpaleczny/navi/internal/web"
)

// dgMetrics reads the digest's counters back as numbers. It implements
// digest.Metrics over a real registry, so what is counted is what production counts.
type dgMetrics struct {
	*metrics.Metrics
	mu        sync.Mutex
	sent      map[string]int
	fallbacks int
	rejected  int
}

func newDGMetrics() *dgMetrics {
	return &dgMetrics{Metrics: metrics.New(), sent: map[string]int{}}
}

func (m *dgMetrics) IncDigestSent(form string) {
	m.mu.Lock()
	m.sent[form]++
	m.mu.Unlock()
	m.Metrics.IncDigestSent(form)
}

func (m *dgMetrics) IncDigestFallback() {
	m.mu.Lock()
	m.fallbacks++
	m.mu.Unlock()
	m.Metrics.IncDigestFallback()
}

func (m *dgMetrics) IncDigestRejected() {
	m.mu.Lock()
	m.rejected++
	m.mu.Unlock()
	m.Metrics.IncDigestRejected()
}

func (m *dgMetrics) snap() (sentFull, sentShort, fallbacks, rejected int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.sent["full"], m.sent["short"], m.fallbacks, m.rejected
}

// dgComposer is a scripted observation writer.
type dgComposer struct {
	mu    sync.Mutex
	calls int
	facts []string
	reply func(n int) (string, error)
}

func (c *dgComposer) Observe(_ context.Context, facts string) (string, error) {
	c.mu.Lock()
	c.calls++
	n := c.calls
	c.facts = append(c.facts, facts)
	c.mu.Unlock()
	return c.reply(n)
}

func (c *dgComposer) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls
}

// dgSummary serves a hand-built Summary, so a loop scenario chooses its form
// instead of depending on whatever the shared database holds.
type dgSummary struct{ sum stats.Summary }

func (s dgSummary) Summary(context.Context, stats.Query) (stats.Summary, error) { return s.sum, nil }

func fp(f float64) *float64 { return &f }

// dgFull is a week with enough settled chains to read: 12 settled, one over the
// threshold's 10, with a skip, two items, a goal, and an item with no chains.
func dgFull() stats.Summary {
	return stats.Summary{
		Range: "week", Timezone: "America/Toronto", To: "2026-01-04",
		Totals:           stats.Totals{Chains: 13, Completed: 10, Skipped: 1, Missed: 2, Settled: 12},
		CompletionRate:   fp(10.0 / 12.0),
		MedianLagMinutes: fp(10), LagSamples: 9,
		Items: []stats.ItemStats{
			{ItemID: "a", Title: "Walk", Totals: stats.Totals{Chains: 7, Completed: 6, Missed: 1, Settled: 7},
				CompletionRate: fp(6.0 / 7.0), CurrentStreak: 3, LongestStreak: 12, MedianLagMinutes: fp(10), LagSamples: 6,
				RecentSkipNotes: []string{}},
			{ItemID: "b", Title: "Read", Totals: stats.Totals{Chains: 6, Completed: 4, Skipped: 1, Missed: 1, Settled: 5},
				CompletionRate: fp(0.8), CurrentStreak: 4, LongestStreak: 9, RecentSkipNotes: []string{"on vacation"}},
			{ItemID: "c", Title: "Floss", CurrentStreak: 2, LongestStreak: 2, RecentSkipNotes: []string{}},
		},
		Goals: []stats.GoalStats{{GoalID: "g", Title: "Gym", Status: "active",
			Completed: ptr(2), Target: ptr(3), Velocity: fp(2.0)}},
	}
}

// dgThin is a week with something in it and too little to read: 4 settled.
func dgThin() stats.Summary {
	return stats.Summary{
		Range: "week", Timezone: "America/Toronto", To: "2026-01-04",
		Totals:         stats.Totals{Chains: 6, Completed: 3, Skipped: 1, Missed: 1, Open: 1, Settled: 4},
		CompletionRate: fp(0.75),
		Items:          []stats.ItemStats{{ItemID: "a", Title: "Walk", Totals: stats.Totals{Chains: 6}}},
	}
}

// dgEmpty is a week in which nothing was scheduled.
func dgEmpty() stats.Summary {
	return stats.Summary{Range: "week", Timezone: "America/Toronto", To: "2026-01-04", Items: []stats.ItemStats{}}
}

// reportDigest drives the weekly digest: the number check as a table, the form
// choice against the statistics page's own threshold, the loop through compose,
// send and its failure modes at constructed Sundays, the figures against the
// real stats.Service and get_stats, the reply path through a real Ladder, and the
// off switch.
//
// Time is handled as the proactive section handles it. The loop takes an explicit
// now, so every phase is checked at an instant constructed in the PAST (a Sunday
// at least a day ago): a future-dated conversations row would sit at the top of
// every later history read. The one pass that must be at the real clock, because a
// reply window is 24 hours from the send, uses WithWeekday and a send time of this
// very minute.
func reportDigest(ctx context.Context, st *store.Store, table *defaults.Table, routingPath, tz string, fallback *time.Location, log *slog.Logger) error {
	fmt.Println("\ndigest  the weekly digest: figures, the number check, thin weeks, the loop, replies, the off switch")

	zones, err := schedule.LoadZones(ctx, st, fallback)
	if err != nil {
		return err
	}
	loc := zones.Local()
	if err := st.ClearGlobalPause(ctx); err != nil {
		return err
	}
	if err := st.ClearDigestState(ctx); err != nil {
		return err
	}

	// ---- 1. the number check ------------------------------------------------------

	full := digest.Settle(dgFull())
	fmt.Println("  the figures a model is given (and the template sends):")
	for _, line := range strings.Split(full.Facts, "\n") {
		fmt.Println("    |", line)
	}

	type numCase struct {
		name string
		obs  string
		ok   bool
	}
	cases := []numCase{
		{"no numbers at all", "Walk has been the steady one this week.", true},
		{"a figure, copied", "Read is at 4 in a row.", true},
		{"a figure as a word", "Read is on four in a row, which is a good run.", true},
		{"figures that are not given, as words", "Five of the six on Read settled well.", false},
		{"a percentage, copied", "Walk finished 86% of what settled.", true},
		{"a percentage as words", "Eighty six percent of Walk settled well.", true}, // tens and unit merge to 86
		{"same percentage spelled out", "86 percent of Walk's reminders got done.", true},
		{"a rounding of a percentage", "Roughly 85% of Walk got done.", false},
		{"more digits than the figure", "Overall 83.3% got done.", false},
		{"a trailing zero is not rounding", "Overall 83.0% got done.", true},
		{"a decimal, copied", "Gym is moving at 2.0 a week.", true},
		{"a decimal, equal value", "Gym is moving at 2 a week.", true},
		{"a number not given", "Walk hit 14 in a row.", false},
		{"a number not given, as a word", "Walk hit fourteen in a row.", false},
		{"a compound word", "That is twenty-one sessions.", false},
		{"two words for a tens number", "That is twenty one sessions.", false},
		{"a hundred", "About a hundred percent on one thing.", false},
		{"a dozen, where the figures hold a 12", "Walk has a dozen in a row.", true}, // matching is by value, not by what the number is attached to
		{"half", "About half of them got done.", false},
		{"a percentage needs a percentage", "Median lag was 10 percent of the day.", false},
		{"a plain number may match a percentage's value", "Walk got 86 of them.", true},
		{"one as a pronoun", "One thing stood out: no one is behind.", true},
		{"one as a count, present", "Read had one skip, and it was explained.", true},
		{"an ordinal", "That is the 3rd week of it.", true},
		{"a time of day", "Most got done around 20:30.", false},
		{"digits in a title are fine", "Gym is at 2 of 3.", true},
	}
	bad := 0
	for _, c := range cases {
		err := digest.CheckNumbers(full.Facts, c.obs)
		if (err == nil) != c.ok {
			bad++
			fmt.Printf("    case %q: %q  got err=%v, want ok=%v  %s\n", c.name, c.obs, err, c.ok, verdict(false))
		}
	}
	fmt.Printf("  the check               %d cases (words, percentages, rounding, pronouns), every one as expected  %s\n",
		len(cases), verdict(bad == 0))

	// The words that are not cardinals, against figures that hold none of their
	// values (2, 3, 12, 50%, and no 1), so each has to be refused.
	const small = "Over the last 7 days: 80% done (4 of 5 completed)."
	smallCases := []numCase{
		{"twice", "Read happened twice as often.", false},
		{"thrice", "Walk was done thrice.", false},
		{"doubled", "Read doubled.", false},
		{"a dozen", "A dozen of them got done.", false},
		{"half", "About half were done.", false},
		{"one as a count, absent", "There was one miss.", false},
		{"one in a row, absent", "That is one in a row.", false},
		{"one as a pronoun is not a count", "One thing stood out, and no one minded.", true},
		{"given, as words", "Four of five got done, which is 80%.", true},
		{"a percentage spelled out", "Eighty percent got done.", true},
	}
	smallBad := 0
	for _, c := range smallCases {
		if err := digest.CheckNumbers(small, c.obs); (err == nil) != c.ok {
			smallBad++
			fmt.Printf("    case %q: %q  got err=%v, want ok=%v  %s\n", c.name, c.obs, err, c.ok, verdict(false))
		}
	}
	fmt.Printf("  the other words         %d cases (twice, thrice, doubled, a dozen, half, one) against figures that hold none of them  %s\n",
		len(smallCases), verdict(smallBad == 0))

	// What the template says is checked by the same function: no sentence in it
	// but the figures' own contains a number.
	tmpl := full.Message("")
	fmt.Printf("  the template            the whole message passes its own check, and says the percentage the page does (%s)  %s\n",
		stats.Percent(10.0/12.0), verdict(digest.CheckNumbers(full.Facts, tmpl) == nil && strings.Contains(tmpl, "83% done")))

	// ---- 2. which form a week earns ---------------------------------------------

	nine, ten := dgFull(), dgFull()
	nine.Totals.Settled, nine.Totals.Completed, nine.Totals.Missed = 9, 8, 1
	ten.Totals.Settled, ten.Totals.Completed, ten.Totals.Missed = 10, 8, 2
	thin := digest.Settle(dgThin())
	fmt.Printf("  form by settled chains  0 chains=%s, 9 settled=%s, 10 settled=%s (threshold stats.MinSettled=%d, the page's %d)  %s\n",
		digest.Settle(dgEmpty()).Form, digest.Settle(nine).Form, digest.Settle(ten).Form, stats.MinSettled, web.MinSettled,
		verdict(digest.Settle(dgEmpty()).Form == digest.FormNone && digest.Settle(nine).Form == digest.FormShort &&
			digest.Settle(ten).Form == digest.FormFull && stats.MinSettled == web.MinSettled && web.MinSettled == 10))
	shortMsg := thin.Message("")
	fmt.Printf("  the short form          counts only: %q, no percent sign, says it is thin  %s\n",
		strings.SplitN(shortMsg, "\n", 2)[0],
		verdict(!strings.Contains(shortMsg, "%") && strings.Contains(shortMsg, "too little to call a pattern") &&
			strings.Contains(shortMsg, "3 of 4 completed; 1 skipped, 1 still open")))

	// ---- 3. the loop, at constructed Sundays ------------------------------------

	local := time.Now().In(loc)
	today := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, loc)
	sunday := today.AddDate(0, 0, -int(local.Weekday())) // this week's Sunday, or today if it is one
	if !sunday.Before(today) {
		sunday = sunday.AddDate(0, 0, -7) // strictly in the past
	}
	nthSunday := func(k int) time.Time { return sunday.AddDate(0, 0, -7*k) }
	at := func(day time.Time, h, m int) time.Time {
		return time.Date(day.Year(), day.Month(), day.Day(), h, m, 0, 0, loc)
	}
	dateOf := func(day time.Time) string { return day.Format(domain.DateLayout) }

	dm := newDGMetrics()
	rec := &recordingTransport{}
	newLoop := func(sum digest.Summarizer, c digest.Composer, n digest.Notifier) *digest.Digest {
		return digest.New(log.With("loop", "digest-seed"), st, sum, n, dm, c, "19:00", fallback)
	}
	digestRow := func(date string) (domain.Conversation, bool, error) {
		rows, err := st.ListRecentConversations(ctx, 5000)
		if err != nil {
			return domain.Conversation{}, false, err
		}
		for _, c := range rows {
			if c.Role == domain.RoleAssistant && c.ContextRef != nil && *c.ContextRef == store.DigestRef(date) {
				return c, true, nil
			}
		}
		return domain.Conversation{}, false, nil
	}
	capDate := func(day time.Time) (int, error) { return st.ProactiveCount(ctx, dateOf(day)) }

	// 3a. The day and the clock. Nothing before the compose window, and nothing on
	// a Monday at DIGEST_AT.
	good := &dgComposer{reply: func(int) (string, error) {
		return "Read hit 4 in a row, its best run, and Walk stayed steady.", nil
	}}
	s1 := nthSunday(1)
	lp := newLoop(dgSummary{dgFull()}, good, rec)
	rEarly, err := lp.Run(ctx, at(s1, 18, 20))
	if err != nil {
		return err
	}
	rMon, err := lp.Run(ctx, at(s1.AddDate(0, 0, 1), 19, 0))
	if err != nil {
		return err
	}
	rSat, err := lp.Run(ctx, at(s1.AddDate(0, 0, -1), 19, 0))
	if err != nil {
		return err
	}
	fmt.Printf("  the day and the clock   18:20 Sunday idle=%v, 19:00 Monday idle=%v, 19:00 Saturday idle=%v, nothing composed or sent  %s\n",
		rEarly.Phase == digest.PhaseIdle, rMon.Phase == digest.PhaseIdle, rSat.Phase == digest.PhaseIdle,
		verdict(rEarly.Phase == "" && rMon.Phase == "" && rSat.Phase == "" && good.count() == 0 && len(rec.sent) == 0))

	// 3b. Composed ahead: the model is asked in the compose window and not at the
	// send, the send is the staged text, and a second pass changes nothing.
	capBefore, err := capDate(s1)
	if err != nil {
		return err
	}
	rc, err := lp.Run(ctx, at(s1, 18, 45))
	if err != nil {
		return err
	}
	rc2, err := lp.Run(ctx, at(s1, 18, 50))
	if err != nil {
		return err
	}
	pDate, pText, pOK, err := st.DigestPending(ctx)
	if err != nil {
		return err
	}
	fmt.Printf("  composed ahead          18:45 phase=%s form=%s, staged for %s=%v, asked once across two passes (calls=%d), nothing sent yet=%v  %s\n",
		rc.Phase, rc.Form, dateOf(s1), pOK && pDate == dateOf(s1), good.count(), len(rec.sent) == 0,
		verdict(rc.Phase == digest.PhaseCompose && rc.Form == digest.FormFull && !rc.Fallback && pOK && pDate == dateOf(s1) &&
			good.count() == 1 && rc2.Phase == digest.PhaseCompose && len(rec.sent) == 0))

	rs, err := lp.Run(ctx, at(s1, 19, 0))
	if err != nil {
		return err
	}
	body := ""
	if len(rec.sent) > 0 {
		body = rec.sent[len(rec.sent)-1].Body
	}
	row, rowOK, err := digestRow(dateOf(s1))
	if err != nil {
		return err
	}
	lastDate, latched, err := st.LastDigestDate(ctx)
	if err != nil {
		return err
	}
	_, _, stillPending, err := st.DigestPending(ctx)
	if err != nil {
		return err
	}
	staged := strings.SplitN(pText, "\n", 2)
	sendOK := rs.Phase == digest.PhaseSend && rs.Sent && len(rec.sent) == 1 && len(staged) == 2 && body == staged[1] &&
		good.count() == 1 && strings.Contains(body, "Read hit 4 in a row") && strings.Contains(body, full.Facts) &&
		rowOK && row.Content == body && latched && lastDate == dateOf(s1) && !stillPending
	fmt.Printf("  sent at DIGEST_AT       sent=%v, the staged text verbatim=%v, still one model call=%v, conversations row context_ref digest:%s=%v, latched=%v, slot cleared=%v  %s\n",
		rs.Sent, len(staged) == 2 && body == staged[1], good.count() == 1, dateOf(s1), rowOK, latched, !stillPending, verdict(sendOK))

	again, err := lp.Run(ctx, at(s1, 19, 5))
	if err != nil {
		return err
	}
	fmt.Printf("  latched                 a pass five minutes later sends nothing (sent=%v, messages=%d)  %s\n",
		again.Sent, len(rec.sent), verdict(!again.Sent && len(rec.sent) == 1))

	// 3c. The cap: scheduled, so it neither counts nor is held by it.
	capAfter, err := capDate(s1)
	if err != nil {
		return err
	}
	fmt.Printf("  not against the cap     kv.proactive_count for %s before=%d after=%d (cap %d), and it went out at 19:00, outside the proactive window  %s\n",
		dateOf(s1), capBefore, capAfter, 3, verdict(capBefore == capAfter))

	// 3d. The staged window missed (a restart across it, a first boot at 22:30):
	// the template, synchronously, with no model call.
	s2 := nthSunday(2)
	late := &dgComposer{reply: func(int) (string, error) { return "should never be asked", nil }}
	_, _, fb0, rj0 := dm.snap()
	rl, err := newLoop(dgSummary{dgFull()}, late, rec).Run(ctx, at(s2, 22, 30))
	if err != nil {
		return err
	}
	_, _, fb1, rj1 := dm.snap()
	lateBody := rec.sent[len(rec.sent)-1].Body
	fmt.Printf("  compose window missed   22:30 sent=%v fallback=%v, model asked %d times, body is the template=%v, counted fallback +%d  %s\n",
		rl.Sent, rl.Fallback, late.count(), lateBody == full.Message(""), fb1-fb0,
		verdict(rl.Sent && rl.Fallback && late.count() == 0 && lateBody == full.Message("") && fb1-fb0 == 1 && rj1 == rj0))

	// 3e. An invented number: refused, the template goes out, it is counted, and
	// the model is not asked again on another tier.
	s3 := nthSunday(3)
	liar := &dgComposer{reply: func(int) (string, error) {
		return "Walk hit 14 in a row this week, a new best.", nil
	}}
	_, _, fb0, rj0 = dm.snap()
	rl, err = newLoop(dgSummary{dgFull()}, liar, rec).Run(ctx, at(s3, 18, 40))
	if err != nil {
		return err
	}
	rs, err = newLoop(dgSummary{dgFull()}, liar, rec).Run(ctx, at(s3, 19, 0))
	if err != nil {
		return err
	}
	liarBody := rec.sent[len(rec.sent)-1].Body
	_, _, fb1, rj1 = dm.snap()
	fmt.Printf("  an invented number      compose rejected=%v fallback=%v, the sent message is the template=%v and says nothing of 14=%v, asked %d time, counted rejected +%d fallback +%d  %s\n",
		rl.Rejected, rl.Fallback, liarBody == full.Message(""), !strings.Contains(liarBody, "14"), liar.count(), rj1-rj0, fb1-fb0,
		verdict(rl.Rejected && rl.Fallback && rs.Sent && liarBody == full.Message("") && !strings.Contains(liarBody, "14") &&
			liar.count() == 1 && rj1-rj0 == 1 && fb1-fb0 == 1))

	// 3f. A broken model: an error is a template and is not a "rejection".
	s4 := nthSunday(4)
	broken := &dgComposer{reply: func(int) (string, error) { return "", fmt.Errorf("provider down") }}
	_, _, fb0, rj0 = dm.snap()
	if _, err := newLoop(dgSummary{dgFull()}, broken, rec).Run(ctx, at(s4, 18, 40)); err != nil {
		return err
	}
	rs, err = newLoop(dgSummary{dgFull()}, broken, rec).Run(ctx, at(s4, 19, 0))
	if err != nil {
		return err
	}
	brokenBody := rec.sent[len(rec.sent)-1].Body
	_, _, fb1, rj1 = dm.snap()
	fmt.Printf("  a broken model          sent=%v, the template=%v, fallback +%d, rejected +%d  %s\n",
		rs.Sent, brokenBody == full.Message(""), fb1-fb0, rj1-rj0,
		verdict(rs.Sent && brokenBody == full.Message("") && fb1-fb0 == 1 && rj1 == rj0))

	// ...and a real model client against a provider that answers 500, and against
	// one that answers well, so the production composer and its prompt are the ones
	// exercised.
	badSrv := newCWModel(func(int) (int, string) { return 500, "" })
	defer badSrv.srv.Close()
	goodSrv := newCWModel(func(int) (int, string) {
		return 200, "Read reached 4 in a row, its best run yet, while Gym sits at 2 of 3."
	})
	defer goodSrv.srv.Close()
	routingFor := func(url string) *model.Routing {
		return &model.Routing{Tasks: map[model.Task]model.TaskRouting{
			model.TaskDigest: {Thinking: true, Tiers: []model.Tier{{Model: "naviseed-digest", BaseURL: url, TimeoutSeconds: 5}}},
		}}
	}
	mc := func(url string) *digest.ModelComposer {
		r := routingFor(url)
		return digest.NewModelComposer(model.New(log.With("component", "model-digest"), r, "", st, dm.Metrics), r, "config/persona.md.does-not-exist")
	}
	s5 := nthSunday(5)
	_, _, fb0, rj0 = dm.snap()
	if _, err := newLoop(dgSummary{dgFull()}, mc(badSrv.srv.URL), rec).Run(ctx, at(s5, 18, 40)); err != nil {
		return err
	}
	if _, err := newLoop(dgSummary{dgFull()}, mc(badSrv.srv.URL), rec).Run(ctx, at(s5, 19, 0)); err != nil {
		return err
	}
	badBody := rec.sent[len(rec.sent)-1].Body
	_, _, fb1, _ = dm.snap()
	s6 := nthSunday(6)
	if _, err := newLoop(dgSummary{dgFull()}, mc(goodSrv.srv.URL), rec).Run(ctx, at(s6, 18, 40)); err != nil {
		return err
	}
	if _, err := newLoop(dgSummary{dgFull()}, mc(goodSrv.srv.URL), rec).Run(ctx, at(s6, 19, 0)); err != nil {
		return err
	}
	goodBody := rec.sent[len(rec.sent)-1].Body
	prompt := goodSrv.prompt(1)
	call, err := lastLLMCall(ctx, st)
	if err != nil {
		return err
	}
	realCfg, loadErr := model.LoadRouting(routingPath)
	thinkingOn := loadErr == nil && realCfg.Tasks[model.TaskDigest].Thinking
	fmt.Printf("  the real composer       500 provider -> template=%v (fallback +%d); good provider -> observation sent=%v, task=%s, committed config has thinking on=%v  %s\n",
		badBody == full.Message(""), fb1-fb0, strings.Contains(goodBody, "its best run yet"), call.Task, thinkingOn,
		verdict(badBody == full.Message("") && fb1-fb0 == 1 && strings.Contains(goodBody, "its best run yet") &&
			call.Task == string(model.TaskDigest) && thinkingOn))
	fmt.Printf("  the prompt              hard rules after the persona, no guilt, only the given numbers, and the figures themselves  %s\n",
		verdict(strings.Contains(prompt, "Hard rules") && strings.Contains(prompt, "Never guilt, never scold") &&
			strings.Contains(prompt, "Use only numbers that appear in the figures") && strings.Contains(prompt, full.Facts)))

	// 3g. A thin week: the short form, with no model call and no percentage.
	s7 := nthSunday(7)
	thinC := &dgComposer{reply: func(int) (string, error) { return "should never be asked", nil }}
	fullN0, shortN0, _, _ := dm.snap()
	if _, err := newLoop(dgSummary{dgThin()}, thinC, rec).Run(ctx, at(s7, 18, 40)); err != nil {
		return err
	}
	rt, err := newLoop(dgSummary{dgThin()}, thinC, rec).Run(ctx, at(s7, 19, 0))
	if err != nil {
		return err
	}
	thinBody := rec.sent[len(rec.sent)-1].Body
	fullN1, shortN1, _, _ := dm.snap()
	fmt.Printf("  a thin week             sent=%v form=%s, model asked %d times, body=%q, counted short +%d full +%d  %s\n",
		rt.Sent, rt.Form, thinC.count(), strings.SplitN(thinBody, "\n", 2)[0], shortN1-shortN0, fullN1-fullN0,
		verdict(rt.Sent && rt.Form == digest.FormShort && thinC.count() == 0 && thinBody == thin.Message("") &&
			shortN1-shortN0 == 1 && fullN1 == fullN0))

	// 3h. A week with nothing in it: nothing is sent, the date is latched anyway so
	// it is not retried all evening, and no conversations row says anything.
	s8 := nthSunday(8)
	nSent := len(rec.sent)
	empty := &dgComposer{reply: func(int) (string, error) { return "should never be asked", nil }}
	if _, err := newLoop(dgSummary{dgEmpty()}, empty, rec).Run(ctx, at(s8, 18, 40)); err != nil {
		return err
	}
	re, err := newLoop(dgSummary{dgEmpty()}, empty, rec).Run(ctx, at(s8, 19, 0))
	if err != nil {
		return err
	}
	re2, err := newLoop(dgSummary{dgEmpty()}, empty, rec).Run(ctx, at(s8, 19, 30))
	if err != nil {
		return err
	}
	_, anyRow, err := digestRow(dateOf(s8))
	if err != nil {
		return err
	}
	latchedDate, latched, err := st.LastDigestDate(ctx)
	if err != nil {
		return err
	}
	openAfter, openOK, err := st.OpenDigest(ctx, at(s8, 20, 0))
	if err != nil {
		return err
	}
	fmt.Printf("  an empty week           skipped=%v, sent nothing=%v, no row=%v, latched %s=%v, not retried=%v, opens no reply window=%v, model asked %d times  %s\n",
		re.Skipped, len(rec.sent) == nSent, !anyRow, dateOf(s8), latched && latchedDate == dateOf(s8), re2.Phase == "" && !re2.Skipped,
		!openOK && openAfter == "", empty.count(),
		verdict(re.Skipped && len(rec.sent) == nSent && !anyRow && latched && latchedDate == dateOf(s8) && re2.Phase == "" &&
			!openOK && empty.count() == 0))

	// 3i. A send that fails writes nothing; the next pass sends once.
	s9 := nthSunday(9)
	if _, err := newLoop(dgSummary{dgFull()}, good, rec).Run(ctx, at(s9, 18, 45)); err != nil {
		return err
	}
	_, failErr := newLoop(dgSummary{dgFull()}, good, &failingTransport{}).Run(ctx, at(s9, 19, 0))
	_, latchedAfterFail, err := st.LastDigestDate(ctx)
	if err != nil {
		return err
	}
	lastBefore, _, _ := st.LastDigestDate(ctx)
	_, _, stagedStill, err := st.DigestPending(ctx)
	if err != nil {
		return err
	}
	nBefore := len(rec.sent)
	rr, err := newLoop(dgSummary{dgFull()}, good, rec).Run(ctx, at(s9, 19, 1))
	if err != nil {
		return err
	}
	fmt.Printf("  a failed send           error=%v, not latched for %s=%v, staged text kept=%v; the next pass sent=%v, +%d message  %s\n",
		failErr != nil, dateOf(s9), latchedAfterFail && lastBefore == dateOf(s9), stagedStill, rr.Sent, len(rec.sent)-nBefore,
		verdict(failErr != nil && !(latchedAfterFail && lastBefore == dateOf(s9)) && stagedStill && rr.Sent && len(rec.sent)-nBefore == 1))

	// ---- 4. the figures are the real service's ----------------------------------

	runID := strings.ToLower(domain.NewID()[16:])
	newItem := func(label string) (domain.Item, error) {
		policy := domain.NotifyAtTime
		return st.CreateItem(ctx, domain.NewItem{
			Title:        fmt.Sprintf("dg %s %s", label, runID),
			Schedule:     json.RawMessage(`{"kind":"fixed","rrule":"FREQ=DAILY","at":"18:00"}`),
			TZ:           tz,
			NotifyPolicy: &policy,
		})
	}
	history := func(item domain.Item, outcomes ...domain.Status) error {
		// d = len..1 days ago, oldest first, inside the trailing week (6 local days
		// back through yesterday).
		for i, status := range outcomes {
			when := time.Date(today.Year(), today.Month(), today.Day()-(len(outcomes)-i), 9, 0, 0, 0, loc)
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
	done, miss, skip := domain.StatusCompleted, domain.StatusMissed, domain.StatusSkipped
	itemX, err := newItem("walk")
	if err != nil {
		return err
	}
	if err := history(itemX, miss, done, done, done, done, skip); err != nil {
		return err
	}
	itemY, err := newItem("read")
	if err != nil {
		return err
	}
	if err := history(itemY, done, done, done, done, done, done); err != nil {
		return err
	}

	svc := stats.New(st, fallback)
	q, ve := stats.NewQuery("week", "", "")
	if ve != nil {
		return ve
	}
	sum, err := svc.Summary(ctx, q)
	if err != nil {
		return err
	}
	realFig := digest.Settle(sum)

	mat := materializer.New(log.With("component", "materializer"), st, fallback)
	tools := agent.New(st, mat, table, fallback, nil)
	res, err := callTool(ctx, tools, "get_stats", agent.GetStatsArgs{Range: "week"})
	if err != nil {
		return err
	}
	chat := conversation.Confirmation("get_stats", res)
	toolJSON, _ := json.Marshal(res.Stats)
	sumJSON, _ := json.Marshal(sum)

	// Every number the digest says is a number get_stats says, in its JSON or in
	// its chat text (the percentages are the text's; the JSON holds the fraction).
	numErr := digest.CheckNumbers(chat+"\n"+string(toolJSON), realFig.Facts)
	numbersOK := numErr == nil
	lines := 0
	allLines := true
	for _, it := range sum.Items {
		if it.Totals.Chains == 0 || lines == 10 {
			continue
		}
		lines++
		want := fmt.Sprintf("%s: %s · %s", it.Title, it.Meta(), stats.StreakPhrase(it.CurrentStreak, it.LongestStreak))
		allLines = allLines && strings.Contains(realFig.Facts, want)
	}
	headline := stats.RangePhrase(sum.Range) + ": "
	if sum.CompletionRate != nil {
		headline += stats.Percent(*sum.CompletionRate) + " done"
	}
	headline += " (" + sum.RateDetail() + ")."
	var mine []string
	for _, it := range sum.Items {
		if it.ItemID == itemX.ID || it.ItemID == itemY.ID {
			mine = append(mine, it.Title+" "+stats.StreakPhrase(it.CurrentStreak, it.LongestStreak)+" "+it.Meta())
		}
	}
	fmt.Printf("  the same week          get_stats(week) == the Service's Summary=%v; form=%s; the headline is the page's words=%v; each item line is the page's phrases=%v (%d lines)  %s\n",
		string(toolJSON) == string(sumJSON), realFig.Form, strings.HasPrefix(realFig.Facts, headline), allLines, lines,
		verdict(string(toolJSON) == string(sumJSON) && realFig.Form == digest.FormFull && strings.HasPrefix(realFig.Facts, headline) && allLines))
	fmt.Printf("  every number agrees     each number in the digest's figures is in get_stats' result or chat text  %s\n", verdict(numbersOK))
	if !numbersOK {
		fmt.Println("    err:", numErr)
		fmt.Println("    chat:", chat)
		fmt.Println("    digest:", realFig.Facts)
	}
	for _, l := range mine {
		fmt.Println("    fixture:", l)
	}
	// 4 settled + 1 skipped + 1 missed for the walk, 6 completed for the read.
	walk, read := stats.ItemStats{}, stats.ItemStats{}
	for _, it := range sum.Items {
		switch it.ItemID {
		case itemX.ID:
			walk = it
		case itemY.ID:
			read = it
		}
	}
	fmt.Printf("  fixtures as expected    walk: 80%% done, streak 4 in a row (best 4), skip note shown; read: 100%% done, 6 in a row (best 6)  %s\n",
		verdict(walk.CompletionRate != nil && stats.Percent(*walk.CompletionRate) == "80%" && walk.CurrentStreak == 4 && walk.LongestStreak == 4 &&
			strings.Contains(walk.Meta(), "skipped: away") && read.CompletionRate != nil && stats.Percent(*read.CompletionRate) == "100%" &&
			read.CurrentStreak == 6 && read.LongestStreak == 6))

	// ---- 5. a reply is recognised ------------------------------------------------

	// The one pass at the real clock: this very minute, on today's weekday, with the
	// real Service and no composer, so it is the template.
	if err := st.ClearDigestState(ctx); err != nil {
		return err
	}
	now := time.Now()
	lnow := now.In(loc)
	realDate := dateOf(lnow)
	capToday0, err := st.ProactiveCount(ctx, realDate)
	if err != nil {
		return err
	}
	realRec := &recordingTransport{}
	realLoop := digest.New(log.With("loop", "digest-real"), st, svc, realRec, dm, nil, lnow.Format("15:04"), fallback).WithWeekday(lnow.Weekday())
	rr, err = realLoop.Run(ctx, now)
	if err != nil {
		return err
	}
	capToday1, err := st.ProactiveCount(ctx, realDate)
	if err != nil {
		return err
	}
	fmt.Printf("  at the real clock       sent=%v form=%s fallback=%v, and the daily count stayed %d -> %d  %s\n",
		rr.Sent, rr.Form, rr.Fallback, capToday0, capToday1, verdict(rr.Sent && rr.Form == digest.FormFull && rr.Fallback && capToday0 == capToday1))

	in1, ok1, err := st.OpenDigest(ctx, now.Add(time.Hour))
	if err != nil {
		return err
	}
	_, ok25, err := st.OpenDigest(ctx, now.Add(25*time.Hour))
	if err != nil {
		return err
	}
	fmt.Printf("  the reply window        open at +1h (%s)=%v, closed at +25h=%v  %s\n",
		in1, ok1 && in1 == store.DigestRef(realDate), !ok25, verdict(ok1 && in1 == store.DigestRef(realDate) && !ok25))

	var captured []wireRequest
	srv := capturingToolCallServer("list_items", `{}`, &captured)
	defer srv.Close()
	routing := &model.Routing{Tasks: map[model.Task]model.TaskRouting{
		model.TaskCRUD: {Tiers: []model.Tier{{Model: "naviseed-digest-reply", BaseURL: srv.URL, TimeoutSeconds: 5}}},
	}}
	client := model.New(log.With("component", "model-digest-reply"), routing, "", st, dm.Metrics)
	sender := &fakeSender{}
	ladder := conversation.New(client, tools, routing, st, table, "config/persona.md.does-not-exist", fallback, sender)
	if err := ladder.Handle(ctx, transport.IncomingMessage{SenderID: "111", Text: "drop the walk, then", Transport: telegram.Name}); err != nil {
		return err
	}
	sys, inHistory := "", false
	if len(captured) > 0 {
		sys = captured[0].systemContent()
		for _, m := range captured[0].Messages {
			if m.Role == "assistant" && strings.Contains(m.Content, "Over the last 7 days") {
				inHistory = true
			}
		}
	}
	fmt.Printf("  recognised by context   prompt carries \"Context ref:  digest:%s\"=%v, the rule for it=%v, the digest is in history=%v  %s\n",
		realDate, strings.Contains(sys, "Context ref:  digest:"+realDate), strings.Contains(sys, `"Context ref: digest:" line`), inHistory,
		verdict(strings.Contains(sys, "Context ref:  digest:"+realDate) && strings.Contains(sys, `"Context ref: digest:" line`) && inHistory))

	// And once the window closes the line is gone, whatever else is open.
	if err := st.ClearDigestState(ctx); err != nil {
		return err
	}
	var captured2 []wireRequest
	srv2 := capturingToolCallServer("list_items", `{}`, &captured2)
	defer srv2.Close()
	routing2 := &model.Routing{Tasks: map[model.Task]model.TaskRouting{
		model.TaskCRUD: {Tiers: []model.Tier{{Model: "naviseed-digest-reply", BaseURL: srv2.URL, TimeoutSeconds: 5}}},
	}}
	ladder2 := conversation.New(model.New(log.With("component", "model-digest-reply2"), routing2, "", st, dm.Metrics),
		tools, routing2, st, table, "config/persona.md.does-not-exist", fallback, sender)
	if err := ladder2.Handle(ctx, transport.IncomingMessage{SenderID: "111", Text: "what do I have today?", Transport: telegram.Name}); err != nil {
		return err
	}
	gone := len(captured2) > 0 && !strings.Contains(captured2[0].systemContent(), "Context ref:  digest:")
	fmt.Printf("  not forever             with no digest open the prompt has no digest line  %s\n", verdict(gone))

	// ---- 6. the off switch -------------------------------------------------------

	envCase := func(value string, set bool) (string, error) {
		old, had := os.LookupEnv("DIGEST_AT")
		defer func() {
			if had {
				_ = os.Setenv("DIGEST_AT", old)
			} else {
				_ = os.Unsetenv("DIGEST_AT")
			}
		}()
		if set {
			_ = os.Setenv("DIGEST_AT", value)
		} else {
			_ = os.Unsetenv("DIGEST_AT")
		}
		c, err := config.Load()
		return c.Schedule.DigestAt, err
	}
	unset, e1 := envCase("", false)
	blank, e2 := envCase("  ", true)
	valid, e3 := envCase("19:00", true)
	_, e4 := envCase("7pm", true)
	_, e5 := envCase("25:00", true)
	fmt.Printf("  DIGEST_AT               unset=%q blank=%q 19:00=%q, \"7pm\" and \"25:00\" fail the boot=%v  %s\n",
		unset, blank, valid, e4 != nil && e5 != nil,
		verdict(e1 == nil && e2 == nil && e3 == nil && unset == "" && blank == "" && valid == "19:00" && e4 != nil && e5 != nil))

	scrape := func(m *metrics.Metrics) string {
		rec := httptest.NewRecorder()
		m.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
		return rec.Body.String()
	}
	off := metrics.New()
	off.IncDigestSent("full") // a call into an unregistered series must be harmless
	offBody := scrape(off)
	on := metrics.New()
	on.RegisterDigest()
	onBody := scrape(on)
	series := []string{"navi_digest_sent_total{form=\"full\"} 0", "navi_digest_sent_total{form=\"short\"} 0",
		"navi_digest_fallback_total 0", "navi_digest_number_rejected_total 0"}
	onAll := true
	for _, s := range series {
		onAll = onAll && strings.Contains(onBody, s)
	}
	fmt.Printf("  no series when off      a registry without RegisterDigest exports no navi_digest series=%v; with it, all four at zero=%v  %s\n",
		!strings.Contains(offBody, "navi_digest"), onAll, verdict(!strings.Contains(offBody, "navi_digest") && onAll))

	// Leave nothing behind for a later section's reply: the real-clock digest above
	// was cleared, and the constructed Sundays' rows are old enough to be history.
	return st.ClearDigestState(ctx)
}

package stats

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/aidenpaleczny/navi/internal/domain"
	"github.com/aidenpaleczny/navi/internal/schedule"
	"github.com/aidenpaleczny/navi/internal/store"
)

// Reader is the slice of the repository the statistics use, declared by the
// consumer like httpapi.Store. *store.Store satisfies it. Note what is absent:
// there is no occurrences read at all, so nothing here can count a row where a
// chain was meant.
type Reader interface {
	StatsChains(ctx context.Context, f store.StatsFilter) ([]store.StatsChain, error)
	ListItems(ctx context.Context, filter store.ItemFilter, now time.Time) ([]domain.Item, error)
	GetItem(ctx context.Context, id string) (domain.Item, error)
	ListGoalProgress(ctx context.Context, filter store.GoalFilter, loc *time.Location) ([]store.GoalProgress, error)
	CurrentTZ(ctx context.Context) (string, bool, error)
}

// Service computes statistics. The HTTP handlers and the get_stats tool each hold
// one built from the same store, and the device zone and the clock are resolved
// in here rather than passed in, so the two entry points cannot disagree about
// what "this week" or "today" means.
type Service struct {
	r         Reader
	defaultTZ *time.Location
	now       func() time.Time
}

// New builds a Service. defaultTZ is the bottom rung of schedule.Zones, the same
// value every other reader of the device zone is given.
func New(r Reader, defaultTZ *time.Location) *Service {
	return &Service{r: r, defaultTZ: defaultTZ, now: time.Now}
}

// prepare resolves what every entry point needs before it reads: the person's
// zone, the window, and - when the query names an item - that the item exists
// and is a reminder. An archived item is accepted, because its history is not
// gone (package doc, Archived items).
func (s *Service) prepare(ctx context.Context, q Query) (window, *domain.Item, error) {
	zones, err := schedule.LoadZones(ctx, s.r, s.defaultTZ)
	if err != nil {
		return window{}, nil, fmt.Errorf("stats: resolve zones: %w", err)
	}
	w := resolve(q.Range, s.now(), zones.Local())

	if q.ItemID == "" {
		return w, nil, nil
	}
	item, err := s.r.GetItem(ctx, q.ItemID)
	if errors.Is(err, store.ErrNotFound) {
		return window{}, nil, domain.Invalid("stats_item_id", "item_id", "item %q does not exist", q.ItemID)
	}
	if err != nil {
		return window{}, nil, fmt.Errorf("stats: item %s: %w", q.ItemID, err)
	}
	if item.Kind != domain.KindReminder {
		return window{}, nil, domain.Invalid("stats_item_kind", "item_id",
			"item %q is an %s: statistics cover reminders", q.ItemID, item.Kind)
	}
	return w, &item, nil
}

// Summary is V5's numbers: totals, completion rate, median lag, a streak row per
// item, and the goal rows.
//
// It reads every chain up to now in one go, whatever the range. Streaks are
// lifetime, so the history is needed regardless of the window, and the window's
// totals are a filter over the same slice - which means this one read cannot
// disagree with itself. (Timeseries and heatmap read only their own window and
// get the view's range pruning; see store.StatsChains.)
func (s *Service) Summary(ctx context.Context, q Query) (Summary, error) {
	w, item, err := s.prepare(ctx, q)
	if err != nil {
		return Summary{}, err
	}

	history, err := s.r.StatsChains(ctx, store.StatsFilter{ItemID: q.ItemID, To: w.to})
	if err != nil {
		return Summary{}, err
	}

	var inWindow []store.StatsChain
	byItem := map[string][]store.StatsChain{}
	byItemWindow := map[string][]store.StatsChain{}
	for _, c := range history {
		byItem[c.ItemID] = append(byItem[c.ItemID], c)
		if w.contains(c) {
			inWindow = append(inWindow, c)
			byItemWindow[c.ItemID] = append(byItemWindow[c.ItemID], c)
		}
	}

	var earliest *store.StatsChain
	if len(history) > 0 {
		earliest = &history[0]
	}

	headers, err := s.itemHeaders(ctx, w, item, inWindow)
	if err != nil {
		return Summary{}, err
	}

	out := Summary{
		Range:    string(q.Range),
		Timezone: w.loc.String(),
		From:     w.fromDate(earliest),
		To:       w.today(),
		ItemID:   q.itemPtr(),
		Totals:   totalsOf(inWindow),
		Items:    make([]ItemStats, 0, len(headers)),
	}
	out.CompletionRate = out.Totals.rate()
	out.MedianLagMinutes, out.LagSamples = medianLag(inWindow)

	for _, h := range headers {
		row := ItemStats{ItemID: h.id, Title: h.title, Archived: h.archived}
		own := byItemWindow[h.id]
		row.Totals = totalsOf(own)
		row.CompletionRate = row.Totals.rate()
		row.CurrentStreak, row.LongestStreak = streaks(byItem[h.id])
		row.MedianLagMinutes, row.LagSamples = medianLag(own)
		row.RecentSkipNotes = recentSkipNotes(own, 3)
		out.Items = append(out.Items, row)
	}

	out.Goals, err = s.goals(ctx, q, w)
	if err != nil {
		return Summary{}, err
	}
	return out, nil
}

type itemHeader struct {
	id       string
	title    string
	archived bool
}

// itemHeaders is the set of items a summary lists: the named item alone, or
// every unarchived reminder plus any archived item with chains in the window.
// Active first, then archived, each by title.
func (s *Service) itemHeaders(ctx context.Context, w window, only *domain.Item, inWindow []store.StatsChain) ([]itemHeader, error) {
	seen := map[string]itemHeader{}
	if only != nil {
		seen[only.ID] = itemHeader{only.ID, only.Title, only.ArchivedAt != nil}
	} else {
		items, err := s.r.ListItems(ctx, store.FilterAll, w.now)
		if err != nil {
			return nil, fmt.Errorf("stats: list items: %w", err)
		}
		for _, it := range items {
			if it.Kind == domain.KindReminder {
				seen[it.ID] = itemHeader{it.ID, it.Title, false}
			}
		}
		for _, c := range inWindow {
			if _, ok := seen[c.ItemID]; !ok {
				seen[c.ItemID] = itemHeader{c.ItemID, c.ItemTitle, c.ItemArchived}
			}
		}
	}

	out := make([]itemHeader, 0, len(seen))
	for _, h := range seen {
		out = append(out, h)
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.archived != b.archived {
			return !a.archived
		}
		if ta, tb := strings.ToLower(a.title), strings.ToLower(b.title); ta != tb {
			return ta < tb
		}
		return a.id < b.id
	})
	return out, nil
}

// recentSkipNotes returns up to n reasons from skipped chains, newest first.
func recentSkipNotes(chains []store.StatsChain, n int) []string {
	notes := []string{}
	for i := len(chains) - 1; i >= 0 && len(notes) < n; i-- {
		c := chains[i]
		if Classify(c) == ClassSkipped && c.TerminalNote != nil && *c.TerminalNote != "" {
			notes = append(notes, *c.TerminalNote)
		}
	}
	return notes
}

// goals is the summary's goal rows. The progress is store.ListGoalProgress as it
// stands - the read the agent's context block and the morning briefing use - and
// this only selects and shapes it. A goal is listed when it is active or its
// period overlaps the window; for a named item, when it is linked to that item.
func (s *Service) goals(ctx context.Context, q Query, w window) ([]GoalStats, error) {
	progress, err := s.r.ListGoalProgress(ctx, store.GoalFilterAll, w.loc)
	if err != nil {
		return nil, fmt.Errorf("stats: goals: %w", err)
	}

	from, to := "", w.today()
	if !w.all {
		from = w.from.In(w.loc).Format(domain.DateLayout)
	}

	out := make([]GoalStats, 0, len(progress))
	for _, p := range progress {
		g := p.Goal
		if q.ItemID != "" && (g.ItemID == nil || *g.ItemID != q.ItemID) {
			continue
		}
		// ISO dates compare as text. The window test is skipped for "all".
		overlaps := w.all || (g.PeriodStart <= to && g.PeriodEnd >= from)
		if g.Status != domain.GoalActive && !overlaps {
			continue
		}

		row := GoalStats{
			GoalID: g.ID, Title: g.Title, Status: string(g.Status),
			PeriodKind: string(g.PeriodKind), PeriodStart: g.PeriodStart, PeriodEnd: g.PeriodEnd,
			ItemID: g.ItemID, Met: p.Met(),
		}
		if p.ItemLinked {
			completed, target := p.Completed, p.Target
			row.Completed, row.Target = &completed, &target
			if v := p.Velocity(w.now, w.loc); v != nil {
				row.Velocity = round(*v, 2)
			}
		} else {
			row.ProgressPct, row.Note = p.LatestPct, p.LatestNote
		}
		out = append(out, row)
	}
	return out, nil
}

// Timeseries is V5's completion rate over time. Each bucket carries the same
// Totals a summary does and the same rate, so a chart and a number agree.
//
// A chain is filed under the local date (or Monday) of its root's scheduled_at.
// Buckets are zero-filled from the window's start to today so a quiet stretch is
// a row of zeros rather than a gap; for range=all the first bucket is the
// earliest chain's.
func (s *Service) Timeseries(ctx context.Context, q Query) (Timeseries, error) {
	w, _, err := s.prepare(ctx, q)
	if err != nil {
		return Timeseries{}, err
	}
	chains, err := s.r.StatsChains(ctx, store.StatsFilter{ItemID: q.ItemID, From: w.from, To: w.to})
	if err != nil {
		return Timeseries{}, err
	}

	out := Timeseries{
		Range: string(q.Range), Bucket: string(q.Bucket), Timezone: w.loc.String(),
		To: w.today(), ItemID: q.itemPtr(), Buckets: []TimeBucket{},
	}
	var earliest *store.StatsChain
	if len(chains) > 0 {
		earliest = &chains[0]
	}
	out.From = w.fromDate(earliest)

	bucketOf := func(t time.Time) time.Time {
		if q.Bucket == BucketWeek {
			return weekStart(t, w.loc)
		}
		return dayStart(t, w.loc)
	}
	step := 1
	if q.Bucket == BucketWeek {
		step = 7
	}

	var first time.Time
	switch {
	case !w.all:
		first = bucketOf(w.from)
	case earliest != nil:
		first = bucketOf(earliest.ScheduledAt)
	default:
		return out, nil
	}

	index := map[string]int{}
	last := bucketOf(w.now)
	for b := first; !b.After(last); b = time.Date(b.Year(), b.Month(), b.Day()+step, 0, 0, 0, 0, w.loc) {
		key := b.Format(domain.DateLayout)
		index[key] = len(out.Buckets)
		out.Buckets = append(out.Buckets, TimeBucket{Start: key})
	}
	for _, c := range chains {
		if i, ok := index[bucketOf(c.ScheduledAt).Format(domain.DateLayout)]; ok {
			out.Buckets[i].Totals.add(Classify(c))
		}
	}
	for i := range out.Buckets {
		out.Buckets[i].CompletionRate = out.Buckets[i].Totals.rate()
	}
	return out, nil
}

// Heatmap is V5's time-of-day completion grid: chains in the window that were
// completed, bucketed by the local weekday and hour of the completion itself.
// The time used is the one the chain was resolved at, which for a completion
// reported at a 21:00 check-in is the report and not the deed - a limit of the
// data, since nothing records when a thing was actually done.
func (s *Service) Heatmap(ctx context.Context, q Query) (Heatmap, error) {
	w, _, err := s.prepare(ctx, q)
	if err != nil {
		return Heatmap{}, err
	}
	chains, err := s.r.StatsChains(ctx, store.StatsFilter{ItemID: q.ItemID, From: w.from, To: w.to})
	if err != nil {
		return Heatmap{}, err
	}

	out := Heatmap{
		Range: string(q.Range), Timezone: w.loc.String(), To: w.today(),
		ItemID: q.itemPtr(), Weekdays: weekdayLabels,
	}
	var earliest *store.StatsChain
	if len(chains) > 0 {
		earliest = &chains[0]
	}
	out.From = w.fromDate(earliest)

	for _, c := range chains {
		if !c.Completed || c.CompletedAt == nil {
			continue
		}
		t := c.CompletedAt.In(w.loc)
		d := (int(t.Weekday()) + 6) % 7
		out.Cells[d][t.Hour()]++
		out.Total++
		if out.Cells[d][t.Hour()] > out.Max {
			out.Max = out.Cells[d][t.Hour()]
		}
	}
	return out, nil
}

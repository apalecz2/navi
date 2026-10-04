package stats

import (
	"math"
	"sort"
	"time"

	"github.com/aidenpaleczny/navi/internal/domain"
	"github.com/aidenpaleczny/navi/internal/store"
)

// Classify is the definition in the package doc, as code: one chain, one class.
//
// Completed is tested first and on its own, because D-011 makes any completed
// link decisive whatever the last link says. Past that the last link's status
// decides, and a chain that has not ended is awaiting when a check-in has asked
// about it and open when nobody has.
func Classify(c store.StatsChain) Class {
	if c.Completed {
		return ClassCompleted
	}
	switch c.Terminal {
	case domain.StatusSkipped:
		return ClassSkipped
	case domain.StatusMissed:
		return ClassMissed
	}
	if c.Reconciled {
		return ClassAwaiting
	}
	return ClassOpen
}

func (t *Totals) add(c Class) {
	t.Chains++
	switch c {
	case ClassCompleted:
		t.Completed++
		t.Settled++
	case ClassMissed:
		t.Missed++
		t.Settled++
	case ClassSkipped:
		t.Skipped++
	case ClassAwaiting:
		t.Awaiting++
	case ClassOpen:
		t.Open++
	}
}

// rate is completed / settled, null when nothing has settled.
func (t Totals) rate() *float64 {
	if t.Settled == 0 {
		return nil
	}
	return round(float64(t.Completed)/float64(t.Settled), 4)
}

func round(x float64, places int) *float64 {
	p := math.Pow(10, float64(places))
	v := math.Round(x*p) / p
	return &v
}

// streaks walks one item's chains in scheduled order. Only a completed chain
// moves the run and only a missed one ends it; see the package doc for why a
// skipped, awaiting or open chain does neither.
func streaks(chains []store.StatsChain) (current, longest int) {
	for _, c := range chains {
		switch Classify(c) {
		case ClassCompleted:
			current++
			if current > longest {
				longest = current
			}
		case ClassMissed:
			current = 0
		}
	}
	return current, longest
}

// medianLag is the median minutes from first notification to completion, and how
// many chains it is over. See the package doc for who is a sample.
func medianLag(chains []store.StatsChain) (*float64, int) {
	var lags []float64
	for _, c := range chains {
		if !c.Completed || c.NotifiedAt == nil || c.CompletedAt == nil {
			continue
		}
		if c.CompletedAt.Before(*c.NotifiedAt) {
			continue
		}
		lags = append(lags, c.CompletedAt.Sub(*c.NotifiedAt).Minutes())
	}
	if len(lags) == 0 {
		return nil, 0
	}
	sort.Float64s(lags)
	mid := len(lags) / 2
	if len(lags)%2 == 1 {
		return round(lags[mid], 1), len(lags)
	}
	return round((lags[mid-1]+lags[mid])/2, 1), len(lags)
}

// totalsOf classifies and counts a set of chains.
func totalsOf(chains []store.StatsChain) Totals {
	var t Totals
	for _, c := range chains {
		t.add(Classify(c))
	}
	return t
}

// window is a resolved Range: the half-open instant range chains are selected
// by, and the local dates it is reported as.
type window struct {
	from time.Time // zero for all
	to   time.Time // exclusive: now, plus the second FormatTime cannot resolve below
	all  bool
	loc  *time.Location
	now  time.Time
}

// resolve turns a range into a window against now, in loc.
//
// The start is local midnight n-1 days before today, built from the date and not
// by subtracting 24-hour multiples, which would land an hour off across a DST
// transition. to is exclusive and is now rounded up to the next second, so a
// chain due exactly now is in and anything later is not.
func resolve(r Range, now time.Time, loc *time.Location) window {
	w := window{loc: loc, now: now, to: now.Truncate(time.Second).Add(time.Second)}
	n := r.days()
	if n == 0 {
		w.all = true
		return w
	}
	t := now.In(loc)
	w.from = time.Date(t.Year(), t.Month(), t.Day()-(n-1), 0, 0, 0, 0, loc)
	return w
}

func (w window) contains(c store.StatsChain) bool {
	return w.all || !c.ScheduledAt.Before(w.from)
}

func (w window) today() string { return w.now.In(w.loc).Format(domain.DateLayout) }

// fromDate is the window's first local date, or nil for an all window that found
// nothing to start from.
func (w window) fromDate(earliest *store.StatsChain) *string {
	if !w.all {
		return strPtr(w.from.In(w.loc).Format(domain.DateLayout))
	}
	if earliest == nil {
		return nil
	}
	return strPtr(earliest.ScheduledAt.In(w.loc).Format(domain.DateLayout))
}

// weekStart is the local Monday on or before t's local date - the week the
// goals and the materializer use.
func weekStart(t time.Time, loc *time.Location) time.Time {
	d := t.In(loc)
	back := (int(d.Weekday()) + 6) % 7
	return time.Date(d.Year(), d.Month(), d.Day()-back, 0, 0, 0, 0, loc)
}

func dayStart(t time.Time, loc *time.Location) time.Time {
	d := t.In(loc)
	return time.Date(d.Year(), d.Month(), d.Day(), 0, 0, 0, 0, loc)
}

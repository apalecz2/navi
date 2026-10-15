package proactive

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/aidenpaleczny/navi/internal/domain"
	"github.com/aidenpaleczny/navi/internal/schedule"
)

// Alternative is the specific change a proposal offers. It is computed by code
// from the item's schedule and history, never by the model: the model words the
// offer, and the offer it words is this (D-013). What a "yes" would mean is
// therefore fixed before the person is asked, which is what lets the composed
// message be checked against it and the Context ref block state it exactly.
type Alternative struct {
	// Tool is domain.ProposalUpdateItem or domain.ProposalDeleteItem.
	Tool string

	// Changes is update_item's "changes" object, or nil for delete_item.
	Changes json.RawMessage

	// Summary is the alternative as a phrase that completes "Want me to ...?".
	Summary string

	// Mentions are what a message must say to count as stating the alternative.
	// Each entry is a group of "|"-separated spellings, any one of which satisfies
	// it, case-insensitively: "2|two|twice" for a count, "mon" for Monday.
	Mentions []string
}

// drop is the alternative of last resort, and an honest one: a schedule that
// cannot be made smaller or moved is one whose remaining question is whether it
// should exist.
func drop() Alternative {
	return Alternative{
		Tool:     domain.ProposalDeleteItem,
		Summary:  "drop it",
		Mentions: []string{"drop|remove|archive|delete|stop"},
	}
}

// Alternate picks the alternative for a trigger on an item. live is the local
// time a snoozed chain's live link was pushed to, nil for the other two triggers.
//
// A snooze proposal tries a different time first, because that is what the
// snoozing was saying. Everything else - and a snooze whose time cannot be moved,
// such as a fuzzy schedule with no window - is a smaller ask, and what cannot be
// made smaller is offered as a drop. No branch returns nothing, so a trigger that
// fires always has something specific to say.
func Alternate(item domain.Item, trigger string, live *time.Time, loc *time.Location, now time.Time) Alternative {
	s, err := schedule.Parse(item.Schedule)
	if err != nil {
		return drop()
	}
	if trigger == domain.TriggerSnoozes && live != nil {
		if alt, ok := retime(s, *live, loc, now); ok {
			return alt
		}
	}
	if alt, ok := shrink(s, loc, now); ok {
		return alt
	}
	return drop()
}

// update wraps a replacement schedule as update_item's changes, after checking it
// would be accepted. The check is the validator a real update_item runs, so a
// proposal can never offer a change the ordinary path would then reject on "yes".
func update(s schedule.Schedule, loc *time.Location, now time.Time, summary string, mentions ...string) (Alternative, bool) {
	if err := schedule.Validate(s, now, loc); err != nil {
		return Alternative{}, false
	}
	raw, err := json.Marshal(map[string]any{"schedule": s})
	if err != nil {
		return Alternative{}, false
	}
	return Alternative{Tool: domain.ProposalUpdateItem, Changes: raw, Summary: summary, Mentions: mentions}, true
}

var dayNames = map[string]string{
	"MO": "Mon", "TU": "Tue", "WE": "Wed", "TH": "Thu", "FR": "Fri", "SA": "Sat", "SU": "Sun",
}

// shrink offers the same thing less often: half the count of a fuzzy schedule,
// or every other day of a fixed or windowed one. A daily rule with no day list
// becomes Monday, Wednesday and Friday.
func shrink(s schedule.Schedule, loc *time.Location, now time.Time) (Alternative, bool) {
	switch s.Kind {
	case schedule.KindFuzzy:
		if s.Count == nil || s.Period == nil || *s.Count < 2 {
			return Alternative{}, false
		}
		n := (*s.Count + 1) / 2
		out := s
		out.Count = &n
		return update(out, loc, now,
			fmt.Sprintf("cut it from %d to %d a %s", *s.Count, n, *s.Period), countMention(n))

	case schedule.KindFixed, schedule.KindWindowed:
		if s.RRule == nil {
			return Alternative{}, false
		}
		rule, days, ok := shrinkRRule(*s.RRule)
		if !ok {
			return Alternative{}, false
		}
		out := s
		out.RRule = &rule
		names := make([]string, len(days))
		mentions := make([]string, len(days))
		for i, d := range days {
			names[i] = dayNames[d]
			mentions[i] = strings.ToLower(dayNames[d])
		}
		return update(out, loc, now, "cut it to "+joinAnd(names), mentions...)
	}
	return Alternative{}, false
}

// shrinkRRule halves a rule's day list and returns the new rule and the days it
// keeps. Rules it does not fully understand (an INTERVAL, a numbered weekday such
// as 1MO, a monthly or yearly frequency) are left alone: a half-understood rewrite
// is exactly the silent wrong schedule D-012 exists to prevent.
func shrinkRRule(rule string) (string, []string, bool) {
	type part struct{ key, val string }
	var parts []part
	for _, p := range strings.Split(rule, ";") {
		k, v, ok := strings.Cut(p, "=")
		if !ok {
			return "", nil, false
		}
		if k == "INTERVAL" {
			return "", nil, false
		}
		parts = append(parts, part{k, v})
	}

	freq, byday := "", ""
	for _, p := range parts {
		switch p.key {
		case "FREQ":
			freq = p.val
		case "BYDAY":
			byday = p.val
		}
	}

	var keep []string
	newFreq := freq
	switch {
	case freq == "DAILY" && byday == "":
		keep, newFreq = []string{"MO", "WE", "FR"}, "WEEKLY"
	case (freq == "DAILY" || freq == "WEEKLY") && byday != "":
		all := strings.Split(byday, ",")
		if len(all) < 2 {
			return "", nil, false
		}
		for i, d := range all {
			if _, ok := dayNames[d]; !ok {
				return "", nil, false
			}
			if i%2 == 0 {
				keep = append(keep, d)
			}
		}
	default:
		return "", nil, false
	}

	var b strings.Builder
	wroteDays := false
	for _, p := range parts {
		if b.Len() > 0 {
			b.WriteByte(';')
		}
		switch p.key {
		case "FREQ":
			b.WriteString("FREQ=" + newFreq)
		case "BYDAY":
			b.WriteString("BYDAY=" + strings.Join(keep, ","))
			wroteDays = true
		default:
			b.WriteString(p.key + "=" + p.val)
		}
	}
	if !wroteDays {
		b.WriteString(";BYDAY=" + strings.Join(keep, ","))
	}
	return b.String(), keep, true
}

// retime moves a schedule to the time of day its live link was pushed to,
// rounded to a quarter hour: "you keep pushing it to 19:30, so make it 19:30". A
// fixed schedule's time becomes that; a windowed or fuzzy schedule's window starts
// there and keeps its length.
func retime(s schedule.Schedule, live time.Time, loc *time.Location, now time.Time) (Alternative, bool) {
	at := quarterHour(live.In(loc))

	switch s.Kind {
	case schedule.KindFixed:
		if s.At == nil || *s.At == at {
			return Alternative{}, false
		}
		out := s
		out.At = &at
		return update(out, loc, now, "move it to "+at, timeMention(at))

	case schedule.KindWindowed, schedule.KindFuzzy:
		if len(s.Window) != 2 || s.Window[0] == at {
			return Alternative{}, false
		}
		from, errFrom := minutes(s.Window[0])
		to, errTo := minutes(s.Window[1])
		start, errAt := minutes(at)
		if errFrom != nil || errTo != nil || errAt != nil || to <= from {
			return Alternative{}, false
		}
		end := start + (to - from)
		if end > 23*60+59 {
			end = 23*60 + 59
		}
		if end <= start {
			return Alternative{}, false
		}
		out := s
		out.Window = []string{at, hhmm(end)}
		return update(out, loc, now,
			fmt.Sprintf("start its window at %s (%s-%s)", at, at, hhmm(end)), timeMention(at))
	}
	return Alternative{}, false
}

// quarterHour rounds a local time to the nearest 15 minutes as HH:MM, never
// rolling over midnight: a time pushed to 23:55 is offered as 23:45, not as a
// time on a different day.
func quarterHour(t time.Time) string {
	m := t.Hour()*60 + t.Minute()
	m = (m + 7) / 15 * 15
	if m > 23*60+45 {
		m = 23*60 + 45
	}
	return hhmm(m)
}

func hhmm(m int) string { return fmt.Sprintf("%02d:%02d", m/60, m%60) }

func minutes(s string) (int, error) {
	t, err := time.Parse(LocalTimeLayout, s)
	if err != nil {
		return 0, err
	}
	return t.Hour()*60 + t.Minute(), nil
}

// timeMention is the spellings of a clock time a message might use: the 24-hour
// form it was given in, and the 12-hour forms people write, with and without the
// space. A message that says "7:30pm" has stated 19:30.
func timeMention(hm string) string {
	m, err := minutes(hm)
	if err != nil {
		return hm
	}
	h24, min := m/60, m%60
	h12, suffix := h24%12, "am"
	if h12 == 0 {
		h12 = 12
	}
	if h24 >= 12 {
		suffix = "pm"
	}
	forms := []string{hm, fmt.Sprintf("%d:%02d", h24, min)}
	for _, sep := range []string{"", " "} {
		forms = append(forms, fmt.Sprintf("%d:%02d%s%s", h12, min, sep, suffix))
		if min == 0 {
			forms = append(forms, fmt.Sprintf("%d%s%s", h12, sep, suffix))
		}
	}
	return strings.Join(forms, "|")
}

// countMention is the spellings of a small count a message might use for it.
func countMention(n int) string {
	words := map[int]string{1: "once|one", 2: "twice|two", 3: "three", 4: "four", 5: "five", 6: "six", 7: "seven"}
	if w, ok := words[n]; ok {
		return strconv.Itoa(n) + "|" + w
	}
	return strconv.Itoa(n)
}

// joinAnd renders "Mon", "Mon and Wed", "Mon, Wed and Fri".
func joinAnd(xs []string) string {
	switch len(xs) {
	case 0:
		return ""
	case 1:
		return xs[0]
	}
	return strings.Join(xs[:len(xs)-1], ", ") + " and " + xs[len(xs)-1]
}

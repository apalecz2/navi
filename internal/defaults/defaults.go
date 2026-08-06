// Package defaults reads /config/defaults.yaml, the vocabulary table that
// resolves under-specified schedules (A6, D-016).
//
// The table has two readers and that is the whole point of it being a file.
// The validator fills a schedule's gaps from it, and P1 renders it into the
// system prompt so the model resolves "periodically" and "in the morning" the
// same way the validator will. They agree by construction because they are
// handed the same loaded *Table, not because two implementations were kept in
// step by hand.
//
// This package knows nothing about the schedule types. internal/schedule
// imports it and not the other way round, so P1's prompt renderer can depend on
// the table alone.
package defaults

import (
	"bytes"
	"fmt"
	"os"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/aidenpaleczny/navi/internal/domain"
)

// Table is the parsed file. The four blocks are the four in
// docs/05-schedule-spec.md#vocabulary-defaults, in that order.
type Table struct {
	// Frequency maps a phrase to a partial schedule. Entries are deliberately
	// partial — "a few times" fixes a count and says nothing about a period,
	// because the period comes from the rest of the sentence.
	Frequency map[string]Frequency `yaml:"frequency"`

	// Windows maps a phrase to a [start, end] pair of local HH:MM times. The
	// key "default" is the window a windowed or fuzzy schedule gets when it
	// names none, and Validate insists it is present.
	Windows map[string]Window `yaml:"windows"`

	// MinGapHours is the minimum spacing between two occurrences of one fuzzy
	// item, keyed by period. Validate insists all three periods are present.
	MinGapHours map[string]int `yaml:"min_gap_hours"`

	Defaults Defaults `yaml:"defaults"`
}

// Frequency is one row of the phrase table. Every field is optional, because
// the rows are partial by design.
type Frequency struct {
	Kind   string `yaml:"kind,omitempty"`
	Period string `yaml:"period,omitempty"`
	Count  int    `yaml:"count,omitempty"`
	RRule  string `yaml:"rrule,omitempty"`
}

// Window is a local time-of-day range, written in YAML as a two-element list.
type Window struct {
	Start string
	End   string
}

// UnmarshalYAML reads the ["09:00", "21:00"] form. The pair is a list in the
// file because that is how docs/05-schedule-spec.md writes it and how the
// schedule JSON carries it; it is a struct here so that Start and End are named
// at every use rather than being index 0 and index 1.
func (w *Window) UnmarshalYAML(node *yaml.Node) error {
	var pair []string
	if err := node.Decode(&pair); err != nil {
		return fmt.Errorf("defaults: window: %w", err)
	}
	if len(pair) != 2 {
		return fmt.Errorf("defaults: window must be two HH:MM times, got %d", len(pair))
	}
	w.Start, w.End = pair[0], pair[1]
	return nil
}

// Strings renders the window in the form the schedule JSON carries.
func (w Window) Strings() []string { return []string{w.Start, w.End} }

// String renders the window for a log line or an inference.
func (w Window) String() string { return w.Start + "-" + w.End }

// Defaults is the item-level block. Four of its five fields restate values that
// also live as DDL defaults and as constants in internal/domain; Table.Validate
// is what stops the two copies from drifting.
type Defaults struct {
	DaysAllowed  []string `yaml:"days_allowed"`
	Priority     int      `yaml:"priority"`
	NotifyPolicy string   `yaml:"notify_policy"`
	TZMode       string   `yaml:"tz_mode"`
	SnoozeCap    int      `yaml:"snooze_cap"`
}

// Load reads and validates the table. It is called once, from main, and the
// resulting value is passed to whatever needs it — this is get_defaults() from
// docs/06-agent-spec.md, and the caching decision lives here so that no caller
// has to know whether the file is re-read.
//
// It is not re-read. Hot-editing the vocabulary is not a requirement, whereas
// hot-editing persona.md is a P5 exit criterion (G5), so get_persona() will
// make the opposite choice in the same package when it lands.
func Load(path string) (*Table, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("defaults: read %s: %w", path, err)
	}

	var t Table
	dec := yaml.NewDecoder(bytes.NewReader(data))
	// A misspelled key is a silently absent table entry, which surfaces as a
	// schedule quietly missing a default rather than as an error.
	dec.KnownFields(true)
	if err := dec.Decode(&t); err != nil {
		return nil, fmt.Errorf("defaults: parse %s: %w", path, err)
	}

	if err := t.Validate(); err != nil {
		return nil, fmt.Errorf("defaults: %s: %w", path, err)
	}
	return &t, nil
}

// Validate checks the file is usable before anything reads it. A malformed
// table is a startup failure rather than a surprise at the first schedule,
// because the failure it prevents — a fuzzy item silently placed with no
// minimum gap — is invisible until the calendar looks wrong.
//
// It checks the file's own shape and its agreement with the schema defaults.
// The schedule vocabulary in it — that the windows are HH:MM times in the right
// order and the days are weekday tokens — is checked by schedule.CheckTable,
// which owns those rules and their wording. main calls both.
func (t *Table) Validate() error {
	if _, ok := t.Windows[WindowDefault]; !ok {
		return fmt.Errorf("windows: %q is required, it is the window every unspecified schedule gets", WindowDefault)
	}
	for name, w := range t.Windows {
		if w.Start == "" || w.End == "" {
			return fmt.Errorf("windows: %q has an empty bound", name)
		}
	}

	for _, period := range []string{"day", "week", "month"} {
		gap, ok := t.MinGapHours[period]
		if !ok {
			return fmt.Errorf("min_gap_hours: %s is missing", period)
		}
		if gap < 0 {
			return fmt.Errorf("min_gap_hours: %s is %d, which is negative", period, gap)
		}
	}

	if len(t.Defaults.DaysAllowed) == 0 {
		return fmt.Errorf("defaults.days_allowed is empty, so a fuzzy schedule would have no day to land on")
	}

	// The four values below exist twice: here, and as the DDL defaults that
	// internal/domain mirrors. Two copies is what D-016 is trying to avoid, and
	// the honest fix at this size is not a third abstraction but an assertion
	// that runs before anything reads either one.
	if t.Defaults.Priority != domain.DefaultPriority {
		return fmt.Errorf("defaults.priority is %d but the schema default is %d; they must agree",
			t.Defaults.Priority, domain.DefaultPriority)
	}
	if t.Defaults.SnoozeCap != domain.DefaultSnoozeCap {
		return fmt.Errorf("defaults.snooze_cap is %d but the schema default is %d; they must agree",
			t.Defaults.SnoozeCap, domain.DefaultSnoozeCap)
	}
	if domain.NotifyPolicy(t.Defaults.NotifyPolicy) != domain.NotifyAtTime {
		return fmt.Errorf("defaults.notify_policy is %q but the schema default is %q; they must agree",
			t.Defaults.NotifyPolicy, domain.NotifyAtTime)
	}
	if domain.TZMode(t.Defaults.TZMode) != domain.TZModeFloating {
		return fmt.Errorf("defaults.tz_mode is %q but the schema default is %q; they must agree",
			t.Defaults.TZMode, domain.TZModeFloating)
	}
	return nil
}

// WindowDefault is the key of the window applied when a schedule names none.
const WindowDefault = "default"

// LookupFrequency returns the frequency row for a phrase, matched
// case-insensitively after trimming, and whether one exists.
//
// Unused this session: the model resolves phrasing, and the validator only ever
// sees the resolved shape. It exists because the phrase table is half of what
// D-016 promised, and P1 renders and consults it from here rather than from a
// second copy in a prompt string.
func (t *Table) LookupFrequency(phrase string) (Frequency, bool) {
	f, ok := t.Frequency[normalize(phrase)]
	return f, ok
}

// NamedWindow returns the window for a phrase such as "morning".
func (t *Table) NamedWindow(phrase string) (Window, bool) {
	w, ok := t.Windows[normalize(phrase)]
	return w, ok
}

// DefaultWindow is the window a schedule gets when it names none.
func (t *Table) DefaultWindow() Window { return t.Windows[WindowDefault] }

// MinGap returns the minimum spacing for a period, and whether the period is
// one the table knows.
func (t *Table) MinGap(period string) (int, bool) {
	gap, ok := t.MinGapHours[period]
	return gap, ok
}

// DaysAllowed returns a copy of the default placement days, so a caller storing
// it on a schedule cannot alias the table.
func (t *Table) DaysAllowed() []string {
	out := make([]string, len(t.Defaults.DaysAllowed))
	copy(out, t.Defaults.DaysAllowed)
	return out
}

// Summary is the startup log line: enough to see which table loaded, without
// printing the whole file.
func (t *Table) Summary() string {
	return fmt.Sprintf("%d phrases, %d windows, default %s",
		len(t.Frequency), len(t.Windows), t.DefaultWindow())
}

func normalize(s string) string { return strings.ToLower(strings.TrimSpace(s)) }

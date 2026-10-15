package digest

import (
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
)

// The number check: V6 applied to prose. The digest's figures come from
// internal/stats, and the only way the digest and the statistics page can then
// disagree is for the model to restate a figure differently - round it, convert
// it, or make one up. So before an observation is accepted, every number in it
// must be a number that was in the figures it was given. One that is not sends
// the digest out without an observation, and the rejection is counted.
//
// A number is a quantity, found two ways:
//
//   - digits: "5", "83%", "2.0", "1,200". "83 percent" is the same quantity as
//     "83%". Times and dates fall out as their digit groups ("20:30" is 20 and
//     30), and "3rd" is 3.
//   - words: "five", "twenty-one", "twenty one", "two hundred", plus the
//     quantity adverbs "twice" (2), "thrice" (3), "double(d)" (2), "triple(d)"
//     (3), "dozen" (12) and "half" (50 percent).
//
// "one" is a pronoun about as often as it is a count ("no one", "one thing"), so
// it is a number only where a count noun follows it (days, weeks, misses, ...) or
// in "one in a row". Missing a "one" is a bounded hole - the claim it makes is
// a 1, and the figures block states every count it contains - and rejecting
// every "one thing" would reject nearly every observation.
//
// Matching is by value, not by spelling: "5" and "five" are the same quantity,
// and "2" matches "2.0". It is not by closeness. "83%" and "83.3%" are different
// quantities, and "about 80%" is a rounding of 83% that the page does not say, so
// both are refused: the entire point is that a rounded figure is a second figure.
// A percentage must match a percentage in the figures; a bare number may match
// any quantity with that value, so "5 of 6 (83%)" can be said as "five of six".
// "half" is a 50% and so needs a 50% in the figures, which is almost never true:
// the prompt asks for exact figures, and this is what enforces it.

// quantity is one number found in a text.
type quantity struct {
	value   float64
	percent bool
	text    string // as written, for the rejection message
}

var tokenRE = regexp.MustCompile(`\d+(?:,\d{3})*(?:\.\d+)?|[A-Za-z]+(?:-[A-Za-z]+)*|%`)

var unitWords = map[string]float64{
	"zero": 0, "one": 1, "two": 2, "three": 3, "four": 4, "five": 5, "six": 6, "seven": 7,
	"eight": 8, "nine": 9, "ten": 10, "eleven": 11, "twelve": 12, "thirteen": 13,
	"fourteen": 14, "fifteen": 15, "sixteen": 16, "seventeen": 17, "eighteen": 18, "nineteen": 19,
}

var tensWords = map[string]float64{
	"twenty": 20, "thirty": 30, "forty": 40, "fifty": 50,
	"sixty": 60, "seventy": 70, "eighty": 80, "ninety": 90,
}

// adverbs are quantities that are not spelled as a cardinal.
var adverbs = map[string]quantity{
	"twice": {value: 2}, "thrice": {value: 3}, "doubled": {value: 2}, "double": {value: 2},
	"tripled": {value: 3}, "triple": {value: 3}, "dozen": {value: 12},
	"half": {value: 50, percent: true},
}

// countNouns are what makes "one" a count.
var countNouns = map[string]bool{
	"day": true, "days": true, "week": true, "weeks": true, "time": true, "times": true,
	"miss": true, "misses": true, "missed": true, "skip": true, "skips": true, "skipped": true,
	"completion": true, "completions": true, "reminder": true, "reminders": true,
	"chain": true, "chains": true, "session": true, "sessions": true, "streak": true,
	"percent": true, "goal": true, "goals": true, "item": true, "items": true,
}

// extract returns every quantity in s, in order.
func extract(s string) []quantity {
	toks := tokenRE.FindAllString(s, -1)
	lower := make([]string, len(toks))
	for i, t := range toks {
		lower[i] = strings.ToLower(t)
	}

	var out []quantity
	for i := 0; i < len(toks); i++ {
		t := toks[i]
		switch {
		case t[0] >= '0' && t[0] <= '9':
			v, err := strconv.ParseFloat(strings.ReplaceAll(t, ",", ""), 64)
			if err != nil {
				continue
			}
			out = append(out, quantity{value: v, percent: percentFollows(lower, i+1), text: t})

		case t == "%":
			// consumed by the digits before it

		default:
			if q, ok := adverbs[lower[i]]; ok {
				q.text = t
				out = append(out, q)
				continue
			}
			v, used, ok := wordNumber(lower, i)
			if !ok {
				continue
			}
			q := quantity{value: v, text: strings.Join(toks[i:i+used], " "), percent: percentFollows(lower, i+used)}
			out = append(out, q)
			i += used - 1
		}
	}
	return out
}

// percentFollows reports whether the token at i says "percent".
func percentFollows(lower []string, i int) bool {
	if i >= len(lower) {
		return false
	}
	if lower[i] == "%" || lower[i] == "percent" || lower[i] == "pct" {
		return true
	}
	return lower[i] == "per" && i+1 < len(lower) && lower[i+1] == "cent"
}

// wordNumber parses a cardinal beginning at lower[i], returning its value and how
// many tokens it used. "twenty-one" is one token, "twenty one" two, and "two
// hundred" two.
func wordNumber(lower []string, i int) (value float64, used int, ok bool) {
	word := lower[i]

	// A hyphenated compound: tens-unit.
	if strings.Contains(word, "-") {
		parts := strings.Split(word, "-")
		if len(parts) == 2 {
			if tens, isTens := tensWords[parts[0]]; isTens {
				if unit, isUnit := unitWords[parts[1]]; isUnit && unit >= 1 && unit <= 9 {
					return hundreds(lower, i+1, tens+unit, 1)
				}
			}
		}
		return 0, 0, false
	}

	if tens, isTens := tensWords[word]; isTens {
		// "twenty one" as two words.
		if i+1 < len(lower) {
			if unit, isUnit := unitWords[lower[i+1]]; isUnit && unit >= 1 && unit <= 9 {
				return hundreds(lower, i+2, tens+unit, 2)
			}
		}
		return hundreds(lower, i+1, tens, 1)
	}

	if unit, isUnit := unitWords[word]; isUnit {
		if word == "one" && !oneIsCount(lower, i) {
			return 0, 0, false
		}
		return hundreds(lower, i+1, unit, 1)
	}

	if word == "hundred" {
		return 100, 1, true
	}
	return 0, 0, false
}

// hundreds folds a following "hundred" into a value that has consumed used
// tokens so far, so "two hundred" is 200.
func hundreds(lower []string, next int, value float64, used int) (float64, int, bool) {
	if next < len(lower) && lower[next] == "hundred" {
		return value * 100, used + 1, true
	}
	return value, used, true
}

func oneIsCount(lower []string, i int) bool {
	if i+1 >= len(lower) {
		return false
	}
	if countNouns[lower[i+1]] {
		return true
	}
	return i+3 < len(lower) && lower[i+1] == "in" && lower[i+2] == "a" && lower[i+3] == "row"
}

func sameValue(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

// InventedNumbersError names the numbers an observation used that the figures do
// not contain.
type InventedNumbersError struct {
	Numbers []string
}

func (e *InventedNumbersError) Error() string {
	return fmt.Sprintf("digest: observation uses numbers that are not in the figures: %s", strings.Join(e.Numbers, ", "))
}

// CheckNumbers returns nil when every quantity in observation is a quantity in
// figures (the exact text the model was given), and an *InventedNumbersError
// naming the ones that are not.
func CheckNumbers(figures, observation string) error {
	given := extract(figures)
	anyValue := func(v float64) bool {
		for _, g := range given {
			if sameValue(g.value, v) {
				return true
			}
		}
		return false
	}
	percentValue := func(v float64) bool {
		for _, g := range given {
			if g.percent && sameValue(g.value, v) {
				return true
			}
		}
		return false
	}

	var bad []string
	for _, q := range extract(observation) {
		ok := anyValue(q.value)
		if q.percent {
			ok = percentValue(q.value)
		}
		if !ok {
			bad = append(bad, q.text)
		}
	}
	if len(bad) > 0 {
		return &InventedNumbersError{Numbers: bad}
	}
	return nil
}

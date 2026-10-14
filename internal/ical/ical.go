// Package ical writes and reads the subset of RFC 5545 the calendar feed uses.
//
// It is hand-written rather than a dependency (D-021): the feed is output-only,
// uses a dozen properties, and the whole of the format that can go wrong is
// three rules - escape TEXT, fold at 75 octets without splitting a character,
// end every line in CRLF - which are about a hundred lines and are easier to
// audit here than inside a general-purpose library. The reader exists so
// cmd/naviseed can parse the feed back; it is not a validator for arbitrary
// input.
//
// It is a leaf: standard library only.
package ical

import (
	"bytes"
	"fmt"
	"strings"
	"unicode/utf8"
)

// MaxLineOctets is RFC 5545 section 3.1's limit on a content line, excluding the
// CRLF.
const MaxLineOctets = 75

// Param is one property parameter, such as TZID=America/Toronto.
type Param struct{ Name, Value string }

// Prop is one content line. Value is already in its wire form: TEXT values go
// through Text, which escapes, and everything else (dates, RRULEs, offsets) is
// passed as-is.
type Prop struct {
	Name   string
	Params []Param
	Value  string
}

// Component is a BEGIN/END block.
type Component struct {
	Name  string
	Props []Prop
	Subs  []*Component
}

// New starts a component.
func New(name string) *Component { return &Component{Name: name} }

// Add appends a property whose value is already wire-form.
func (c *Component) Add(name, value string, params ...Param) *Component {
	c.Props = append(c.Props, Prop{Name: name, Params: params, Value: value})
	return c
}

// AddText appends a TEXT property, escaping the value.
func (c *Component) AddText(name, value string, params ...Param) *Component {
	return c.Add(name, EscapeText(value), params...)
}

// Sub appends a nested component and returns it.
func (c *Component) Sub(name string) *Component {
	s := New(name)
	c.Subs = append(c.Subs, s)
	return s
}

// Get returns the first property with a name, case-insensitively.
func (c *Component) Get(name string) (Prop, bool) {
	for _, p := range c.Props {
		if strings.EqualFold(p.Name, name) {
			return p, true
		}
	}
	return Prop{}, false
}

// All returns every property with a name.
func (c *Component) All(name string) []Prop {
	var out []Prop
	for _, p := range c.Props {
		if strings.EqualFold(p.Name, name) {
			out = append(out, p)
		}
	}
	return out
}

// Param returns a parameter's value.
func (p Prop) Param(name string) string {
	for _, q := range p.Params {
		if strings.EqualFold(q.Name, name) {
			return q.Value
		}
	}
	return ""
}

// Clean makes a string safe to put in a TEXT value: invalid UTF-8 becomes
// U+FFFD, and every control character other than tab and newline is dropped
// (NUL, the C0 and C1 ranges, DEL), as are the line and paragraph separators
// and the byte-order mark. A title is user and model text, and one stray byte in
// it would otherwise break the whole feed for every subscriber.
func Clean(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch {
		case r == '\n' || r == '\t':
			b.WriteRune(r)
		case r == '\r':
			// folded into \n by EscapeText when it follows one; a bare CR is a line
			// break to everything that renders text.
			b.WriteRune('\n')
		case r < 0x20, r == 0x7f, r >= 0x80 && r <= 0x9f:
		case r == 0x2028, r == 0x2029, r == 0xfeff:
		default:
			b.WriteRune(r) // invalid UTF-8 already arrives here as U+FFFD
		}
	}
	return strings.ReplaceAll(b.String(), "\n\n\n", "\n\n")
}

// EscapeText is RFC 5545 section 3.3.11: backslash, semicolon and comma are
// escaped and a newline is written as \n. The value is cleaned first.
func EscapeText(s string) string {
	s = Clean(s)
	var b strings.Builder
	b.Grow(len(s) + 8)
	for _, r := range s {
		switch r {
		case '\\':
			b.WriteString(`\\`)
		case ';':
			b.WriteString(`\;`)
		case ',':
			b.WriteString(`\,`)
		case '\n':
			b.WriteString(`\n`)
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// UnescapeText reverses EscapeText.
func UnescapeText(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+1 < len(s) {
			i++
			switch s[i] {
			case 'n', 'N':
				b.WriteByte('\n')
			default:
				b.WriteByte(s[i])
			}
			continue
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// paramValue quotes a parameter value when it holds a character that would
// otherwise end it (section 3.2). A double quote cannot appear in a parameter at
// all, so it is dropped.
func paramValue(v string) string {
	v = strings.ReplaceAll(Clean(v), `"`, "")
	v = strings.ReplaceAll(v, "\n", " ")
	if strings.ContainsAny(v, ";:,") {
		return `"` + v + `"`
	}
	return v
}

// Encode renders a component tree with CRLF line endings and folded lines.
func Encode(c *Component) []byte {
	var b bytes.Buffer
	encode(&b, c)
	return b.Bytes()
}

func encode(b *bytes.Buffer, c *Component) {
	writeLine(b, "BEGIN:"+c.Name)
	for _, p := range c.Props {
		var l strings.Builder
		l.WriteString(p.Name)
		for _, q := range p.Params {
			l.WriteString(";" + q.Name + "=" + paramValue(q.Value))
		}
		l.WriteString(":")
		// A property value is wire-form already, but a raw newline in one would
		// inject a line, so it is defended here as well as in EscapeText.
		l.WriteString(strings.NewReplacer("\r", "", "\n", `\n`).Replace(p.Value))
		writeLine(b, l.String())
	}
	for _, s := range c.Subs {
		encode(b, s)
	}
	writeLine(b, "END:"+c.Name)
}

// writeLine folds at MaxLineOctets. Continuation lines start with one space,
// which counts against their 75, and a fold never lands inside a multi-byte
// character.
func writeLine(b *bytes.Buffer, line string) {
	limit := MaxLineOctets
	for len(line) > limit {
		cut := limit
		for cut > 0 && !utf8.RuneStart(line[cut]) {
			cut--
		}
		b.WriteString(line[:cut])
		b.WriteString("\r\n ")
		line = line[cut:]
		limit = MaxLineOctets - 1
	}
	b.WriteString(line)
	b.WriteString("\r\n")
}

// Parse reads a feed back. It unfolds lines, splits each into name, parameters
// and value, and nests components. It is strict about structure - a missing
// CRLF, an END that does not match, an unclosed component - because its job is
// to prove what Encode wrote.
func Parse(data []byte) (*Component, error) {
	if !bytes.HasSuffix(data, []byte("\r\n")) {
		return nil, fmt.Errorf("ical: input does not end in CRLF")
	}
	if bytes.Contains(bytes.ReplaceAll(data, []byte("\r\n"), nil), []byte("\n")) ||
		bytes.Contains(bytes.ReplaceAll(data, []byte("\r\n"), nil), []byte("\r")) {
		return nil, fmt.Errorf("ical: bare CR or LF in input")
	}
	if !utf8.Valid(data) {
		return nil, fmt.Errorf("ical: input is not valid UTF-8")
	}

	raw := strings.Split(strings.TrimSuffix(string(data), "\r\n"), "\r\n")
	var lines []string
	for i, l := range raw {
		if len(l) > MaxLineOctets {
			return nil, fmt.Errorf("ical: line %d is %d octets, over %d", i+1, len(l), MaxLineOctets)
		}
		if strings.HasPrefix(l, " ") || strings.HasPrefix(l, "\t") {
			if len(lines) == 0 {
				return nil, fmt.Errorf("ical: line %d continues nothing", i+1)
			}
			lines[len(lines)-1] += l[1:]
			continue
		}
		lines = append(lines, l)
	}

	var (
		stack []*Component
		root  *Component
	)
	for _, l := range lines {
		name, params, value, err := splitLine(l)
		if err != nil {
			return nil, err
		}
		switch strings.ToUpper(name) {
		case "BEGIN":
			c := New(value)
			if len(stack) == 0 {
				if root != nil {
					return nil, fmt.Errorf("ical: second top-level component %s", value)
				}
				root = c
			} else {
				top := stack[len(stack)-1]
				top.Subs = append(top.Subs, c)
			}
			stack = append(stack, c)
		case "END":
			if len(stack) == 0 || stack[len(stack)-1].Name != value {
				return nil, fmt.Errorf("ical: END:%s does not close the open component", value)
			}
			stack = stack[:len(stack)-1]
		default:
			if len(stack) == 0 {
				return nil, fmt.Errorf("ical: property %s outside a component", name)
			}
			top := stack[len(stack)-1]
			top.Props = append(top.Props, Prop{Name: name, Params: params, Value: value})
		}
	}
	if len(stack) != 0 || root == nil {
		return nil, fmt.Errorf("ical: unclosed component")
	}
	return root, nil
}

// splitLine separates name;params:value, honouring quoted parameter values.
func splitLine(l string) (name string, params []Param, value string, err error) {
	inQuote := false
	colon := -1
	for i := 0; i < len(l); i++ {
		switch l[i] {
		case '"':
			inQuote = !inQuote
		case ':':
			if !inQuote {
				colon = i
			}
		}
		if colon >= 0 {
			break
		}
	}
	if colon < 0 {
		return "", nil, "", fmt.Errorf("ical: no colon in line %q", l)
	}
	head, value := l[:colon], l[colon+1:]

	var parts []string
	start, inQuote := 0, false
	for i := 0; i < len(head); i++ {
		switch head[i] {
		case '"':
			inQuote = !inQuote
		case ';':
			if !inQuote {
				parts = append(parts, head[start:i])
				start = i + 1
			}
		}
	}
	parts = append(parts, head[start:])

	name = parts[0]
	for _, p := range parts[1:] {
		k, v, ok := strings.Cut(p, "=")
		if !ok {
			return "", nil, "", fmt.Errorf("ical: parameter %q has no value", p)
		}
		params = append(params, Param{Name: k, Value: strings.Trim(v, `"`)})
	}
	if name == "" {
		return "", nil, "", fmt.Errorf("ical: empty property name in %q", l)
	}
	return name, params, value, nil
}

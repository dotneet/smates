package config

// Editing smates.yml without rewriting it: the document is parsed with yaml.v3 to find
// where a key and its value are, and the change is spliced into the original text, so
// comments, blank lines, quoting and indentation everywhere else stay as they were. Every
// edit is checked by parsing the result again (Parse) before it is returned.

import (
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/bmatcuk/doublestar/v4"
	"gopkg.in/yaml.v3"
)

// ListKeys are the top-level lists AddToList can append to.
var ListKeys = []string{"ignore", "inputs"}

// ValidateRepoGlob checks a repo-relative glob for ignore or inputs. Backslashes are
// allowed: they escape glob metacharacters (`smates orphans` suggests such globs).
func ValidateRepoGlob(g string) error {
	switch {
	case strings.TrimSpace(g) == "":
		return errors.New("empty glob")
	case strings.HasPrefix(g, "/"), g == "..", strings.HasPrefix(g, "../"), strings.HasPrefix(g, "./"):
		return fmt.Errorf("%q must be relative to the repository root (no leading /, ./ or ../)", g)
	case !doublestar.ValidatePattern(g):
		return fmt.Errorf("bad glob %q", g)
	}
	return nil
}

// AddToList appends values to the top-level list key of the config document data
// (creating the key when it is missing or null) and returns the new document, the
// values it added and the values the list already had. Values already present, and
// repeated values, are added once. The result is validated with Parse.
func AddToList(data []byte, key string, values []string) (out []byte, added, present []string, err error) {
	if !slices.Contains(ListKeys, key) {
		return nil, nil, nil, fmt.Errorf("cannot add to %q (lists: %s)", key, strings.Join(ListKeys, ", "))
	}
	for _, v := range values {
		if err := ValidateRepoGlob(v); err != nil {
			return nil, nil, nil, fmt.Errorf("%s: %w", key, err)
		}
	}
	if _, err := Parse(data); err != nil {
		return nil, nil, nil, fmt.Errorf("the current config is invalid (fix it first): %w", err)
	}
	t, root, err := parseDoc(data)
	if err != nil {
		return nil, nil, nil, err
	}
	k, v := lookup(root, key)
	var have []string
	if v != nil && v.Kind == yaml.SequenceNode {
		for _, c := range v.Content {
			have = append(have, c.Value)
		}
	}
	for _, x := range values {
		switch {
		case slices.Contains(have, x):
			if !slices.Contains(present, x) {
				present = append(present, x)
			}
		case !slices.Contains(added, x):
			added = append(added, x)
		}
	}
	if len(added) == 0 {
		return data, nil, present, nil
	}
	quoted := make([]string, len(added))
	for i, x := range added {
		quoted[i] = strconv.Quote(x)
	}
	switch {
	case k == nil:
		var b strings.Builder
		b.WriteString(key + ":\n")
		for _, q := range quoted {
			b.WriteString("  - " + q + "\n")
		}
		out = t.appendBlock(root, b.String())
	case isNull(v):
		indent := strings.Repeat(" ", k.Column-1+2)
		var b strings.Builder
		for _, q := range quoted {
			b.WriteString(indent + "- " + q + "\n")
		}
		out, err = t.replaceNull(v, b.String())
	case v.Kind == yaml.SequenceNode && v.Style&yaml.FlowStyle != 0:
		out, err = t.appendFlow(v, strings.Join(quoted, ", "))
	case v.Kind == yaml.SequenceNode:
		indent := strings.Repeat(" ", v.Column-1)
		var b strings.Builder
		for _, q := range quoted {
			b.WriteString(indent + "- " + q + "\n")
		}
		at := t.blockEnd(v.Content[len(v.Content)-1].Line, v.Column-1)
		out = t.insertAfterLine(at, b.String())
	default:
		return nil, nil, nil, fmt.Errorf("%s is not a list (line %d)", key, v.Line)
	}
	if err != nil {
		return nil, nil, nil, err
	}
	c, err := Parse(out)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("the edited config does not parse (nothing written): %w", err)
	}
	got := c.Ignore
	if key == "inputs" {
		got = c.Inputs
	}
	for _, x := range added {
		if !slices.Contains(got, x) {
			return nil, nil, nil, fmt.Errorf("internal error: %s lacks %q after the edit (nothing written)", key, x)
		}
	}
	return out, added, present, nil
}

// SetScalar sets a scalar key of the config, given as a dotted path of YAML keys
// ("orphans", "state", "agent.model", "flaky.retry"), to value, and returns the new
// document and the previous value ("" when unset). The value is checked against the
// key's type and the result is validated with Parse.
func SetScalar(data []byte, key, value string) (out []byte, previous string, err error) {
	path := strings.Split(key, ".")
	typ, err := scalarField(path)
	if err != nil {
		return nil, "", err
	}
	text, err := renderScalar(typ, value)
	if err != nil {
		return nil, "", fmt.Errorf("%s: %w", key, err)
	}
	if _, err := Parse(data); err != nil {
		return nil, "", fmt.Errorf("the current config is invalid (fix it first): %w", err)
	}
	t, root, err := parseDoc(data)
	if err != nil {
		return nil, "", err
	}
	m := root
	for i, seg := range path {
		last := i == len(path)-1
		var k, v *yaml.Node
		if m != nil {
			k, v = lookup(m, seg)
		}
		if k == nil {
			// Create the remaining keys at the end of mapping m.
			indent := 0
			if m != nil && m != root {
				if m.Style&yaml.FlowStyle != 0 {
					return nil, "", fmt.Errorf("%s is a flow mapping ({...}): edit it by hand", strings.Join(path[:i], "."))
				}
				indent = m.Column - 1
			}
			block := nested(path[i:], indent, text)
			if m == nil || m == root {
				out = t.appendBlock(root, block)
			} else {
				lastKey := m.Content[len(m.Content)-2]
				out = t.insertAfterLine(t.blockEnd(lastKey.Line, indent), block)
			}
			break
		}
		if !last {
			switch {
			case isNull(v):
				out, err = t.replaceNull(v, nested(path[i+1:], k.Column-1+2, text))
			case v.Kind == yaml.MappingNode:
				m = v
				continue
			default:
				return nil, "", fmt.Errorf("%s is not a mapping (line %d)", strings.Join(path[:i+1], "."), v.Line)
			}
			break
		}
		if v.Kind != yaml.ScalarNode {
			return nil, "", fmt.Errorf("%s is not a scalar (line %d)", key, v.Line)
		}
		if !isNull(v) {
			previous = v.Value
		}
		out, err = t.replaceScalar(k, v, text)
		break
	}
	if err != nil {
		return nil, "", err
	}
	if _, err := Parse(out); err != nil {
		return nil, "", fmt.Errorf("the edited config is invalid (nothing written): %w", err)
	}
	_, root2, err := parseDoc(out)
	if err != nil {
		return nil, "", err
	}
	n := root2
	for _, seg := range path {
		if _, n = lookup(n, seg); n == nil {
			break
		}
	}
	if n == nil || n.Value != value {
		return nil, "", fmt.Errorf("internal error: %s is not %q after the edit (nothing written)", key, value)
	}
	return out, previous, nil
}

// ScalarKeys lists the keys SetScalar accepts.
func ScalarKeys() []string {
	var out []string
	var walk func(t reflect.Type, prefix string)
	walk = func(t reflect.Type, prefix string) {
		for i := 0; i < t.NumField(); i++ {
			f := t.Field(i)
			tag := strings.Split(f.Tag.Get("yaml"), ",")[0]
			if tag == "" || tag == "-" {
				continue
			}
			if f.Type.Kind() == reflect.Struct && f.Type != durationType {
				walk(f.Type, prefix+tag+".")
			} else if isScalarType(f.Type) {
				out = append(out, prefix+tag)
			}
		}
	}
	walk(reflect.TypeOf(Config{}), "")
	return out
}

var durationType = reflect.TypeOf(time.Duration(0))

func isScalarType(t reflect.Type) bool {
	if t == durationType {
		return true
	}
	if t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	switch t.Kind() {
	case reflect.String, reflect.Bool, reflect.Int, reflect.Int64:
		return true
	}
	return false
}

// scalarField returns the Go type of the config field at a dotted YAML path.
func scalarField(path []string) (reflect.Type, error) {
	t := reflect.TypeOf(Config{})
	for i, seg := range path {
		found := false
		if t.Kind() == reflect.Struct && t != durationType {
			for j := 0; j < t.NumField(); j++ {
				f := t.Field(j)
				if tag := strings.Split(f.Tag.Get("yaml"), ",")[0]; tag == seg && tag != "-" {
					t, found = f.Type, true
					break
				}
			}
		}
		if !found {
			return nil, fmt.Errorf("unknown key %q (settable: %s)", strings.Join(path[:i+1], "."), strings.Join(ScalarKeys(), ", "))
		}
	}
	if !isScalarType(t) {
		hint := ""
		if t.Kind() == reflect.Slice && len(path) == 1 && slices.Contains(ListKeys, path[0]) {
			hint = fmt.Sprintf("; use `smates config add-%s`", strings.TrimSuffix(path[0], "s"))
		}
		return nil, fmt.Errorf("%s is not a scalar key%s (settable: %s)", strings.Join(path, "."), hint, strings.Join(ScalarKeys(), ", "))
	}
	return t, nil
}

// renderScalar checks value against t and returns its YAML text.
func renderScalar(t reflect.Type, value string) (string, error) {
	if t == durationType {
		if _, err := time.ParseDuration(value); err != nil {
			return "", fmt.Errorf("want a duration such as 90s or 2m (got %q)", value)
		}
		return value, nil
	}
	if t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	switch t.Kind() {
	case reflect.Bool:
		if value != "true" && value != "false" {
			return "", fmt.Errorf("want true or false (got %q)", value)
		}
		return value, nil
	case reflect.Int, reflect.Int64:
		if _, err := strconv.Atoi(value); err != nil {
			return "", fmt.Errorf("want an integer (got %q)", value)
		}
		return value, nil
	}
	b, err := yaml.Marshal(value)
	if err != nil {
		return "", err
	}
	s := strings.TrimSpace(string(b))
	if strings.Contains(s, "\n") { // a multi-line string: keep it on one line
		s = strconv.Quote(value)
	}
	return s, nil
}

// nested renders keys as nested block mappings at indent, the last one set to value.
func nested(keys []string, indent int, value string) string {
	var b strings.Builder
	for i, k := range keys {
		b.WriteString(strings.Repeat(" ", indent+2*i) + k + ":")
		if i == len(keys)-1 {
			b.WriteString(" " + value)
		}
		b.WriteString("\n")
	}
	return b.String()
}

func isNull(n *yaml.Node) bool {
	return n.Kind == yaml.ScalarNode && n.Tag == "!!null"
}

func lookup(m *yaml.Node, key string) (k, v *yaml.Node) {
	if m == nil || m.Kind != yaml.MappingNode {
		return nil, nil
	}
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return m.Content[i], m.Content[i+1]
		}
	}
	return nil, nil
}

// text is a document with its line offsets.
type text struct {
	data  []byte
	start []int // byte offset of each line (line n starts at start[n-1])
}

// parseDoc parses data and returns its top-level mapping (nil for an empty document).
func parseDoc(data []byte) (*text, *yaml.Node, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, nil, err
	}
	t := &text{data: data, start: []int{0}}
	for i, c := range data {
		if c == '\n' {
			t.start = append(t.start, i+1)
		}
	}
	if len(doc.Content) == 0 {
		return t, nil, nil
	}
	root := doc.Content[0]
	if isNull(root) {
		return t, nil, nil
	}
	if root.Kind != yaml.MappingNode {
		return nil, nil, errors.New("the config is not a mapping")
	}
	if root.Style&yaml.FlowStyle != 0 || root.Column != 1 {
		return nil, nil, errors.New("the top-level mapping is in flow style or indented: edit it by hand")
	}
	return t, root, nil
}

// offset converts a 1-based line and column (in characters) to a byte offset.
func (t *text) offset(line, col int) int {
	o := t.start[line-1]
	for i := 1; i < col && o < len(t.data); i++ {
		_, n := utf8.DecodeRune(t.data[o:])
		o += n
	}
	return o
}

// lineEnd is the offset of the newline ending line (len(data) for the last line
// without one).
func (t *text) lineEnd(line int) int {
	if line < len(t.start) {
		return t.start[line] - 1
	}
	return len(t.data)
}

func (t *text) lines() int {
	n := len(t.start)
	if n > 1 && t.start[n-1] == len(t.data) {
		n-- // the empty "line" after a final newline
	}
	return n
}

func (t *text) lineText(line int) string {
	return string(t.data[t.start[line-1]:t.lineEnd(line)])
}

// blockEnd returns the last line of the block entry starting at line: following lines
// indented by more than indent belong to it (blank lines in between too).
func (t *text) blockEnd(line, indent int) int {
	end := line
	for l := line + 1; l <= t.lines(); l++ {
		s := t.lineText(l)
		trimmed := strings.TrimLeft(s, " ")
		if strings.TrimSpace(s) == "" {
			continue
		}
		if len(s)-len(trimmed) <= indent {
			break
		}
		end = l
	}
	return end
}

// insertAfterLine inserts block (whole lines) after line.
func (t *text) insertAfterLine(line int, block string) []byte {
	at := t.lineEnd(line)
	var b []byte
	b = append(b, t.data[:at]...)
	b = append(b, '\n')
	b = append(b, strings.TrimSuffix(block, "\n")...)
	b = append(b, t.data[at:]...)
	if at == len(t.data) {
		b = append(b, '\n')
	}
	return b
}

// appendBlock appends top-level lines to the document.
func (t *text) appendBlock(root *yaml.Node, block string) []byte {
	b := append([]byte{}, t.data...)
	if len(b) > 0 && b[len(b)-1] != '\n' {
		b = append(b, '\n')
	}
	return append(b, block...)
}

// replaceNull replaces a null value (empty, ~ or null) with block, written on the
// lines after the key; a comment after the value stays on the key's line.
func (t *text) replaceNull(v *yaml.Node, block string) ([]byte, error) {
	from := t.offset(v.Line, v.Column)
	to := from + len(v.Value)
	end := t.lineEnd(v.Line)
	if string(t.data[from:to]) != v.Value {
		return nil, fmt.Errorf("line %d: cannot locate the value: edit it by hand", v.Line)
	}
	head := strings.TrimRight(string(t.data[:from]), " \t")
	rest := strings.TrimRight(string(t.data[to:end]), " \t")
	if rest != "" && !strings.HasPrefix(strings.TrimLeft(rest, " \t"), "#") {
		return nil, fmt.Errorf("line %d: unexpected text after the value: edit it by hand", v.Line)
	}
	var b strings.Builder
	b.WriteString(head + rest + "\n" + block)
	if end < len(t.data) {
		b.Write(t.data[end+1:])
	}
	return []byte(b.String()), nil
}

// replaceScalar replaces the scalar value v of key k with text.
func (t *text) replaceScalar(k, v *yaml.Node, val string) ([]byte, error) {
	if isNull(v) && v.Value == "" {
		at := t.offset(v.Line, v.Column)
		return []byte(strings.TrimRight(string(t.data[:at]), " \t") + " " + val + string(t.data[at:])), nil
	}
	if v.Style&(yaml.LiteralStyle|yaml.FoldedStyle) != 0 {
		return nil, fmt.Errorf("line %d: block scalar: edit it by hand", v.Line)
	}
	from := t.offset(v.Line, v.Column)
	end := t.lineEnd(v.Line)
	line := string(t.data[from:end])
	n := 0
	switch {
	case v.Style&yaml.DoubleQuotedStyle != 0:
		n = quotedLen(line, '"')
	case v.Style&yaml.SingleQuotedStyle != 0:
		n = quotedLen(line, '\'')
	default:
		n = len(line)
		if i := strings.Index(line, " #"); i >= 0 {
			n = i
		}
		n = len(strings.TrimRight(line[:n], " \t"))
	}
	if n <= 0 || t.blockEnd(v.Line, k.Column-1) != v.Line {
		return nil, fmt.Errorf("line %d: multi-line value: edit it by hand", v.Line)
	}
	return []byte(string(t.data[:from]) + val + string(t.data[from+n:])), nil
}

// quotedLen returns the length of the quoted scalar starting s (0 when it does not end
// on this line).
func quotedLen(s string, q byte) int {
	for i := 1; i < len(s); i++ {
		switch {
		case q == '"' && s[i] == '\\':
			i++
		case s[i] == q && q == '\'' && i+1 < len(s) && s[i+1] == '\'':
			i++
		case s[i] == q:
			return i + 1
		}
	}
	return 0
}

// appendFlow adds items to the flow sequence v ([...]).
func (t *text) appendFlow(v *yaml.Node, items string) ([]byte, error) {
	from := t.offset(v.Line, v.Column)
	if from >= len(t.data) || t.data[from] != '[' {
		return nil, fmt.Errorf("line %d: cannot locate the list: edit it by hand", v.Line)
	}
	depth, end := 0, -1
	for i := from; i < len(t.data) && end < 0; i++ {
		switch c := t.data[i]; c {
		case '"', '\'':
			n := quotedLen(string(t.data[i:]), c)
			if n == 0 {
				return nil, fmt.Errorf("line %d: cannot parse the list: edit it by hand", v.Line)
			}
			i += n - 1
		case '#':
			if i > 0 && (t.data[i-1] == ' ' || t.data[i-1] == '\t') {
				return nil, fmt.Errorf("line %d: comment inside the list: edit it by hand", v.Line)
			}
		case '[', '{':
			depth++
		case ']', '}':
			if depth--; depth == 0 {
				end = i
			}
		}
	}
	if end < 0 {
		return nil, fmt.Errorf("line %d: unterminated list", v.Line)
	}
	last := end - 1
	for last > from && strings.ContainsRune(" \t\r\n", rune(t.data[last])) {
		last--
	}
	sep := ", "
	switch t.data[last] {
	case '[':
		sep = ""
	case ',':
		sep = " "
	}
	return []byte(string(t.data[:last+1]) + sep + items + string(t.data[last+1:])), nil
}

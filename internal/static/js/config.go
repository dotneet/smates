package js

import (
	"bytes"
	"encoding/json"
	"strconv"
	"strings"
)

// Config files (jest.config.js, vitest.config.ts, ...) are read without running them:
// ConfigObject evaluates the exported object when it is written as literals. Values are
// the Go types encoding/json produces (string, float64, bool, nil, []any), *Object for
// objects, Regex for regular expression literals and Unknown for anything that would
// need evaluation (identifiers, calls, functions, concatenations).

// Object is an object literal. Spread is set when it contains a ...spread, whose keys
// are unknown: a key missing from Fields may then still be set.
type Object struct {
	Fields map[string]any
	Spread bool
}

// Get returns the value of key and whether it is known: a missing key is known to be
// absent unless the object has a spread.
func (o *Object) Get(key string) (any, bool) {
	if o == nil {
		return nil, true
	}
	v, ok := o.Fields[key]
	if !ok {
		return nil, !o.Spread
	}
	return v, true
}

// Unknown is an expression that is not a literal; it holds the expression's source.
type Unknown string

// Regex is a regular expression literal.
type Regex struct {
	Source, Flags string
}

// ParseJSONC decodes JSON with comments and trailing commas into v.
func ParseJSONC(b []byte, v any) error { return parseJSONC(b, v) }

// FromJSON converts a value decoded by encoding/json into the representation of
// ConfigObject (objects become *Object).
func FromJSON(v any) any {
	switch x := v.(type) {
	case map[string]any:
		o := &Object{Fields: map[string]any{}}
		for k, e := range x {
			o.Fields[k] = FromJSON(e)
		}
		return o
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = FromJSON(e)
		}
		return out
	}
	return v
}

// ConfigObject returns the value a config module exports: the operand of
// `module.exports =` or `export default`, unwrapped from a call such as
// defineConfig({...}) or defineWorkspace([...]) and resolved through a top-level
// `const name = ...` binding. ok is false when the module has no such export.
func ConfigObject(src []byte) (v any, ok bool) {
	l := lex(src)
	p := &cfgParser{parser: parser{src: src, toks: l.toks}}
	for i := 0; i < len(p.toks); i++ {
		switch {
		case p.ident(i, "module") && p.punct(i+1, '.') && p.ident(i+2, "exports") && p.punct(i+3, '=') && !p.punct(i+4, '='):
			return p.export(i + 4), true
		case p.ident(i, "export") && p.ident(i+1, "default"):
			return p.export(i + 2), true
		}
	}
	return nil, false
}

type cfgParser struct {
	parser
	depth int // bindings followed, to stop on cycles
}

// export evaluates the exported expression at token i.
func (p *cfgParser) export(i int) any {
	v, _ := p.value(i)
	return p.unwrap(v, i)
}

// unwrap follows a binding (an exported identifier) or a call wrapping a literal.
func (p *cfgParser) unwrap(v any, i int) any {
	u, ok := v.(Unknown)
	if !ok || p.depth > 8 {
		return v
	}
	p.depth++
	defer func() { p.depth-- }()
	t := p.at(i)
	// defineConfig({...}), defineConfig(() => ({...})), a.b({...})
	j := i
	for p.at(j).kind == tIdent && p.punct(j+1, '.') {
		j += 2
	}
	if p.at(j).kind == tIdent && p.punct(j+1, '(') {
		k := j + 2
		// () => ({...}); a function body (() => { ... }) is not followed.
		if p.punct(k, '(') && p.punct(k+1, ')') && p.punct(k+2, '=') && p.punct(k+3, '>') {
			if !p.punct(k+4, '(') {
				return v
			}
			k += 5
		}
		if p.punct(k, '{') || p.punct(k, '[') || p.at(k).kind == tIdent {
			a, _ := p.value(k)
			return p.unwrap(a, k)
		}
		return v
	}
	if t.kind == tIdent && string(u) == p.text0(i) {
		name := p.text0(i)
		for k := 0; k+2 < len(p.toks); k++ {
			if (p.ident(k, "const") || p.ident(k, "let") || p.ident(k, "var")) && p.ident(k+1, name) {
				m := k + 2
				if p.punct(m, ':') { // type annotation
					for m < len(p.toks) && !(p.punct(m, '=') && !p.punct(m+1, '=')) && !p.punct(m, ';') {
						m++
					}
				}
				if p.punct(m, '=') {
					b, _ := p.value(m + 1)
					return p.unwrap(b, m+1)
				}
			}
		}
	}
	return v
}

// text0 returns the raw text of token i.
func (p *cfgParser) text0(i int) string {
	t := p.at(i)
	return string(p.src[t.s:t.e])
}

// value parses the expression starting at token i and returns its value and the index
// of the token after it (a ',' or closing bracket at the same nesting level, or the end).
func (p *cfgParser) value(i int) (any, int) {
	start := i
	var v any
	t := p.at(i)
	switch {
	case p.punct(i, '{'):
		v, i = p.object(i + 1)
	case p.punct(i, '['):
		v, i = p.array(i + 1)
	case p.literal(i):
		v, i = p.text(i), i+1
	case t.kind == tRegex:
		raw := string(p.src[t.s:t.e])
		end := strings.LastIndexByte(raw, '/')
		v, i = Regex{Source: raw[1:end], Flags: raw[end+1:]}, i+1
	case t.kind == tNumber:
		f, err := strconv.ParseFloat(p.text0(i), 64)
		if err != nil {
			return p.unknown(start)
		}
		v, i = f, i+1
	case p.punct(i, '-') && p.at(i+1).kind == tNumber:
		f, err := strconv.ParseFloat(p.text0(i+1), 64)
		if err != nil {
			return p.unknown(start)
		}
		v, i = -f, i+2
	case p.ident(i, "true"), p.ident(i, "false"):
		v, i = p.ident(i, "true"), i+1
	case p.ident(i, "null"), p.ident(i, "undefined"):
		v, i = nil, i+1
	default:
		return p.unknown(start)
	}
	// TypeScript `as const` / `satisfies T` keep the value; anything else (a + b,
	// x.concat(y), cond ? a : b) makes it an expression.
	if p.ident(i, "as") || p.ident(i, "satisfies") {
		return v, p.skip(i)
	}
	if !p.end(i) {
		return p.unknown(start)
	}
	return v, i
}

// end reports whether token i ends an expression inside an object or array literal or
// at the top level.
func (p *cfgParser) end(i int) bool {
	return i >= len(p.toks) || p.punct(i, ',') || p.punct(i, '}') || p.punct(i, ']') || p.punct(i, ')') || p.punct(i, ';')
}

// unknown skips the expression at i and returns it as Unknown.
func (p *cfgParser) unknown(i int) (any, int) {
	j := p.skip(i)
	if j <= i {
		return Unknown(""), i
	}
	start, end := p.at(i).s, len(p.src)
	if k := p.at(i).kind; k == tString || k == tTemplate {
		start-- // the opening quote
	}
	if j < len(p.toks) {
		end = p.toks[j].s
	}
	return Unknown(strings.TrimSpace(string(p.src[start:end]))), j
}

// skip returns the index of the first ',', ';' or unbalanced closing bracket at or
// after i at the nesting level of i.
func (p *cfgParser) skip(i int) int {
	depth := 0
	for ; i < len(p.toks); i++ {
		t := p.toks[i]
		if t.kind != tPunct {
			continue
		}
		switch t.c {
		case '{', '[', '(':
			depth++
		case '}', ']', ')':
			if depth == 0 {
				return i
			}
			depth--
		case ',', ';':
			if depth == 0 {
				return i
			}
		}
	}
	return i
}

// spread reports whether tokens i..i+2 are "...".
func (p *cfgParser) spread(i int) bool {
	return p.punct(i, '.') && p.punct(i+1, '.') && p.punct(i+2, '.')
}

// object parses object members from i (after '{') and returns the index after '}'.
func (p *cfgParser) object(i int) (*Object, int) {
	o := &Object{Fields: map[string]any{}}
	for i < len(p.toks) && !p.punct(i, '}') {
		if p.punct(i, ',') {
			i++
			continue
		}
		if p.spread(i) {
			o.Spread = true
			i = p.skip(i + 3)
			continue
		}
		key := ""
		t := p.at(i)
		switch {
		case t.kind == tIdent || t.kind == tNumber:
			key = p.text0(i)
		case p.literal(i):
			key = p.text(i)
		default:
			// Computed key ([k]: v) or something unexpected: skip the member.
			o.Spread = true
			i = p.skip(i)
			if p.punct(i, '[') || p.punct(i, ']') { // malformed; avoid looping
				i++
			}
			continue
		}
		switch {
		case p.punct(i+1, ':'):
			var v any
			v, i = p.value(i + 2)
			o.Fields[key] = v
		case p.end(i + 1):
			// Shorthand property {include}: the value of a binding.
			o.Fields[key] = Unknown(key)
			i++
		default:
			// Method, getter, async member: not a literal.
			o.Fields[key] = Unknown(key)
			i = p.skipMember(i)
		}
	}
	return o, i + 1
}

// skipMember skips a method definition (name(...) {...}) starting at i.
func (p *cfgParser) skipMember(i int) int {
	for i < len(p.toks) && !p.punct(i, '{') && !p.end(i) {
		if p.punct(i, '(') {
			i = p.skip(i+1) + 1
			continue
		}
		i++
	}
	if p.punct(i, '{') {
		i = p.skip(i+1) + 1
	}
	return i
}

// array parses array elements from i (after '[') and returns the index after ']'. A
// spread element is returned as Unknown("...expr").
func (p *cfgParser) array(i int) ([]any, int) {
	out := []any{}
	for i < len(p.toks) && !p.punct(i, ']') {
		if p.punct(i, ',') {
			i++
			continue
		}
		if p.spread(i) {
			v, j := p.unknown(i + 3)
			out = append(out, Unknown("..."+string(v.(Unknown))))
			i = j
			continue
		}
		var v any
		v, i = p.value(i)
		out = append(out, v)
		if !p.end(i) {
			i = p.skip(i)
		}
		if p.punct(i, '}') || p.punct(i, ')') { // malformed
			i++
		}
	}
	return out, i + 1
}

// DecodeJSONObject decodes a JSON object (with comments) into the ConfigObject form.
func DecodeJSONObject(b []byte) (*Object, error) {
	var m map[string]any
	d := json.NewDecoder(bytes.NewReader(stripJSONC(b)))
	if err := d.Decode(&m); err != nil {
		return nil, err
	}
	o, _ := FromJSON(m).(*Object)
	if o == nil {
		o = &Object{Fields: map[string]any{}}
	}
	return o, nil
}

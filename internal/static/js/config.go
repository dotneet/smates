package js

import (
	"bytes"
	"encoding/json"
	"path"
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

// ConfigObject returns the value a config module exports: the operand of a top-level
// `module.exports =` or `export default`, unwrapped from a single-argument call of a
// known config helper (defineConfig({...}), defineWorkspace([...]), see configWrappers)
// and resolved through a top-level `const name = ...` binding. ok is false when the
// module has no such export.
//
// The result is Unknown when evaluating it would need running the module: a call of any
// other function (mergeConfig(base, {...})), or an exported binding (or module.exports)
// that the module changes after its declaration, through a member (`config.testMatch =`,
// `config.roots.push(...)`, `config[k] =`, `module.exports.x =`, `Object.assign(config,
// ...)`) or by reassigning it.
func ConfigObject(src []byte) (v any, ok bool) {
	l := lex(src)
	p := &cfgParser{parser: parser{src: src, toks: l.toks}}
	p.levels()
	for i := 0; i < len(p.toks); i++ {
		if p.level[i] != 0 {
			continue
		}
		switch {
		case p.moduleExports(i) && p.assign(i+3):
			v = p.export(i + 4)
			if p.mutated("module.exports", i) {
				return Unknown("module.exports, changed later"), true
			}
			return v, true
		case p.ident(i, "export") && p.ident(i+1, "default"):
			return p.export(i + 2), true
		}
	}
	return nil, false
}

// configWrappers are the functions whose single argument is the configuration itself
// (vitest/vite: defineConfig, defineProject, defineWorkspace). Jest has no such helper.
var configWrappers = map[string]bool{"defineConfig": true, "defineProject": true, "defineWorkspace": true}

// mutatingMethods change the object or array they are called on.
var mutatingMethods = map[string]bool{
	"push": true, "unshift": true, "splice": true, "pop": true, "shift": true, "sort": true, "reverse": true,
	"fill": true, "copyWithin": true, "set": true, "add": true, "delete": true, "clear": true,
}

// objectMutators are the Object functions that change their first argument.
var objectMutators = map[string]bool{"assign": true, "defineProperty": true, "defineProperties": true, "setPrototypeOf": true}

type cfgParser struct {
	parser
	level []int // nesting depth of each token ((, [, {)
	depth int   // bindings followed, to stop on cycles
}

// levels computes the nesting depth of every token.
func (p *cfgParser) levels() {
	p.level = make([]int, len(p.toks))
	d := 0
	for i, t := range p.toks {
		if t.kind == tPunct && (t.c == ')' || t.c == ']' || t.c == '}') && d > 0 {
			d--
		}
		p.level[i] = d
		if t.kind == tPunct && (t.c == '(' || t.c == '[' || t.c == '{') {
			d++
		}
	}
}

// moduleExports reports whether tokens i..i+2 are `module.exports` (not a member of
// something else).
func (p *cfgParser) moduleExports(i int) bool {
	return p.ident(i, "module") && p.punct(i+1, '.') && p.ident(i+2, "exports") && !p.punct(i-1, '.')
}

// assign reports whether token i is a plain assignment `=` (not ==, ===, =>).
func (p *cfgParser) assign(i int) bool {
	return p.punct(i, '=') && !p.punct(i+1, '=') && !p.punct(i+1, '>') &&
		!(p.at(i-1).kind == tPunct && p.at(i-1).e == p.at(i).s && strings.IndexByte("=!<>", p.at(i-1).c) >= 0)
}

// export evaluates the exported expression at token i.
func (p *cfgParser) export(i int) any {
	v, _ := p.value(i)
	return p.unwrap(v, i)
}

// unwrap follows a binding (an exported identifier) or a known wrapper call.
func (p *cfgParser) unwrap(v any, i int) any {
	u, ok := v.(Unknown)
	if !ok || p.depth > 8 {
		return v
	}
	p.depth++
	defer func() { p.depth-- }()
	// defineConfig({...}), defineConfig(() => ({...})), vitest.defineConfig({...})
	j := i
	for p.at(j).kind == tIdent && p.punct(j+1, '.') {
		j += 2
	}
	if p.at(j).kind == tIdent && p.punct(j+1, '(') {
		if !configWrappers[p.text0(j)] {
			return v
		}
		k := j + 2
		arrow := false
		// () => ({...}); a function body (() => { ... }) is not followed.
		if p.punct(k, '(') && p.punct(k+1, ')') && p.punct(k+2, '=') && p.punct(k+3, '>') {
			if !p.punct(k+4, '(') {
				return v
			}
			k += 5
			arrow = true
		}
		if !p.punct(k, '{') && !p.punct(k, '[') && p.at(k).kind != tIdent {
			return v
		}
		a, next := p.value(k)
		if arrow {
			if !p.punct(next, ')') {
				return v
			}
			next++
		}
		if p.punct(next, ',') { // trailing comma
			next++
		}
		if !p.punct(next, ')') { // more than one argument
			return v
		}
		return p.unwrap(a, k)
	}
	if p.at(i).kind == tIdent && string(u) == p.text0(i) {
		name := p.text0(i)
		for k := 0; k+2 < len(p.toks); k++ {
			if p.level[k] != 0 || !(p.ident(k, "const") || p.ident(k, "let") || p.ident(k, "var")) || !p.ident(k+1, name) {
				continue
			}
			m := k + 2
			if p.punct(m, ':') { // type annotation
				for m < len(p.toks) && !p.assign(m) && !p.punct(m, ';') {
					m++
				}
			}
			if !p.assign(m) {
				continue
			}
			if p.mutated(name, k+1) {
				return Unknown(name + ", changed after its declaration")
			}
			b, _ := p.value(m + 1)
			return p.unwrap(b, m+1)
		}
	}
	return v
}

// mutated reports whether the module writes to the binding name ("module.exports" for
// the CommonJS export) anywhere other than at its declaration (token decl): assigning
// it again or one of its members, calling a mutating method on it or a member, deleting
// or incrementing a member, or passing it to Object.assign and the like. Writes through
// `exports.x =` count for module.exports. Shadowing bindings are not told apart: a false
// positive only makes the config unreadable, never wrong.
func (p *cfgParser) mutated(name string, decl int) bool {
	for i := 0; i < len(p.toks); i++ {
		if i == decl {
			continue
		}
		var k int // first token after the head of the reference
		switch {
		case name == "module.exports" && p.moduleExports(i):
			k = i + 3
		case name == "module.exports" && p.ident(i, "exports") && !p.punct(i-1, '.'):
			k = i + 1
			if !p.punct(k, '.') && !p.punct(k, '[') && !p.objectMutator(i) {
				continue // `exports` alone is only a write through a member
			}
		case name != "module.exports" && p.ident(i, name) && !p.punct(i-1, '.'):
			if prev := p.at(i - 1); prev.kind == tIdent && (p.ident(i-1, "const") || p.ident(i-1, "let") || p.ident(i-1, "var") ||
				p.ident(i-1, "function") || p.ident(i-1, "class") || p.ident(i-1, "import")) {
				continue
			}
			k = i + 1
		default:
			continue
		}
		if p.objectMutator(i) {
			return true
		}
		member, method := false, ""
		for {
			if p.punct(k, '.') && p.at(k+1).kind == tIdent && !p.punct(k+1, '.') {
				method = p.text0(k + 1)
				k += 2
			} else if p.punct(k, '?') && p.punct(k+1, '.') && p.at(k+2).kind == tIdent {
				method = p.text0(k + 2)
				k += 3
			} else if p.punct(k, '[') {
				method = ""
				k = p.skip(k+1) + 1
			} else {
				break
			}
			member = true
		}
		switch {
		case p.assign(k), p.compoundAssign(k):
			return true
		case member && (p.ident(i-1, "delete") || p.incDec(k) || p.incDec(i-2)):
			return true
		case member && method != "" && p.punct(k, '(') && mutatingMethods[method]:
			return true
		}
	}
	return false
}

// objectMutator reports whether the reference at token i is the first argument of
// Object.assign / Object.defineProperty / ... .
func (p *cfgParser) objectMutator(i int) bool {
	return p.punct(i-1, '(') && p.at(i-2).kind == tIdent && objectMutators[p.text0(i-2)] &&
		p.punct(i-3, '.') && p.ident(i-4, "Object")
}

// compoundAssign reports whether tokens from i are an operator assignment (+=, ||=,
// ??=, <<=, ...).
func (p *cfgParser) compoundAssign(i int) bool {
	j := i
	for j < i+3 && p.at(j).kind == tPunct && strings.IndexByte("+-*/%&|^?<>", p.at(j).c) >= 0 {
		j++
	}
	if j == i || !p.punct(j, '=') || p.punct(j+1, '=') {
		return false
	}
	// a <= b, a >= b are comparisons.
	if j == i+1 && (p.punct(i, '<') || p.punct(i, '>')) {
		return false
	}
	return true
}

// incDec reports whether tokens i, i+1 are ++ or --.
func (p *cfgParser) incDec(i int) bool {
	return (p.punct(i, '+') && p.punct(i+1, '+')) || (p.punct(i, '-') && p.punct(i+1, '-'))
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
	if !p.end(i) && !p.newStatement(start, i) {
		return p.unknown(start)
	}
	return v, i
}

// newStatement reports whether token i, after a value that starts at token start at
// the top level, begins the next statement of code written without semicolons: an
// identifier or keyword on a line of its own (const config = {...}<newline>export
// default config).
func (p *cfgParser) newStatement(start, i int) bool {
	if p.level == nil || start >= len(p.level) || p.level[start] != 0 || i >= len(p.toks) || p.toks[i].kind != tIdent {
		return false
	}
	return bytes.IndexByte(p.src[p.toks[i-1].e:p.toks[i].s], '\n') >= 0
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
			// Computed key ([k]: v) or something unexpected (a stray ';' or ')'): skip
			// the member. Keys after it may have been missed, so none reads as absent.
			o.Spread = true
			if j := p.skip(i); j > i {
				i = j
			} else {
				i++ // malformed; avoid looping
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

// skipMember skips a method definition (name(...) {...}) starting at i. Parameters and
// body are skipped as balanced brackets: the ',' and ';' inside them do not end the
// member.
func (p *cfgParser) skipMember(i int) int {
	start := i
	for i < len(p.toks) && !p.end(i) {
		switch {
		case p.punct(i, '{'):
			return p.closing(i)
		case p.punct(i, '('), p.punct(i, '['):
			i = p.closing(i)
		default:
			i++
		}
	}
	return max(i, start+1)
}

// closing returns the index after the bracket closing the one at i (or the end).
func (p *cfgParser) closing(i int) int {
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
			if depth--; depth <= 0 {
				return i + 1
			}
		}
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
		start := i
		var v any
		v, i = p.value(i)
		out = append(out, v)
		if !p.end(i) {
			i = p.skip(i)
		}
		if i == start || p.punct(i, '}') || p.punct(i, ')') { // malformed (a stray ';' or ')')
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

// CallArgs returns the first argument of every call of the function name in
// JavaScript or TypeScript source, evaluated like the values of ConfigObject (an
// identifier is resolved through a top-level const binding; nil for a call without
// arguments): storybookTest({ configDir: ".storybook" }). A method of something else
// (x.name(...)) or a declaration (function name(...)) is not a call of it.
func CallArgs(src []byte, name string) []any {
	l := lex(src)
	p := &cfgParser{parser: parser{src: src, toks: l.toks}}
	p.levels()
	var out []any
	for i := range p.toks {
		if !p.ident(i, name) || p.punct(i-1, '.') || p.ident(i-1, "function") || !p.punct(i+1, '(') {
			continue
		}
		if p.punct(i+2, ')') {
			out = append(out, nil)
			continue
		}
		v, _ := p.value(i + 2)
		out = append(out, p.unwrap(v, i+2))
	}
	return out
}

// PathValue evaluates the source of an expression naming a path: a string literal
// (fromFile false), or path.join / path.resolve (or a bare join / resolve) of
// __dirname, import.meta.dirname or a binding named dirname followed by string
// literals, which is relative to the file (fromFile true): path.join(dirname,
// ".storybook") is ".storybook". ok is false for anything else, and for an absolute
// segment.
func PathValue(expr string) (p string, fromFile, ok bool) {
	src := []byte(expr)
	ps := parser{src: src, toks: lex(src).toks}
	n := len(ps.toks)
	if n == 1 && ps.literal(0) {
		return ps.text(0), false, true
	}
	i := 0
	if ps.ident(0, "path") && ps.punct(1, '.') {
		i = 2
	}
	if !(ps.ident(i, "join") || ps.ident(i, "resolve")) || !ps.punct(i+1, '(') || !ps.punct(n-1, ')') {
		return "", false, false
	}
	i += 2
	switch {
	case ps.ident(i, "__dirname"), ps.ident(i, "dirname"):
		i++
	case ps.ident(i, "import") && ps.punct(i+1, '.') && ps.ident(i+2, "meta") && ps.punct(i+3, '.') && ps.ident(i+4, "dirname"):
		i += 5
	default:
		return "", false, false
	}
	segs := []string{"."}
	for ; i < n-1; i += 2 {
		if !ps.punct(i, ',') {
			return "", false, false
		}
		if i+1 == n-1 {
			break // trailing comma
		}
		if !ps.literal(i+1) || strings.HasPrefix(ps.text(i+1), "/") {
			return "", false, false
		}
		segs = append(segs, ps.text(i+1))
	}
	return path.Join(segs...), true, true
}

// StringLiterals returns the values of the string literals (and template literals
// without substitutions) in JavaScript or TypeScript source, in order, comments left
// out: the paths a configuration names (setupFiles: ["./test/setup.ts"]).
func StringLiterals(src []byte) []string {
	l := lex(src)
	p := parser{src: src, toks: l.toks}
	var out []string
	for i := range p.toks {
		if p.literal(i) {
			out = append(out, p.text(i))
		}
	}
	return out
}

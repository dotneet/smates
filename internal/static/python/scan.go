package python

import "strings"

// tokKind classifies lexer tokens.
type tokKind uint8

const (
	tokName tokKind = iota + 1
	tokNumber
	tokString
	tokOp
	tokNewline // end of a logical line
)

// token is one lexical token. Name and string texts are substrings of the source, so
// lexing allocates only the token slice.
type token struct {
	kind tokKind
	op   byte // tokOp: the operator byte
	// top reports that the token was emitted outside any bracket. A ';' or ':' at the
	// top level separates statements.
	top bool
	// partial (tokString) reports that text is only the static prefix of an f-string
	// that contains replacement fields.
	partial bool
	text    string // tokName: the identifier; tokString: the raw literal content
}

// lexer is a single-pass, allocation-light Python tokenizer. It knows exactly as much
// of the grammar as is needed to find import statements reliably: comments, all string
// literal forms (prefixes, triple quotes, PEP 701 f-strings with nested quotes),
// explicit and implicit line joining, and bracket depth. Indentation is ignored.
type lexer struct {
	s     string
	toks  []token
	depth int
}

// lex tokenizes Python source.
func lex(s string) []token {
	lx := lexer{s: s, toks: make([]token, 0, len(s)/6+1)}
	lx.run()
	return lx.toks
}

func (lx *lexer) emit(t token) {
	t.top = lx.depth == 0
	lx.toks = append(lx.toks, t)
}

func (lx *lexer) newline() {
	if n := len(lx.toks); n > 0 && lx.toks[n-1].kind != tokNewline {
		lx.toks = append(lx.toks, token{kind: tokNewline, top: true})
	}
}

// maxUnicodeName bounds the length of a \N{NAME} escape.
const maxUnicodeName = 128

// bom is the UTF-8 byte order mark.
const bom = "\xef\xbb\xbf"

func (lx *lexer) run() {
	s := lx.s
	i := 0
	if strings.HasPrefix(s, bom) {
		i = len(bom)
	}
	lineStart := true
	for i < len(s) {
		c := s[i]
		switch c {
		case '\n':
			i++
			if lx.depth == 0 {
				lx.newline()
			}
			lineStart = true
			continue
		case ' ', '\t', '\r', '\f', '\v':
			i++
			continue
		case '#':
			if k := strings.IndexByte(s[i:], '\n'); k >= 0 {
				i += k
			} else {
				i = len(s)
			}
			continue
		case '\\':
			j := i + 1
			if j < len(s) && s[j] == '\r' {
				j++
			}
			if j < len(s) && s[j] == '\n' {
				i = j + 1 // explicit line joining: no newline, still the same line
				continue
			}
			i++
			continue
		}
		first := lineStart
		lineStart = false
		switch {
		case isIdentStart(c):
			j := i + 1
			for j < len(s) && isIdentChar(s[j]) {
				j++
			}
			word := s[i:j]
			if j < len(s) && (s[j] == '"' || s[j] == '\'') && isStringPrefix(word) {
				text, partial, end := scanString(s, j, word)
				lx.emit(token{kind: tokString, text: text, partial: partial})
				i = end
				continue
			}
			if first && lx.depth > 0 && (word == "import" || word == "from") {
				// "import" can never appear inside brackets and a line starting with
				// "from" inside brackets is all but unheard of: the brackets are
				// unbalanced (broken or truncated source), so recover here rather than
				// losing every import in the rest of the file.
				lx.depth = 0
				lx.newline()
			}
			lx.emit(token{kind: tokName, text: word})
			i = j
		case isDigit(c) || c == '.' && i+1 < len(s) && isDigit(s[i+1]):
			j := i + 1
			for j < len(s) && (isIdentChar(s[j]) || s[j] == '.') {
				j++
			}
			lx.emit(token{kind: tokNumber})
			i = j
		case c == '"' || c == '\'':
			text, partial, end := scanString(s, i, "")
			lx.emit(token{kind: tokString, text: text, partial: partial})
			i = end
		default:
			switch c {
			case '(', '[', '{':
				lx.emit(token{kind: tokOp, op: c})
				lx.depth++
			case ')', ']', '}':
				if lx.depth > 0 {
					lx.depth--
				}
				lx.emit(token{kind: tokOp, op: c})
			default:
				lx.emit(token{kind: tokOp, op: c})
			}
			i++
		}
	}
	lx.newline()
}

// scanString scans a string literal whose opening quote is at s[q] and returns its
// raw content, whether the content is only the static prefix of a formatted string,
// and the index just past the literal. An unterminated single-quoted literal ends at
// the end of its line so that damage from malformed input stays local.
func scanString(s string, q int, prefix string) (text string, partial bool, end int) {
	var raw, format bool
	for k := 0; k < len(prefix); k++ {
		switch prefix[k] | 0x20 {
		case 'r':
			raw = true
		case 'f', 't':
			format = true
		}
	}
	quote := s[q]
	triple := q+2 < len(s) && s[q+1] == quote && s[q+2] == quote
	start := q + 1
	if triple {
		start = q + 3
	}
	staticEnd := -1
	content := func(stop int) string {
		if staticEnd >= 0 {
			return s[start:staticEnd]
		}
		return s[start:stop]
	}
	i := start
	for i < len(s) {
		c := s[i]
		switch {
		case c == '\\':
			if format && i+1 < len(s) {
				if nx := s[i+1]; nx == '{' || nx == '}' {
					i++ // the brace still opens/closes a replacement field
					continue
				}
				if !raw && s[i+1] == 'N' && i+2 < len(s) && s[i+2] == '{' {
					// Unicode names are short; bounding the search keeps lexing linear.
					if k := strings.IndexByte(s[i+3:min(len(s), i+3+maxUnicodeName)], '}'); k >= 0 {
						i += 3 + k + 1 // \N{NAME} escape, not a replacement field
						continue
					}
				}
			}
			i += 2
		case c == quote:
			if !triple {
				return content(i), staticEnd >= 0, i + 1
			}
			if i+2 < len(s) && s[i+1] == quote && s[i+2] == quote {
				return content(i), staticEnd >= 0, i + 3
			}
			i++
		case c == '\n' && !triple:
			return content(i), staticEnd >= 0, i
		case format && c == '{':
			if i+1 < len(s) && s[i+1] == '{' {
				i += 2
				continue
			}
			if staticEnd < 0 {
				staticEnd = i
			}
			i = skipField(s, i+1, quote, triple)
		default:
			i++
		}
	}
	return content(len(s)), staticEnd >= 0, len(s)
}

// skipField skips the expression part of an f-string replacement field starting at
// s[i] (just after '{') and returns the index after its closing '}'. Nested strings
// (including ones reusing the enclosing quote, PEP 701) and brackets are honored.
func skipField(s string, i int, quote byte, triple bool) int {
	depth := 0
	for i < len(s) {
		c := s[i]
		switch {
		case c == '"' || c == '\'':
			_, _, i = scanString(s, i, "")
		case isIdentStart(c):
			j := i + 1
			for j < len(s) && isIdentChar(s[j]) {
				j++
			}
			if j < len(s) && (s[j] == '"' || s[j] == '\'') && isStringPrefix(s[i:j]) {
				_, _, i = scanString(s, j, s[i:j])
				continue
			}
			i = j
		case c == '(' || c == '[' || c == '{':
			depth++
			i++
		case c == ')' || c == ']':
			if depth > 0 {
				depth--
			}
			i++
		case c == '}':
			if depth == 0 {
				return i + 1
			}
			depth--
			i++
		case c == ':' && depth == 0:
			return skipFormatSpec(s, i+1, quote, triple)
		case c == '\n' && !triple && depth == 0:
			return i
		case c == '#':
			if k := strings.IndexByte(s[i:], '\n'); k >= 0 {
				i += k
			} else {
				i = len(s)
			}
		default:
			i++
		}
	}
	return i
}

// skipFormatSpec skips a replacement field's format spec (after ':'), which is literal
// text with optional nested fields, and returns the index after the closing '}'.
func skipFormatSpec(s string, i int, quote byte, triple bool) int {
	for i < len(s) {
		switch c := s[i]; {
		case c == '{':
			i = skipField(s, i+1, quote, triple)
		case c == '}':
			return i + 1
		case c == quote, c == '\n' && !triple:
			return i // malformed; let the literal scanner deal with it
		case c == '\\':
			i += 2
		default:
			i++
		}
	}
	return i
}

func isIdentStart(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c == '_' || c >= 0x80
}

func isIdentChar(c byte) bool { return isIdentStart(c) || isDigit(c) }

func isDigit(c byte) bool { return c >= '0' && c <= '9' }

// isStringPrefix reports whether w is a string literal prefix (r, b, u, f, t and their
// two-letter combinations, any case).
func isStringPrefix(w string) bool {
	if len(w) == 0 || len(w) > 2 {
		return false
	}
	for k := 0; k < len(w); k++ {
		switch w[k] | 0x20 {
		case 'r', 'b', 'u', 'f', 't':
		default:
			return false
		}
	}
	return len(w) == 1 || w[0]|0x20 != w[1]|0x20
}

// isIdent reports whether s is a (non-empty) Python identifier.
func isIdent(s string) bool {
	if s == "" || !isIdentStart(s[0]) {
		return false
	}
	for k := 1; k < len(s); k++ {
		if !isIdentChar(s[k]) {
			return false
		}
	}
	return true
}

// isDotted reports whether s is a dotted name such as "a.b.c".
func isDotted(s string) bool {
	for {
		k := strings.IndexByte(s, '.')
		if k < 0 {
			return isIdent(s)
		}
		if !isIdent(s[:k]) {
			return false
		}
		s = s[k+1:]
	}
}

// modRef is a module reference found in a source file.
type modRef struct {
	// level is the number of leading dots of a relative reference (0: absolute).
	level int
	// name is the dotted module name without leading dots. It may be empty for
	// "from . import x". For a prefix reference it is the static prefix of the name.
	name string
	// names are the names a from-import binds; each may be a submodule or an attribute.
	names []string
	// pkg is the literal anchor package of a relative importlib.import_module call.
	pkg string
	// prefix marks a dynamic import whose name is only known up to name: every module
	// starting with that prefix may be imported.
	prefix bool
	// every resolves each dotted prefix of name as a module (mock.patch-style targets,
	// where it is unknown which part is the module and which are attributes).
	every bool
}

// scanned is what scan extracts from a source file.
type scanned struct {
	refs []modRef
	// all holds the string entries of __all__ (relevant in a package's __init__,
	// where "from pkg import *" imports listed submodules).
	all []string
}

// scan extracts module references from Python source.
func scan(src []byte) scanned {
	var out scanned
	if len(src) == 0 {
		return out
	}
	toks := lex(string(src))
	statements(toks, &out)
	dynamic(toks, &out)
	return out
}

// isBoundary reports whether t ends a simple statement. A top-level ':' is treated as
// one so that "if x: import y" and "try: import z" are seen; for other uses of a
// top-level ':' (annotations, lambdas) that is harmless.
func isBoundary(t token) bool {
	return t.kind == tokNewline || t.kind == tokOp && t.top && (t.op == ';' || t.op == ':')
}

func isOp(toks []token, i int, op byte) bool {
	return i < len(toks) && toks[i].kind == tokOp && toks[i].op == op
}

func isName(toks []token, i int, name string) bool {
	return i < len(toks) && toks[i].kind == tokName && toks[i].text == name
}

// statements finds import and from-import statements, wherever they are nested.
func statements(toks []token, out *scanned) {
	start := true
	for i := 0; i < len(toks); {
		t := toks[i]
		if isBoundary(t) {
			start = true
			i++
			continue
		}
		if start && t.kind == tokName {
			switch t.text {
			case "import":
				i = parseImport(toks, i+1, out)
				start = false
				continue
			case "from":
				i = parseFrom(toks, i+1, out)
				start = false
				continue
			}
		}
		start = false
		i++
	}
}

// parseDotted parses NAME ('.' NAME)* at toks[i].
func parseDotted(toks []token, i int) (string, int) {
	if i >= len(toks) || toks[i].kind != tokName {
		return "", i
	}
	name := toks[i].text
	i++
	for isOp(toks, i, '.') && i+1 < len(toks) && toks[i+1].kind == tokName {
		name += "." + toks[i+1].text
		i += 2
	}
	return name, i
}

// parseImport parses the rest of "import a.b as c, d".
func parseImport(toks []token, i int, out *scanned) int {
	for {
		name, j := parseDotted(toks, i)
		if name == "" {
			return j
		}
		out.refs = append(out.refs, modRef{name: name})
		i = j
		if isName(toks, i, "as") {
			i += 2
		}
		if !isOp(toks, i, ',') {
			return i
		}
		i++
	}
}

// parseFrom parses the rest of "from .mod import (a as b, c)" or "from x import *".
func parseFrom(toks []token, i int, out *scanned) int {
	level := 0
	for isOp(toks, i, '.') {
		level++
		i++
	}
	var name string
	if !isName(toks, i, "import") {
		name, i = parseDotted(toks, i)
	}
	if !isName(toks, i, "import") || level == 0 && name == "" {
		return i
	}
	i++
	ref := modRef{level: level, name: name}
	if isOp(toks, i, '*') {
		out.refs = append(out.refs, ref)
		return i + 1
	}
	paren := isOp(toks, i, '(')
	if paren {
		i++
	}
names:
	for i < len(toks) {
		switch t := toks[i]; {
		case t.kind == tokName:
			ref.names = append(ref.names, t.text)
			i++
			if isName(toks, i, "as") {
				i += 2
			}
		case t.kind == tokOp && t.op == ',':
			i++
		default:
			break names
		}
	}
	if paren && isOp(toks, i, ')') {
		i++
	}
	out.refs = append(out.refs, ref)
	return i
}

// dynamic finds imports expressed as calls or assignments with string literals:
// importlib.import_module / __import__ / pytest.importorskip, pytest_plugins,
// __all__, and mock.patch / monkeypatch.setattr targets.
func dynamic(toks []token, out *scanned) {
	for i, t := range toks {
		if t.kind != tokName {
			continue
		}
		switch t.text {
		case "import_module", "__import__", "importorskip":
			if ref, ok := dynamicImport(toks, i); ok {
				out.refs = append(out.refs, ref)
			}
		case "pytest_plugins":
			if j, ok := assignment(toks, i+1); ok {
				for _, name := range stringList(toks, j) {
					if isDotted(name) {
						out.refs = append(out.refs, modRef{name: name})
					}
				}
			}
		case "__all__":
			j, ok := assignment(toks, i+1)
			if !ok && isOp(toks, i+1, '.') && (isName(toks, i+2, "extend") || isName(toks, i+2, "append")) && isOp(toks, i+3, '(') {
				j, ok = i+3, true
			}
			if ok {
				out.all = append(out.all, stringList(toks, j)...)
			}
		case "patch", "setattr", "delattr":
			if isOp(toks, i+1, '(') && i+2 < len(toks) && toks[i+2].kind == tokString && !toks[i+2].partial {
				if name := toks[i+2].text; strings.IndexByte(name, '.') > 0 && isDotted(name) {
					out.refs = append(out.refs, modRef{name: name, every: true})
				}
			}
		}
	}
}

// dynamicImport parses import_module("x"), __import__("x") or importorskip("x") with
// the call name at toks[i]. A name built from a literal prefix (f-string, "+", "%",
// .format) yields a prefix reference.
func dynamicImport(toks []token, i int) (modRef, bool) {
	if !isOp(toks, i+1, '(') || i+2 >= len(toks) || toks[i+2].kind != tokString {
		return modRef{}, false
	}
	arg := toks[i+2]
	text, partial := arg.text, arg.partial
	if !partial && !isOp(toks, i+3, ',') && !isOp(toks, i+3, ')') {
		// "pkg.plugins." + name, "pkg.%s" % name, "pkg.{}".format(name), ...
		partial = true
		if k := strings.IndexAny(text, "{%"); k >= 0 {
			text = text[:k]
		}
	}
	level := 0
	for level < len(text) && text[level] == '.' {
		level++
	}
	text = text[level:]
	if level > 0 && toks[i].text != "import_module" {
		return modRef{}, false
	}
	ref := modRef{level: level, name: text, prefix: partial}
	if partial {
		if !validPrefix(text) || level == 0 && text == "" {
			return modRef{}, false
		}
	} else if !isDotted(text) && !(level > 0 && text == "") {
		return modRef{}, false
	}
	if level > 0 && isOp(toks, i+3, ',') {
		// import_module(".x", "pkg") or import_module(".x", package="pkg")
		j := i + 4
		if isName(toks, j, "package") && isOp(toks, j+1, '=') {
			j += 2
		}
		if j < len(toks) && toks[j].kind == tokString && !toks[j].partial && isDotted(toks[j].text) {
			ref.pkg = toks[j].text
		}
	}
	return ref, true
}

// validPrefix reports whether s can be the static prefix of a dotted module name: dot
// separated identifiers where the last one may be incomplete or empty.
func validPrefix(s string) bool {
	k := strings.LastIndexByte(s, '.')
	if k >= 0 && !isDotted(s[:k]) {
		return false
	}
	last := s[k+1:]
	return last == "" || isIdent(last)
}

// assignment reports whether toks[i:] is "= ...", "+= ..." or ": T = ..." and returns
// the index of the value.
func assignment(toks []token, i int) (int, bool) {
	if isOp(toks, i, ':') {
		// Annotated assignment: skip the annotation up to '='.
		for i++; i < len(toks) && !isBoundary(toks[i]); i++ {
			if isOp(toks, i, '=') {
				return i + 1, true
			}
		}
		return 0, false
	}
	if isOp(toks, i, '+') {
		i++
	}
	if isOp(toks, i, '=') && !isOp(toks, i+1, '=') {
		return i + 1, true
	}
	return 0, false
}

// stringList collects the literal strings of a value starting at toks[i]: a single
// string, a comma separated sequence of strings, or a bracketed list/tuple (whose
// non-literal elements are ignored).
func stringList(toks []token, i int) []string {
	var out []string
	depth := 0
	for ; i < len(toks); i++ {
		t := toks[i]
		switch {
		case t.kind == tokString:
			if !t.partial {
				out = append(out, t.text)
			}
		case t.kind == tokOp && (t.op == '(' || t.op == '['):
			depth++
		case t.kind == tokOp && (t.op == ')' || t.op == ']'):
			depth--
			if depth <= 0 {
				return out
			}
		case t.kind == tokOp && t.op == ',':
		default:
			if depth == 0 {
				return out
			}
		}
	}
	return out
}

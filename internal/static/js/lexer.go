package js

import (
	"bytes"
	"strconv"
	"strings"
	"unicode/utf8"
)

// tokKind classifies a lexical token. The lexer only distinguishes what the reference
// extractor needs; operators are emitted one byte at a time.
type tokKind uint8

const (
	tEOF tokKind = iota
	tPunct
	tIdent
	tNumber
	tString
	tTemplate
	tRegex
)

// token is a lexical token. Offsets index into the lexed source.
type token struct {
	kind tokKind
	c    byte // punctuator byte
	expr bool // template literal with at least one ${} substitution
	// s:e is the identifier or number text, the raw body of a string literal, or the
	// raw text of a template literal up to its first substitution (the whole body if it
	// has none).
	s, e int
	// ts:te is the raw text of a template literal after its last substitution.
	ts, te int
}

// slashMode says how a '/' at the current position is read.
type slashMode uint8

const (
	// slashRegex: a '/' starts a regular expression (after an operator, '(' or ',', a
	// keyword such as return, the ')' of an if/while/for/with head, the '}' of a block,
	// or at the start of input).
	slashRegex slashMode = iota
	// slashDiv: a '/' is a division (after an operand: identifier, number, literal, ')',
	// ']', postfix ++/--, or a TypeScript non-null assertion).
	slashDiv
	// slashMaybe: either reading is plausible (after the '}' of an object literal, '>',
	// or a contextual keyword such as await that may be an identifier). A regex is tried,
	// but only accepted if it looks like one; see regex.
	slashMaybe
)

// parenKind says what an open '(' started.
type parenKind uint8

const (
	parenOther parenKind = iota
	// parenHead: the head of if, while or with, after whose ')' a statement starts.
	parenHead
	// parenFor: the head of for (or for await), in which `of` is a keyword.
	parenFor
)

// braceKind says what an open '{' started.
type braceKind uint8

const (
	// braceObject: an object literal, an object type, or anything the lexer cannot
	// classify. After its '}' a '/' may divide.
	braceObject braceKind = iota
	// braceBlock: a statement block, function or class body. After its '}' a statement
	// starts, so a '/' starts a regular expression.
	braceBlock
	// braceJSX: a JSX expression container ({x} in a tag or among children). Its '}'
	// returns to the JSX element.
	braceJSX
)

// jsxState is where in a JSX element the lexer is.
type jsxState uint8

const (
	jsxTag   jsxState = iota // in the opening tag: name, attributes
	jsxText                  // among the children: text, child elements, {expressions}
	jsxClose                 // in the closing tag </name>
)

// jsxElem is an open JSX element (or fragment).
type jsxElem struct {
	// depth is the brace depth at which the element was opened: while it is the
	// current depth the lexer reads JSX, deeper it reads the code of a container.
	depth int
	state jsxState
}

// tmplFrame records an open ${ substitution: the template token it belongs to and the
// brace depth at which its closing } is expected.
type tmplFrame struct {
	tok   int
	depth int
}

// lexer is a small JavaScript/TypeScript tokenizer. It is not a validating parser: its
// job is to skip comments, strings, template literals, regular expressions and JSX text
// exactly enough that module specifiers are never taken from (or hidden by) them.
//
// Strings and regular expressions never span lines in valid code, so both stop at a line
// break. This bounds the damage of a mis-lexed construct (an apostrophe in JSX text, a
// division mistaken for a regex) to a single line. Template literals do span lines, so
// a backtick swallowed by (or exposed from) a mis-lexed regex would invert the template
// state for the rest of the file; the slash heuristics below are tuned to avoid that.
// Even so the lexer can be wrong: the analyzer adds what a coarse scan of the text finds
// (see coarseRefs), so that a mis-lexed file loses no specifier.
type lexer struct {
	src    []byte
	toks   []token
	refs   []ref // references found in comments (triple-slash directives)
	frames []tmplFrame
	depth  int
	slash  slashMode // how a '/' here is read
	// parens has one entry per open '(': what it opened.
	parens []parenKind
	// braces has one entry per open '{' (not ${): what it opened. closed is the kind
	// of the one the last '}' closed.
	braces []braceKind
	closed braceKind
	incdec int // index of the token completing the last ++ or -- (-1 if none)
	// jsx enables JSX: a '<' where an expression starts opens an element, whose text
	// is skipped. It is off for TypeScript (.ts), where <T>x is a type assertion.
	jsx   bool
	elems []jsxElem // open JSX elements, innermost last
	// skipped are the spans of src read as JSX text or tags, which produce no tokens.
	// Where the lexer took code for JSX, what they hold is lost to it (see
	// withCoarse).
	skipped [][2]int
}

// lex tokenizes src without JSX (configuration files).
func lex(src []byte) *lexer { return lexCode(src, false) }

// lexCode tokenizes src, recognizing JSX if jsx is set.
func lexCode(src []byte, jsx bool) *lexer {
	l := &lexer{src: src, toks: make([]token, 0, len(src)/6+8), slash: slashRegex, incdec: -1, jsx: jsx}
	l.run()
	return l
}

func (l *lexer) run() {
	src := l.src
	n := len(src)
	i := 0
	if bytes.HasPrefix(src, []byte(bom)) {
		i = len(bom)
	}
	if i+1 < n && src[i] == '#' && src[i+1] == '!' {
		i = lineEnd(src, i)
	}
	for i < n {
		if k := len(l.elems); k > 0 && l.elems[k-1].depth == l.depth {
			j := l.jsxStep(i)
			if k := len(l.skipped); k > 0 && l.skipped[k-1][1] == i {
				l.skipped[k-1][1] = j
			} else if j > i {
				l.skipped = append(l.skipped, [2]int{i, j})
			}
			i = j
			continue
		}
		c := src[i]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\f' || c == '\v':
			i++
		case c == '/' && i+1 < n && src[i+1] == '/':
			j := lineEnd(src, i)
			if i+2 < n && src[i+2] == '/' {
				l.directive(src[i+3 : j])
			}
			i = j
		case c == '/' && i+1 < n && src[i+1] == '*':
			// A regex never starts with '*': in code this is a comment wherever it is.
			i = commentEnd(src, i)
		case c == '\'' || c == '"':
			i = l.str(i)
		case c == '`':
			l.toks = append(l.toks, token{kind: tTemplate, s: i + 1})
			i = l.template(i+1, len(l.toks)-1, false)
		case c == '}' && len(l.frames) > 0 && l.frames[len(l.frames)-1].depth == l.depth:
			f := l.frames[len(l.frames)-1]
			l.frames = l.frames[:len(l.frames)-1]
			i = l.template(i+1, f.tok, true)
		case c == '/' && l.slash != slashDiv:
			if j, ok := l.regex(i, l.slash == slashMaybe); ok {
				l.emit(token{kind: tRegex, s: i, e: j})
				i = j
			} else {
				l.punct(i)
				i++
			}
		case c == '<' && l.jsxStart(i):
			l.elems = append(l.elems, jsxElem{depth: l.depth, state: jsxTag})
			i++
		case isIdentStart(c):
			j := i + 1
			for j < n && isIdentPart(src[j]) {
				j++
			}
			l.emit(token{kind: tIdent, s: i, e: j})
			i = j
		case c >= '0' && c <= '9':
			j := i + 1
			for j < n && (isIdentPart(src[j]) || src[j] == '.') {
				j++
			}
			l.emit(token{kind: tNumber, s: i, e: j})
			i = j
		default:
			l.punct(i)
			i++
		}
	}
}

// commentEnd returns the offset after the block comment starting at i (the end of
// input if it is not closed).
func commentEnd(src []byte, i int) int {
	j := bytes.Index(src[i+2:], []byte("*/"))
	if j < 0 {
		return len(src)
	}
	return i + j + 4
}

// tokAt returns the token k places before the last one (0: the last), and false if
// there is none.
func (l *lexer) tokAt(k int) (token, bool) {
	i := len(l.toks) - 1 - k
	if i < 0 {
		return token{}, false
	}
	return l.toks[i], true
}

// isPunct reports whether t is the punctuator c.
func isPunct(t token, c byte) bool { return t.kind == tPunct && t.c == c }

// name returns the text of an identifier token ("" for other tokens).
func (l *lexer) name(t token) string {
	if t.kind != tIdent {
		return ""
	}
	return string(l.src[t.s:t.e])
}

// emit appends a token and records how a following '/' is read.
func (l *lexer) emit(t token) {
	prev, hasPrev := l.tokAt(0)
	l.toks = append(l.toks, t)
	switch t.kind {
	case tPunct:
		l.slash = l.punctSlash(t, prev, hasPrev && prev.e == t.s)
	case tIdent:
		l.slash = l.identSlash(l.name(t), prev)
	default:
		l.slash = slashDiv
	}
}

// identSlash returns how a '/' after the identifier name is read; prev is the token
// before it.
func (l *lexer) identSlash(name string, prev token) slashMode {
	// After '.' a keyword is a property name: x.return / 2.
	if isPunct(prev, '.') {
		return slashDiv
	}
	switch name {
	case "of":
		// A keyword only in the head of a for statement, and not as the name a
		// declaration binds there (for (const of of xs)). Elsewhere it is an
		// identifier: const r = of / 2.
		if k := len(l.parens); k > 0 && l.parens[k-1] == parenFor {
			if p := l.name(prev); p != "const" && p != "let" && p != "var" {
				return slashRegex
			}
		}
		return slashDiv
	case "await", "yield":
		// Keywords in async functions and generators, identifiers elsewhere (in
		// scripts): either reading is plausible.
		return slashMaybe
	}
	if regexKeywords[name] {
		return slashRegex
	}
	return slashDiv
}

// punctSlash returns how a '/' after the punctuator t is read. prev is the token before
// t and adjacent reports whether it ends exactly where t starts.
func (l *lexer) punctSlash(t, prev token, adjacent bool) slashMode {
	switch t.c {
	case '(':
		l.parens = append(l.parens, l.parenKind())
	case ')':
		kind := parenOther
		if k := len(l.parens); k > 0 {
			kind = l.parens[k-1]
			l.parens = l.parens[:k-1]
		}
		if kind != parenOther {
			return slashRegex // if (a) /re/.test(s): a statement starts
		}
		return slashDiv
	case ']':
		return slashDiv
	case '}':
		if l.closed == braceBlock {
			return slashRegex // function f() {}\n/re/.test(s): a statement starts
		}
		// After an object literal a slash divides ({a: 1}.a / 2 aside, rarely); the
		// brace may also be one the lexer could not classify.
		return slashMaybe
	case '>':
		// After an arrow (=>) a slash starts an expression body, so a regex.
		if adjacent && isPunct(prev, '=') {
			return slashRegex
		}
		// It may start a regex (a > /re/) or, where JSX went unrecognized, be JSX text
		// or a closing tag.
		return slashMaybe
	case '<':
		// </tag> in JSX the lexer did not recognize. a < /re/ is valid but practically
		// never written.
		return slashDiv
	case '+', '-':
		// Maximal munch: ++ and -- pair from the left, so a+++b is a++ + b. After a
		// postfix ++/-- a slash divides; a prefix ++/-- is never followed by '/'.
		k := len(l.toks) - 1
		if adjacent && isPunct(prev, t.c) && l.incdec != k-1 {
			l.incdec = k
			return slashDiv
		}
	case '!':
		// TypeScript non-null assertion: width! / 2, f()! / 2, a[i]! / 2. A prefix ! is
		// never directly attached to an operand on its left, except after a keyword
		// (return!/re/.test(s), as minifiers write).
		if adjacent && (prev.kind == tPunct && (prev.c == ')' || prev.c == ']') ||
			prev.kind == tIdent && !regexKeywords[l.name(prev)]) {
			return slashDiv
		}
	}
	return slashRegex
}

// parenKind classifies the '(' just emitted by the tokens before it: if (, while (,
// with (, for (, for await (; not a method so named (x.if()).
func (l *lexer) parenKind() parenKind {
	kw, _ := l.tokAt(1)
	before, _ := l.tokAt(2)
	if isPunct(before, '.') {
		return parenOther
	}
	switch l.name(kw) {
	case "if", "while", "with":
		return parenHead
	case "for":
		return parenFor
	case "await":
		if forKw, _ := l.tokAt(2); l.name(forKw) == "for" {
			return parenFor
		}
	}
	return parenOther
}

// braceKind classifies a '{' about to be emitted by the token before it.
func (l *lexer) braceKind() braceKind {
	t, ok := l.tokAt(0)
	if !ok {
		return braceBlock
	}
	switch t.kind {
	case tPunct:
		switch t.c {
		case ';', '}', ')':
			// After a statement, or after the head of if/for/while/catch/switch, a
			// function's or a method's parameters.
			return braceBlock
		case '>':
			// An arrow body (=> {), or the end of a type before a body:
			// function f(): Promise<T> {, class A<T> {.
			return braceBlock
		case '{':
			if k := len(l.braces); k == 0 || l.braces[k-1] == braceBlock {
				return braceBlock
			}
		}
		return braceObject
	case tIdent:
		if before, _ := l.tokAt(1); isPunct(before, '.') {
			return braceObject
		}
		switch name := l.name(t); name {
		case "else", "do", "try", "finally":
			return braceBlock
		case "default":
			return braceObject // export default { ... }
		default:
			if regexKeywords[name] {
				// return {, typeof {, ... take an expression; `: void {` ends a
				// return type.
				if before, _ := l.tokAt(1); isPunct(before, ':') {
					return braceBlock
				}
				return braceObject
			}
		}
		// class A {, extends B {, interface I {, enum E {, namespace N {.
		return braceBlock
	}
	// A literal: declare module "x" {.
	return braceBlock
}

func (l *lexer) punct(i int) {
	c := l.src[i]
	switch c {
	case '{':
		l.braces = append(l.braces, l.braceKind())
		l.depth++
	case '}':
		l.depth--
		l.closed = braceObject
		if k := len(l.braces); k > 0 {
			l.closed = l.braces[k-1]
			l.braces = l.braces[:k-1]
		}
	}
	l.emit(token{kind: tPunct, c: c, s: i, e: i + 1})
}

// jsxStart reports whether the '<' at i opens a JSX element: JSX is enabled, an
// expression may start here, and what follows reads as a tag: <>, <Name>, <Name/>,
// <Name attr, <Name {...props}, <Name<T>. A TypeScript or Flow type parameter list
// (<T,>, <T extends U>, <T = U>, <const T>, and <T> before a parameter list: a generic
// arrow function or call signature, <T>(x: T) => T) does not. Taking a type for JSX
// would skip the code after it as JSX text; taking JSX for code is what the lexer did
// before it knew JSX.
func (l *lexer) jsxStart(i int) bool {
	if !l.jsx {
		return false
	}
	if l.slash != slashRegex {
		prev, _ := l.tokAt(0)
		if p := l.name(prev); p != "await" && p != "yield" {
			return false
		}
	}
	src := l.src
	n := len(src)
	j := i + 1
	if j < n && src[j] == '>' {
		return true // fragment
	}
	if j >= n || !isJSXNameStart(src[j]) {
		return false
	}
	k := j
	for k < n && isJSXNamePart(src[k]) {
		k++
	}
	if string(src[j:k]) == "const" {
		return false
	}
	for k < n && (isSpace(src[k]) || src[k] == '\v') {
		k++
	}
	if k >= n {
		return false
	}
	switch c := src[k]; {
	case c == '>':
		m := k + 1
		for m < n && (isSpace(src[m]) || src[m] == '\v' || src[m] == '\n' || src[m] == '\r') {
			m++
		}
		return m >= n || src[m] != '('
	case c == '/', c == '{', c == '<':
		return true
	case isJSXNameStart(c):
		e := k
		for e < n && isIdentPart(src[e]) {
			e++
		}
		return string(src[k:e]) != "extends"
	}
	return false
}

func isJSXNameStart(c byte) bool {
	return c == '_' || c == '$' || c >= 0x80 || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

func isJSXNamePart(c byte) bool {
	return isJSXNameStart(c) || (c >= '0' && c <= '9') || c == '-' || c == '.' || c == ':'
}

// jsxStep lexes JSX at i, inside the innermost open element, up to the next point
// where code starts (an expression container) or the element changes, and returns the
// offset to continue at. Text and attribute values produce no tokens: what they
// contain (apostrophes, slashes, backticks, // or /*) is not code.
func (l *lexer) jsxStep(i int) int {
	src := l.src
	n := len(src)
	e := &l.elems[len(l.elems)-1]
	if e.state == jsxText {
		for j := i; j < n; j++ {
			switch src[j] {
			case '{':
				l.openContainer(j)
				return j + 1
			case '<':
				switch {
				case j+1 < n && src[j+1] == '/':
					e.state = jsxClose
					return j + 2
				case j+1 < n && (src[j+1] == '>' || isJSXNameStart(src[j+1])):
					l.elems = append(l.elems, jsxElem{depth: l.depth, state: jsxTag})
					return j + 1
				}
			case '}':
				// Not JSX text: the lexer misjudged the element, and this closes a
				// template substitution.
				if k := len(l.frames); k > 0 && l.frames[k-1].depth == l.depth {
					l.abortJSX()
					return j
				}
			}
		}
		return n
	}
	for j := i; j < n; j++ {
		c := src[j]
		switch {
		case isSpace(c) || c == '\v' || isJSXNamePart(c) || c == '=':
		case c == '/' && j+1 < n && src[j+1] == '>':
			l.closeElem()
			return j + 2
		case c == '>':
			if e.state == jsxTag {
				e.state = jsxText
			} else {
				l.closeElem()
			}
			return j + 1
		case c == '/' && j+1 < n && src[j+1] == '/':
			j = lineEnd(src, j) - 1
		case c == '/' && j+1 < n && src[j+1] == '*':
			j = commentEnd(src, j) - 1
		case (c == '"' || c == '\'') && e.state == jsxTag:
			// Attribute values have no escapes and may span lines. The search is
			// bounded, so that unterminated ones cannot make lexing quadratic.
			k := bytes.IndexByte(src[j+1:min(n, j+1+maxAttrValue)], c)
			if k < 0 {
				l.abortJSX()
				return j
			}
			j += k + 1
		case c == '{' && e.state == jsxTag:
			l.openContainer(j)
			return j + 1
		case c == '<' && e.state == jsxTag:
			// Type arguments of a generic component: <Select<Option> ...>.
			j = skipAngles(src, j) - 1
		default:
			// Not a tag after all.
			l.abortJSX()
			return j
		}
	}
	return n
}

// maxAttrValue bounds the length of a JSX attribute string value.
const maxAttrValue = 1 << 14

// openContainer opens the JSX expression container whose '{' is at i.
func (l *lexer) openContainer(i int) {
	l.braces = append(l.braces, braceJSX)
	l.depth++
	l.emit(token{kind: tPunct, c: '{', s: i, e: i + 1})
	l.slash = slashRegex
}

// closeElem closes the innermost JSX element. Back in code, the element was an operand.
func (l *lexer) closeElem() {
	l.elems = l.elems[:len(l.elems)-1]
	if k := len(l.elems); k == 0 || l.elems[k-1].depth != l.depth {
		l.slash = slashDiv
	}
}

// abortJSX drops the elements open at the current depth: what the lexer took for JSX
// is not, and lexing goes on as code.
func (l *lexer) abortJSX() {
	for k := len(l.elems); k > 0 && l.elems[k-1].depth == l.depth; k-- {
		l.elems = l.elems[:k-1]
	}
	l.slash = slashMaybe
}

// skipAngles returns the offset after the '>' closing the '<' at i (nested pairs
// counted, an arrow's '>' not), or the line end if it does not close on the line.
func skipAngles(src []byte, i int) int {
	depth := 0
	for j := i; j < len(src); j++ {
		switch src[j] {
		case '<':
			depth++
		case '>':
			if j > 0 && src[j-1] == '=' {
				continue
			}
			if depth--; depth == 0 {
				return j + 1
			}
		case '\n', '\r':
			return j
		}
	}
	return len(src)
}

// str lexes a quoted string starting at i and returns the offset after it.
func (l *lexer) str(i int) int {
	src := l.src
	q := src[i]
	for j := i + 1; j < len(src); j++ {
		switch src[j] {
		case '\\':
			if j+2 < len(src) && src[j+1] == '\r' && src[j+2] == '\n' {
				j++
			}
			j++
		case q:
			l.emit(token{kind: tString, s: i + 1, e: j})
			return j + 1
		case '\n', '\r':
			// Unterminated: not a string after all (e.g. an apostrophe in JSX text).
			l.emit(token{kind: tString, s: i + 1, e: j})
			return j
		}
	}
	l.emit(token{kind: tString, s: i + 1, e: len(src)})
	return len(src)
}

// template lexes one segment of the template literal toks[tok], starting at i (just
// after the opening backtick, or after the } closing a substitution when tail is set).
// It returns the offset at which lexing continues.
func (l *lexer) template(i, tok int, tail bool) int {
	src := l.src
	set := func(e int) {
		if tail {
			l.toks[tok].ts, l.toks[tok].te = i, e
		} else {
			l.toks[tok].e = e
		}
	}
	for j := i; j < len(src); j++ {
		switch src[j] {
		case '\\':
			j++
		case '`':
			set(j)
			l.slash = slashDiv
			return j + 1
		case '$':
			if j+1 < len(src) && src[j+1] == '{' {
				set(j)
				l.toks[tok].expr = true
				l.frames = append(l.frames, tmplFrame{tok: tok, depth: l.depth})
				l.slash = slashRegex
				return j + 2
			}
		}
	}
	set(len(src))
	return len(src)
}

// regex lexes a regular expression literal starting at i. It reports false when the
// literal does not close on the same line, in which case the slash is a division.
//
// When strict is set (the slash may as well be a division) the literal is also rejected
// if it starts with '>' (a JSX self-closing tag: {x} />), or if its body contains a
// quote or backtick and what follows it does not continue a regex operand (.test(s),
// a closing bracket, a separator, an operator such as && or ==, the line end). Such a
// body is far more likely code spanning two divisions ({a} / {`b`} / c), and swallowing
// its opening quote or backtick would hide the literal that follows; for a backtick,
// everything up to the next template literal.
func (l *lexer) regex(i int, strict bool) (int, bool) {
	src := l.src
	if strict && i+1 < len(src) && src[i+1] == '>' {
		return 0, false
	}
	inClass, quoted := false, false
	for j := i + 1; j < len(src); j++ {
		switch src[j] {
		case '`', '\'', '"':
			quoted = true
		case '\\':
			j++
			if j < len(src) && (src[j] == '\n' || src[j] == '\r') {
				return 0, false
			}
			if j < len(src) && (src[j] == '`' || src[j] == '\'' || src[j] == '"') {
				quoted = true
			}
		case '\n', '\r':
			return 0, false
		case '[':
			inClass = true
		case ']':
			inClass = false
		case '/':
			if !inClass {
				j++
				for j < len(src) && isIdentPart(src[j]) {
					j++
				}
				if strict && quoted && !regexFollows(src, j) {
					return 0, false
				}
				return j, true
			}
		}
	}
	return 0, false
}

// regexFollows reports whether the text at j (just after a regex literal and its
// flags) can follow a regular expression operand.
func regexFollows(src []byte, j int) bool {
	for j < len(src) && (src[j] == ' ' || src[j] == '\t') {
		j++
	}
	if j >= len(src) {
		return true
	}
	return strings.IndexByte(".),;]}:?&|=!\n\r", src[j]) >= 0
}

// directive records TypeScript triple-slash references: /// <reference path="..." /> is
// a file relative to the current one and /// <reference types="..." /> a package.
func (l *lexer) directive(text []byte) {
	text = bytes.TrimLeft(text, " \t")
	if !bytes.HasPrefix(text, []byte("<reference")) {
		return
	}
	if v, ok := attrValue(text, "path"); ok && v != "" {
		l.refs = append(l.refs, ref{kind: refFile, spec: v})
	}
	if v, ok := attrValue(text, "types"); ok && v != "" {
		l.refs = append(l.refs, ref{kind: refModule, spec: v})
	}
}

// bom is the UTF-8 byte order mark, skipped at the start of a file.
const bom = "\xef\xbb\xbf"

// regexKeywords are identifiers after which a '/' starts a regular expression. of,
// await and yield are contextual (see identSlash).
var regexKeywords = map[string]bool{
	"return": true, "typeof": true, "instanceof": true, "in": true, "of": true,
	"new": true, "delete": true, "void": true, "throw": true, "case": true, "do": true,
	"else": true, "yield": true, "await": true, "extends": true,
}

func isIdentStart(c byte) bool {
	return c == '_' || c == '$' || c == '#' || c == '\\' || c >= 0x80 ||
		(c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

func isIdentPart(c byte) bool {
	return isIdentStart(c) || (c >= '0' && c <= '9')
}

func lineEnd(src []byte, i int) int {
	for j := i; j < len(src); j++ {
		if src[j] == '\n' || src[j] == '\r' {
			return j
		}
	}
	return len(src)
}

// attrValue returns the value of an HTML/XML-style attribute name="value" (or 'value',
// or unquoted) in b. The name must start b or follow whitespace.
func attrValue(b []byte, name string) (string, bool) {
	for off := 0; ; {
		k := bytes.Index(b[off:], []byte(name))
		if k < 0 {
			return "", false
		}
		k += off
		off = k + len(name)
		if k > 0 && !isSpace(b[k-1]) {
			continue
		}
		j := off
		for j < len(b) && isSpace(b[j]) {
			j++
		}
		if j >= len(b) || b[j] != '=' {
			continue
		}
		j++
		for j < len(b) && isSpace(b[j]) {
			j++
		}
		if j >= len(b) {
			return "", false
		}
		if q := b[j]; q == '"' || q == '\'' {
			end := bytes.IndexByte(b[j+1:], q)
			if end < 0 {
				return "", false
			}
			return string(b[j+1 : j+1+end]), true
		}
		end := j
		for end < len(b) && !isSpace(b[end]) && b[end] != '>' {
			end++
		}
		return string(b[j:end]), true
	}
}

func isSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\f'
}

// unescape returns the cooked value of a raw string or template literal body.
func unescape(raw []byte) string {
	if bytes.IndexByte(raw, '\\') < 0 {
		return string(raw)
	}
	var b strings.Builder
	b.Grow(len(raw))
	for i := 0; i < len(raw); i++ {
		c := raw[i]
		if c != '\\' || i+1 >= len(raw) {
			b.WriteByte(c)
			continue
		}
		i++
		switch raw[i] {
		case '\n': // line continuation
		case '\r':
			if i+1 < len(raw) && raw[i+1] == '\n' {
				i++
			}
		case 'n':
			b.WriteByte('\n')
		case 't':
			b.WriteByte('\t')
		case 'r':
			b.WriteByte('\r')
		case 'b':
			b.WriteByte('\b')
		case 'f':
			b.WriteByte('\f')
		case 'v':
			b.WriteByte('\v')
		case '0':
			b.WriteByte(0)
		case 'x':
			if i+2 < len(raw) {
				if v, err := strconv.ParseUint(string(raw[i+1:i+3]), 16, 8); err == nil {
					b.WriteRune(rune(v))
					i += 2
					continue
				}
			}
			b.WriteByte('x')
		case 'u':
			if r, n := unicodeEscape(raw[i+1:]); n > 0 {
				b.WriteRune(r)
				i += n
				continue
			}
			b.WriteByte('u')
		default:
			b.WriteByte(raw[i])
		}
	}
	return b.String()
}

// unicodeEscape decodes the part of a \u escape after the 'u' (XXXX or {X...}) and
// returns the rune and the number of bytes consumed (0 if malformed).
func unicodeEscape(b []byte) (rune, int) {
	if len(b) > 0 && b[0] == '{' {
		end := bytes.IndexByte(b, '}')
		if end < 2 {
			return 0, 0
		}
		v, err := strconv.ParseUint(string(b[1:end]), 16, 32)
		if err != nil || !utf8.ValidRune(rune(v)) {
			return 0, 0
		}
		return rune(v), end + 1
	}
	if len(b) < 4 {
		return 0, 0
	}
	v, err := strconv.ParseUint(string(b[:4]), 16, 32)
	if err != nil {
		return 0, 0
	}
	return rune(v), 4
}

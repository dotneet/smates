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

// tmplFrame records an open ${ substitution: the template token it belongs to and the
// brace depth at which its closing } is expected.
type tmplFrame struct {
	tok   int
	depth int
}

// lexer is a small JavaScript/TypeScript tokenizer. It is not a validating parser: its
// job is to skip comments, strings, template literals and regular expressions exactly
// enough that module specifiers are never taken from (or hidden by) them.
//
// Strings and regular expressions never span lines in valid code, so both stop at a line
// break. This bounds the damage of a mis-lexed construct (an apostrophe in JSX text, a
// division mistaken for a regex) to a single line.
type lexer struct {
	src     []byte
	toks    []token
	refs    []ref // references found in comments (triple-slash directives)
	frames  []tmplFrame
	depth   int
	regexOK bool // whether a '/' here starts a regular expression
}

// lex tokenizes src.
func lex(src []byte) *lexer {
	l := &lexer{src: src, toks: make([]token, 0, len(src)/6+8), regexOK: true}
	l.run()
	return l
}

func (l *lexer) run() {
	src := l.src
	n := len(src)
	i := 0
	if n >= 2 && src[0] == '#' && src[1] == '!' {
		i = lineEnd(src, 0)
	}
	for i < n {
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
			j := bytes.Index(src[i+2:], []byte("*/"))
			if j < 0 {
				i = n
			} else {
				i += j + 4
			}
		case c == '\'' || c == '"':
			i = l.str(i)
		case c == '`':
			l.toks = append(l.toks, token{kind: tTemplate, s: i + 1})
			i = l.template(i+1, len(l.toks)-1, false)
		case c == '}' && len(l.frames) > 0 && l.frames[len(l.frames)-1].depth == l.depth:
			f := l.frames[len(l.frames)-1]
			l.frames = l.frames[:len(l.frames)-1]
			i = l.template(i+1, f.tok, true)
		case c == '/' && l.regexOK:
			if j, ok := l.regex(i); ok {
				l.emit(token{kind: tRegex, s: i, e: j})
				i = j
			} else {
				l.punct(i)
				i++
			}
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

// emit appends a token and records whether a following '/' would start a regex.
func (l *lexer) emit(t token) {
	l.toks = append(l.toks, t)
	switch t.kind {
	case tPunct:
		// After ')' and ']' a slash divides. After '}' it is ambiguous (block vs object
		// literal); a regex is the more common reading and the line bound limits the cost
		// of guessing wrong.
		l.regexOK = t.c != ')' && t.c != ']'
	case tIdent:
		l.regexOK = regexKeywords[string(l.src[t.s:t.e])]
	default:
		l.regexOK = false
	}
}

func (l *lexer) punct(i int) {
	c := l.src[i]
	switch c {
	case '{':
		l.depth++
	case '}':
		l.depth--
	}
	l.emit(token{kind: tPunct, c: c, s: i, e: i + 1})
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
			l.regexOK = false
			return j + 1
		case '$':
			if j+1 < len(src) && src[j+1] == '{' {
				set(j)
				l.toks[tok].expr = true
				l.frames = append(l.frames, tmplFrame{tok: tok, depth: l.depth})
				l.regexOK = true
				return j + 2
			}
		}
	}
	set(len(src))
	return len(src)
}

// regex lexes a regular expression literal starting at i. It reports false when the
// literal does not close on the same line, in which case the slash is a division.
func (l *lexer) regex(i int) (int, bool) {
	src := l.src
	inClass := false
	for j := i + 1; j < len(src); j++ {
		switch src[j] {
		case '\\':
			j++
			if j < len(src) && (src[j] == '\n' || src[j] == '\r') {
				return 0, false
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
				return j, true
			}
		}
	}
	return 0, false
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

// regexKeywords are identifiers after which a '/' starts a regular expression.
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

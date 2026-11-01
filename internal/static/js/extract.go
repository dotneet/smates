package js

import (
	"bytes"
	"strings"
)

// refKind says how a reference found in source is resolved.
type refKind uint8

const (
	// refModule is a module specifier resolved with Node/TypeScript/bundler rules.
	refModule refKind = iota
	// refFile is a path relative to the importing file (new URL(..., import.meta.url),
	// /// <reference path>, src attributes of single-file components).
	refFile
	// refTemplate is a template literal with substitutions: any file matching
	// spec + "*" + suffix may be loaded.
	refTemplate
	// refGlob is an import.meta.glob pattern.
	refGlob
	// refContext is a require.context directory.
	refContext
)

// ref is a reference to another file found in source.
type ref struct {
	kind refKind
	spec string
	// suffix is the static text after the last substitution of a refTemplate.
	suffix string
	// recursive is whether a refContext includes subdirectories.
	recursive bool
}

// mockFns are the methods of vi / jest / vitest that take a module specifier.
var mockFns = map[string]bool{
	"mock": true, "doMock": true, "unmock": true, "doUnmock": true,
	"importActual": true, "importMock": true, "requireActual": true, "requireMock": true,
	"createMockFromModule": true, "genMockFromModule": true, "setMock": true,
}

// scanSource returns the references in JavaScript or TypeScript source.
func scanSource(src []byte) []ref {
	l := lex(src)
	p := parser{src: src, toks: l.toks}
	refs := l.refs
	for i, t := range p.toks {
		if t.kind != tIdent {
			continue
		}
		switch string(src[t.s:t.e]) {
		case "import":
			if p.punct(i-1, '.') {
				continue // obj.import(...)
			}
			switch {
			case p.punct(i+1, '('): // import("x"), typeof import("x")
				refs = p.appendArg(refs, i+2)
			case p.literal(i + 1): // import "x"
				refs = p.appendArg(refs, i+1)
			case p.punct(i+1, '.') && p.ident(i+2, "meta") && p.punct(i+3, '.') && p.punct(i+5, '('):
				switch {
				case p.ident(i+4, "glob") || p.ident(i+4, "globEager"):
					refs = p.appendGlobs(refs, i+6)
				case p.ident(i+4, "resolve"):
					refs = p.appendArg(refs, i+6)
				}
			}
		case "from":
			// `from` directly followed by a string literal only occurs in import and
			// export declarations, so no statement-level parsing is needed.
			if !p.punct(i-1, '.') && p.literal(i+1) {
				refs = p.appendArg(refs, i+1)
			}
		case "require":
			switch {
			case p.punct(i+1, '('):
				refs = p.appendArg(refs, i+2)
			case p.punct(i+1, '.') && p.ident(i+2, "resolve") && p.punct(i+3, '('):
				refs = p.appendArg(refs, i+4)
			case p.punct(i+1, '.') && p.ident(i+2, "context") && p.punct(i+3, '(') && p.literal(i+4):
				recursive := !(p.punct(i+5, ',') && p.ident(i+6, "false"))
				refs = append(refs, ref{kind: refContext, spec: p.text(i + 4), recursive: recursive})
			}
		case "vi", "jest", "vitest":
			if p.punct(i+1, '.') && p.punct(i+3, '(') && p.toks[i+2].kind == tIdent &&
				mockFns[string(src[p.toks[i+2].s:p.toks[i+2].e])] {
				refs = p.appendArg(refs, i+4)
			}
		case "URL":
			// new URL("./file", import.meta.url)
			if p.punct(i+1, '(') && p.literal(i+2) && p.punct(i+3, ',') && p.ident(i+4, "import") &&
				p.punct(i+5, '.') && p.ident(i+6, "meta") && p.punct(i+7, '.') && p.ident(i+8, "url") {
				refs = append(refs, ref{kind: refFile, spec: p.text(i + 2)})
			}
		}
	}
	return refs
}

// parser gives indexed, bounds-checked access to a token stream.
type parser struct {
	src  []byte
	toks []token
}

func (p *parser) at(i int) token {
	if i < 0 || i >= len(p.toks) {
		return token{}
	}
	return p.toks[i]
}

func (p *parser) punct(i int, c byte) bool {
	t := p.at(i)
	return t.kind == tPunct && t.c == c
}

func (p *parser) ident(i int, name string) bool {
	t := p.at(i)
	return t.kind == tIdent && string(p.src[t.s:t.e]) == name
}

// literal reports whether token i is a string literal or a template literal without
// substitutions.
func (p *parser) literal(i int) bool {
	t := p.at(i)
	return t.kind == tString || (t.kind == tTemplate && !t.expr)
}

// text returns the cooked value of the literal at i.
func (p *parser) text(i int) string {
	t := p.at(i)
	return unescape(p.src[t.s:t.e])
}

// appendArg appends the specifier at token i, if it is a literal, a template literal
// with a static prefix, or a concatenation starting with a literal ("./x/" + name).
// A leading __dirname or import.meta.dirname counts as the static prefix "."
// (`${__dirname}/x/${n}.js`, __dirname + "/x/" + n).
func (p *parser) appendArg(refs []ref, i int) []ref {
	t := p.at(i)
	switch {
	case p.literal(i) && p.punct(i+1, '+'):
		return append(refs, ref{kind: refTemplate, spec: p.text(i), suffix: p.concatSuffix(i + 2)})
	case p.literal(i):
		return append(refs, ref{kind: refModule, spec: p.text(i)})
	case t.kind == tTemplate:
		if r, ok := p.dirnameTemplate(i); ok {
			return append(refs, r)
		}
		return append(refs, ref{
			kind:   refTemplate,
			spec:   unescape(p.src[t.s:t.e]),
			suffix: unescape(p.src[t.ts:t.te]),
		})
	case p.dirname(i) > i && p.punct(p.dirname(i), '+') && p.literal(p.dirname(i)+1):
		j := p.dirname(i) + 1
		if s := p.text(j); strings.HasPrefix(s, "/") {
			if p.punct(j+1, '+') {
				return append(refs, ref{kind: refTemplate, spec: "." + s, suffix: p.concatSuffix(j + 2)})
			}
			return append(refs, ref{kind: refModule, spec: "." + s})
		}
	}
	return refs
}

// dirname returns the index after a __dirname or import.meta.dirname expression at
// token i, or i if there is none.
func (p *parser) dirname(i int) int {
	switch {
	case p.ident(i, "__dirname") && !p.punct(i-1, '.'):
		return i + 1
	case p.ident(i, "import") && p.punct(i+1, '.') && p.ident(i+2, "meta") && p.punct(i+3, '.') && p.ident(i+4, "dirname"):
		return i + 5
	}
	return i
}

// dirnameTemplate returns the reference of the template literal at token i if it
// starts with a dirname substitution followed by "/": the text up to the next
// substitution (or the end) is then its static prefix, relative to the file.
func (p *parser) dirnameTemplate(i int) (ref, bool) {
	t := p.at(i)
	k := p.dirname(i + 1)
	if !t.expr || t.s != t.e || k == i+1 {
		return ref{}, false
	}
	// The lexer consumes the '}' closing the substitution and the text after it, so
	// the segment is read from the source.
	src := p.src
	j := p.at(k - 1).e
	for j < len(src) && isSpace(src[j]) {
		j++
	}
	if j >= len(src) || src[j] != '}' {
		return ref{}, false
	}
	start := j + 1
	for j = start; j < len(src); j++ {
		var r ref
		switch {
		case src[j] == '\\':
			j++
			continue
		case src[j] == '`':
			r = ref{kind: refModule}
		case src[j] == '$' && j+1 < len(src) && src[j+1] == '{':
			r = ref{kind: refTemplate, suffix: unescape(src[t.ts:t.te])}
		default:
			continue
		}
		seg := unescape(src[start:j])
		if !strings.HasPrefix(seg, "/") {
			return ref{}, false
		}
		r.spec = "." + seg
		return r, true
	}
	return ref{}, false
}

// concatSuffix returns the trailing literal of a concatenation whose remaining operands
// start at token i ("./x/" + name + ".js" -> ".js"), or "" if it does not end in one.
// Only short, flat expressions are inspected.
func (p *parser) concatSuffix(i int) string {
	depth := 0
	for j := i; j < len(p.toks) && j < i+64; j++ {
		t := p.toks[j]
		if t.kind != tPunct {
			continue
		}
		switch t.c {
		case '(', '[', '{':
			depth++
		case ']', '}':
			depth--
		case ')', ',', ';':
			if depth > 0 && t.c == ')' {
				depth--
				continue
			}
			if depth == 0 && j-2 >= i && p.punct(j-2, '+') && p.literal(j-1) {
				return p.text(j - 1)
			}
			if depth == 0 {
				return ""
			}
		}
	}
	return ""
}

// appendGlobs appends the import.meta.glob pattern(s) at token i: a literal or an array
// of literals.
func (p *parser) appendGlobs(refs []ref, i int) []ref {
	if p.literal(i) {
		return append(refs, ref{kind: refGlob, spec: p.text(i)})
	}
	if !p.punct(i, '[') {
		return refs
	}
	for j := i + 1; j < len(p.toks) && !p.punct(j, ']'); j++ {
		if p.literal(j) {
			refs = append(refs, ref{kind: refGlob, spec: p.text(j)})
		}
	}
	return refs
}

// scanSFC returns the references in a single-file component (.vue, .svelte, .astro).
// <script> blocks are lexed as code; markup between blocks is lexed too (for
// import()/require() in bindings) and its relative src attributes become file
// references. <script src> and <style src> are file references. For .astro, the
// leading --- frontmatter fence is code.
func scanSFC(src []byte, astro bool) []ref {
	var refs []ref
	lower := asciiLower(src)
	i := 0
	if astro {
		if body, end := frontmatter(src); end > 0 {
			refs = append(refs, scanSource(body)...)
			i = end
		}
	}
	markup := i
	for i < len(src) {
		k := bytes.IndexByte(src[i:], '<')
		if k < 0 {
			break
		}
		j := i + k
		if bytes.HasPrefix(src[j:], []byte("<!--")) {
			end := bytes.Index(src[j+4:], []byte("-->"))
			if end < 0 {
				refs = append(refs, scanMarkup(src[markup:j])...)
				return refs
			}
			refs = append(refs, scanMarkup(src[markup:j])...)
			i = j + 4 + end + 3
			markup = i
			continue
		}
		var name string
		switch {
		case isTagAt(lower, j, "script"):
			name = "script"
		case isTagAt(lower, j, "style"):
			name = "style"
		default:
			i = j + 1
			continue
		}
		refs = append(refs, scanMarkup(src[markup:j])...)
		tagEnd := tagClose(src, j)
		attrs := src[j+1+len(name) : tagEnd]
		if v, ok := attrValue(attrs, "src"); ok && v != "" {
			refs = append(refs, ref{kind: refFile, spec: v})
		}
		bodyStart := tagEnd + 1
		if tagEnd > j && src[tagEnd-1] == '/' { // self-closing
			i, markup = bodyStart, bodyStart
			continue
		}
		closeTag := []byte("</" + name)
		end := bytes.Index(lower[min(bodyStart, len(src)):], closeTag)
		if end < 0 {
			end = len(src)
		} else {
			end += bodyStart
		}
		if name == "script" && bodyStart < end {
			refs = append(refs, scanSource(src[bodyStart:end])...)
		}
		if end >= len(src) {
			return refs
		}
		i = tagClose(src, end) + 1
		markup = i
	}
	if markup < len(src) {
		refs = append(refs, scanMarkup(src[markup:])...)
	}
	return refs
}

// scanMarkup returns the references in template markup: code-like references found by
// the lexer plus relative src="..." attributes.
func scanMarkup(b []byte) []ref {
	if len(bytes.TrimSpace(b)) == 0 {
		return nil
	}
	// Bindings live inside double-quoted attributes (:src="require('./x')"); blank the
	// quotes so the lexer sees their code.
	code := make([]byte, len(b))
	for i, c := range b {
		if c == '"' {
			c = ' '
		}
		code[i] = c
	}
	refs := scanSource(code)
	for off := 0; off < len(b); {
		k := bytes.Index(b[off:], []byte("src"))
		if k < 0 {
			break
		}
		k += off
		off = k + 3
		if v, ok := attrValue(b[k:min(len(b), k+512)], "src"); ok && isRelative(v) {
			refs = append(refs, ref{kind: refFile, spec: v})
		}
	}
	return refs
}

// frontmatter returns the body of a leading --- fence and the offset after it.
func frontmatter(src []byte) ([]byte, int) {
	start := len(src) - len(bytes.TrimLeft(src, " \t\r\n\xef\xbb\xbf"))
	if !bytes.HasPrefix(src[start:], []byte("---")) {
		return nil, 0
	}
	bodyStart := start + 3
	k := bytes.Index(src[bodyStart:], []byte("\n---"))
	if k < 0 {
		return nil, 0
	}
	end := bodyStart + k
	return src[bodyStart:end], end + 4
}

// isTagAt reports whether lower[j:] opens the tag name (lower must be lower-cased).
func isTagAt(lower []byte, j int, name string) bool {
	if !bytes.HasPrefix(lower[j+1:], []byte(name)) {
		return false
	}
	k := j + 1 + len(name)
	return k >= len(lower) || isSpace(lower[k]) || lower[k] == '>' || lower[k] == '/'
}

// tagClose returns the offset of the '>' ending the tag that starts at j, skipping
// quoted attribute values (len(src) if unterminated).
func tagClose(src []byte, j int) int {
	var q byte
	for k := j; k < len(src); k++ {
		c := src[k]
		switch {
		case q != 0:
			if c == q {
				q = 0
			}
		case c == '"' || c == '\'':
			q = c
		case c == '>':
			return k
		}
	}
	return len(src)
}

// asciiLower lower-cases ASCII letters, keeping byte offsets intact.
func asciiLower(b []byte) []byte {
	out := make([]byte, len(b))
	for i, c := range b {
		if c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		}
		out[i] = c
	}
	return out
}

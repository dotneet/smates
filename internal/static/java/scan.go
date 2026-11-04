package java

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// unit is what scan finds in one Java compilation unit (a .java file).
type unit struct {
	pkg     string   // the package declaration ("" for the unnamed package)
	imports []imp    // import declarations, in order
	types   []string // names of the top-level types the file declares
	// idents holds every identifier outside the package and import declarations: the
	// candidates for simple names of types of the file's own package and of packages
	// it imports on demand.
	idents map[string]bool
	// names holds every qualified name (two identifiers or more joined by dots)
	// outside the package and import declarations: fully qualified type names, and
	// member accesses that never resolve to a type.
	names []string
}

// imp is one import declaration.
type imp struct {
	name     string // the dotted name, without a trailing ".*"
	static   bool   // import static
	onDemand bool   // ends with ".*"
}

// tokKind classifies lexer tokens.
type tokKind uint8

const (
	tokIdent tokKind = iota + 1
	tokPunct
)

// token is one lexical token. Literals and comments produce none.
type token struct {
	kind tokKind
	text string // tokIdent: the identifier
	op   byte   // tokPunct: the punctuation byte
}

// lex tokenizes Java source. It knows as much of the grammar as finding names needs:
// comments, string, text block and character literals (skipped, so a name inside one
// is not a reference), numbers, and identifiers (Unicode letters, digits, '_' and '$').
// Every other byte is a punctuation token.
func lex(s string) []token {
	if strings.Contains(s, `\u`) {
		s = unescapeUnicode(s)
	}
	toks := make([]token, 0, len(s)/5+1)
	i := 0
	for i < len(s) {
		c := s[i]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\f':
			i++
		case strings.HasPrefix(s[i:], "//"):
			// A lone CR ends a line too (JLS 3.4).
			if k := strings.IndexAny(s[i:], "\r\n"); k >= 0 {
				i += k
			} else {
				i = len(s)
			}
		case strings.HasPrefix(s[i:], "/*"):
			if k := strings.Index(s[i+2:], "*/"); k >= 0 {
				i += 2 + k + 2
			} else {
				i = len(s)
			}
		case strings.HasPrefix(s[i:], `"""`):
			i = skipTextBlock(s, i+3)
		case c == '"' || c == '\'':
			i = skipQuoted(s, i+1, c)
		case c >= '0' && c <= '9':
			i = skipNumber(s, i)
		case c < utf8.RuneSelf && !isIdentByte(c):
			toks = append(toks, token{kind: tokPunct, op: c})
			i++
		default:
			j := i
			for j < len(s) {
				r, size := utf8.DecodeRuneInString(s[j:])
				if !isIdentRune(r) {
					break
				}
				j += size
			}
			if j == i {
				// A non-ASCII rune that cannot be part of an identifier.
				_, size := utf8.DecodeRuneInString(s[i:])
				i += size
				continue
			}
			toks = append(toks, token{kind: tokIdent, text: s[i:j]})
			i = j
		}
	}
	return toks
}

func isIdentByte(c byte) bool {
	return c == '_' || c == '$' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9'
}

func isIdentRune(r rune) bool {
	if r < utf8.RuneSelf {
		return isIdentByte(byte(r))
	}
	return r != utf8.RuneError && (unicode.IsLetter(r) || unicode.IsDigit(r))
}

// skipQuoted returns the index after a string or character literal whose content starts
// at i. A literal cannot span lines, so an unterminated one ends at the line terminator
// (LF, CR or both).
func skipQuoted(s string, i int, quote byte) int {
	for i < len(s) {
		switch s[i] {
		case '\\':
			i += 2
		case quote:
			return i + 1
		case '\n', '\r':
			return i
		default:
			i++
		}
	}
	return len(s)
}

// skipTextBlock returns the index after a text block whose content starts at i.
func skipTextBlock(s string, i int) int {
	for i < len(s) {
		switch {
		case s[i] == '\\':
			i += 2
		case strings.HasPrefix(s[i:], `"""`):
			return i + 3
		default:
			i++
		}
	}
	return len(s)
}

// skipNumber returns the index after a numeric literal starting at i (1_000L, 0x1Fp-3,
// 1.5e+10f).
func skipNumber(s string, i int) int {
	start := i
	hex := strings.HasPrefix(s[i:], "0x") || strings.HasPrefix(s[i:], "0X")
	for i < len(s) {
		c := s[i]
		switch {
		case isIdentByte(c) || c == '.':
			i++
		case (c == '+' || c == '-') && i > start && isExponent(s[i-1], hex) && i+1 < len(s) && s[i+1] >= '0' && s[i+1] <= '9':
			// An exponent's sign. In 0xFE+Config.MASK, E is a hexadecimal digit and
			// the sign starts an addition: the literal must not swallow the name.
			i++
		default:
			return i
		}
	}
	return i
}

// isExponent reports whether c marks the exponent of a decimal (e) or hexadecimal (p)
// floating-point literal.
func isExponent(c byte, hex bool) bool {
	if hex {
		return c == 'p' || c == 'P'
	}
	return c == 'e' || c == 'E'
}

// unescapeUnicode translates the \uXXXX escapes Java applies before lexing (JLS 3.3),
// so that an escaped quote or line break cannot hide code from the lexer. A backslash
// preceded by an odd number of backslashes does not start an escape.
func unescapeUnicode(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	backslashes := 0
	for i := 0; i < len(s); {
		c := s[i]
		if c != '\\' {
			backslashes = 0
			b.WriteByte(c)
			i++
			continue
		}
		if backslashes%2 == 0 {
			j := i + 1
			for j < len(s) && s[j] == 'u' {
				j++
			}
			if j > i+1 && j+4 <= len(s) {
				if r, ok := hex4(s[j : j+4]); ok {
					b.WriteRune(r)
					i = j + 4
					backslashes = 0
					continue
				}
			}
		}
		backslashes++
		b.WriteByte(c)
		i++
	}
	return b.String()
}

func hex4(s string) (rune, bool) {
	var r rune
	for i := 0; i < 4; i++ {
		c := s[i]
		var d byte
		switch {
		case c >= '0' && c <= '9':
			d = c - '0'
		case c >= 'a' && c <= 'f':
			d = c - 'a' + 10
		case c >= 'A' && c <= 'F':
			d = c - 'A' + 10
		default:
			return 0, false
		}
		r = r<<4 | rune(d)
	}
	return r, true
}

// typeKeywords start a type declaration. record is a contextual keyword: it declares a
// type only when a name and a component list or type parameters follow.
var typeKeywords = map[string]bool{"class": true, "interface": true, "enum": true, "record": true}

// scan extracts what the analyzer needs from Java source.
func scan(src []byte) *unit {
	toks := lex(string(src))
	u := &unit{idents: map[string]bool{}}
	depth := 0
	for i := 0; i < len(toks); i++ {
		t := toks[i]
		if t.kind == tokPunct {
			switch t.op {
			case '{':
				depth++
			case '}':
				if depth > 0 {
					depth--
				}
			}
			continue
		}
		if depth == 0 {
			switch {
			case t.text == "package" && u.pkg == "" && len(u.types) == 0:
				if name, next, ok := dotted(toks, i+1); ok {
					u.pkg = name
					i = next - 1
					continue
				}
			case t.text == "import" && len(u.types) == 0:
				if im, next, ok := importDecl(toks, i+1); ok {
					u.imports = append(u.imports, im)
					i = next - 1
					continue
				}
			case typeKeywords[t.text] && !after(toks, i, '.'):
				if name, ok := typeName(toks, i); ok {
					u.types = append(u.types, name)
				}
			}
		}
		// A qualified name: record it whole, and each of its identifiers.
		name, next, _ := dotted(toks, i)
		u.idents[t.text] = true
		if next > i+1 {
			u.names = append(u.names, name)
			for _, part := range strings.Split(name, ".")[1:] {
				u.idents[part] = true
			}
			i = next - 1
		}
	}
	return u
}

// after reports whether the token before toks[i] is the punctuation op.
func after(toks []token, i int, op byte) bool {
	return i > 0 && toks[i-1].kind == tokPunct && toks[i-1].op == op
}

// dotted reads identifiers joined by dots starting at toks[i]. It returns the name, the
// index of the first token after it, and whether toks[i] is an identifier at all.
func dotted(toks []token, i int) (string, int, bool) {
	if i >= len(toks) || toks[i].kind != tokIdent {
		return "", i, false
	}
	var b strings.Builder
	b.WriteString(toks[i].text)
	j := i + 1
	for j+1 < len(toks) && toks[j].kind == tokPunct && toks[j].op == '.' && toks[j+1].kind == tokIdent {
		b.WriteByte('.')
		b.WriteString(toks[j+1].text)
		j += 2
	}
	return b.String(), j, true
}

// importDecl reads the rest of an import declaration starting after "import".
func importDecl(toks []token, i int) (imp, int, bool) {
	var im imp
	if i < len(toks) && toks[i].kind == tokIdent && toks[i].text == "static" {
		// "static" can also be the first package of a non-static import only in
		// theory: it is a keyword, so no package has that name.
		im.static = true
		i++
	}
	name, next, ok := dotted(toks, i)
	if !ok {
		return im, i, false
	}
	im.name = name
	if next+1 < len(toks) && toks[next].kind == tokPunct && toks[next].op == '.' &&
		toks[next+1].kind == tokPunct && toks[next+1].op == '*' {
		im.onDemand = true
		next += 2
	}
	return im, next, true
}

// typeName returns the name a type declaration starting with the keyword at toks[i]
// declares.
func typeName(toks []token, i int) (string, bool) {
	if i+1 >= len(toks) || toks[i+1].kind != tokIdent {
		return "", false
	}
	if toks[i].text == "record" {
		if i+2 >= len(toks) || toks[i+2].kind != tokPunct || (toks[i+2].op != '(' && toks[i+2].op != '<') {
			return "", false
		}
	}
	return toks[i+1].text, true
}

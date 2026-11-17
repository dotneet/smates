package js

import (
	"regexp"
	"sort"
)

// coarseLiteral is a single-line string literal (s1, s2), or a template literal without
// substitutions (s3), possibly after a magic comment.
const coarseLiteral = `(?:/\*[^*]*\*/\s*)?` +
	`(?:'(?P<s1>(?:[^'\\\n\r]|\\.)*)'|"(?P<s2>(?:[^"\\\n\r]|\\.)*)"|` + "`(?P<s3>[^`$\\\\]*)`" + `)`

// coarsePatterns match the module-loading forms the lexer recognizes, written with a
// string literal. They are applied to the raw text, comments and strings included, and
// each on its own, so that one match does not hide another (a mock function's type
// arguments may hold an import()).
var coarsePatterns = []*regexp.Regexp{
	// import "x", import("x"), from "x" (import and export declarations), require("x"),
	// require.resolve("x"), import.meta.resolve("x").
	regexp.MustCompile(`(?:^|[^\w$.])(?:` +
		`(?:import|require)\s*\(\s*` +
		`|(?:import|from)\s*` +
		`|require\s*\.\s*resolve\s*\(\s*` +
		`|import\s*\.\s*meta\s*\.\s*resolve\s*\(\s*` +
		`)` + coarseLiteral),
	// The vi/jest/vitest functions that take a specifier (fn), possibly with type
	// arguments, and with import(...), require(...) or require.resolve(...) around it.
	regexp.MustCompile(`(?:^|[^\w$.])(?:vi|jest|vitest)\s*\.\s*(?P<fn>\w+)\s*(?:<[^;\n]*?>)?\s*\(\s*` +
		`(?:(?:import|require(?:\s*\.\s*resolve)?)\s*\(\s*)?` + coarseLiteral),
	// new URL("x", import.meta.url): a file next to the importing one.
	regexp.MustCompile(`(?:^|[^\w$.])(?P<url>URL)\s*\(\s*` + coarseLiteral + `\s*,\s*import\s*\.\s*meta\s*\.\s*url\b`),
	// import(`./x/${name}.js`), require(`./x/${name}`): a template literal with a
	// static prefix loads any file under it (refTemplate, without the suffix: more).
	regexp.MustCompile("(?:^|[^\\w$.])(?:import|require)\\s*\\(\\s*`(?P<tpl>[^`$\\\\]+)\\$\\{"),
	// import.meta.glob("./x/*.ts"), import.meta.glob(["./a/*.ts", ...]) (its first
	// pattern), with type arguments too.
	regexp.MustCompile(`(?:^|[^\w$.])import\s*\.\s*meta\s*\.\s*(?P<glob>glob)(?:Eager)?\s*(?:<[^;\n]*?>)?\s*\(\s*\[?\s*` + coarseLiteral),
	// require.context("./x"): the directory, with its subdirectories (more).
	regexp.MustCompile(`(?:^|[^\w$.])require\s*\.\s*(?P<ctx>context)\s*\(\s*` + coarseLiteral),
}

// coarseDynamic matches, in what the lexer skipped as JSX, a load whose target is not
// one string: an import() or require() whose argument does not start with a string
// literal, a template literal with a substitution, import.meta.glob and
// require.context (whose array or options the coarse patterns may not read in full).
var coarseDynamic = regexp.MustCompile("(?:^|[^\\w$.])(?:(?:import|require)\\s*\\(\\s*(?:[^\\s'\"`)/]|`[^`]*\\$\\{)|import\\s*\\.\\s*meta\\s*\\.\\s*glob|require\\s*\\.\\s*context)")

// coarseRef is a reference coarseRefs found, with the offset of its specifier.
type coarseRef struct {
	ref
	pos int
}

// coarseRefs returns the references a scan of the raw text finds, without lexing:
// every string literal in a module-loading position (see coarsePatterns), wherever it
// is. It is a safety net under the lexer, which a construct it misreads (JSX text it
// takes for code, a regex it takes for a division) can lead to skip a stretch of code.
// What it finds may also come from comments or strings, which only adds dependencies.
func coarseRefs(src []byte) []coarseRef {
	var refs []coarseRef
	for _, re := range coarsePatterns {
		group := func(m []int, name string) (int, int, bool) {
			g := re.SubexpIndex(name)
			if g < 0 || m[2*g] < 0 {
				return 0, 0, false
			}
			return m[2*g], m[2*g+1], true
		}
		for _, m := range re.FindAllSubmatchIndex(src, -1) {
			start, end := -1, -1
			for _, name := range []string{"s1", "s2", "s3", "tpl"} {
				if s, e, ok := group(m, name); ok {
					start, end = s, e
					break
				}
			}
			if start < 0 || start == end {
				continue
			}
			r := ref{kind: refModule, spec: unescape(src[start:end])}
			if s, e, ok := group(m, "fn"); ok {
				mock, known := mockFns[string(src[s:e])]
				if !known {
					continue // vi.fn("x"), jest.spyOn(...)
				}
				r.mock = mock
			}
			if _, _, ok := group(m, "url"); ok {
				r.kind = refFile
			}
			if _, _, ok := group(m, "tpl"); ok {
				r.kind = refTemplate
			}
			if _, _, ok := group(m, "glob"); ok {
				r.kind = refGlob
			}
			if _, _, ok := group(m, "ctx"); ok {
				r.kind, r.recursive = refContext, true
			}
			refs = append(refs, coarseRef{r, start})
		}
	}
	return refs
}

// withCoarse returns refs, the references the lexer found in src, plus those
// coarseRefs finds that the lexer missed. These are marked coarse (they may be a
// comment's or a string's, so they are not reported as unresolved), except in the
// spans the lexer read as JSX text or tags (jsx, sorted): there it skipped what
// would be code if it misjudged the JSX, so what is found there counts in full,
// including a load of a computed name (refDynamic).
func withCoarse(refs []ref, src []byte, jsx [][2]int) []ref {
	inJSX := func(pos int) bool {
		k := sort.Search(len(jsx), func(k int) bool { return jsx[k][1] > pos })
		return k < len(jsx) && jsx[k][0] <= pos
	}
	type key struct {
		kind refKind
		spec string
		mock bool
	}
	// seen maps the references found so far to their index in refs, -1 for the
	// lexer's.
	seen := map[key]int{}
	dynamic := false
	for _, r := range refs {
		seen[key{r.kind, r.spec, r.mock}] = -1
		dynamic = dynamic || r.kind == refDynamic
	}
	for _, c := range coarseRefs(src) {
		k := key{c.kind, c.spec, c.mock}
		c.coarse = !inJSX(c.pos)
		if i, ok := seen[k]; ok {
			if i >= 0 && !c.coarse {
				refs[i].coarse = false // also in JSX, besides a comment
			}
			continue
		}
		seen[k] = len(refs)
		refs = append(refs, c.ref)
	}
	if !dynamic {
		for _, s := range jsx {
			if coarseDynamic.Match(src[s[0]:s[1]]) {
				refs = append(refs, ref{kind: refDynamic})
				break
			}
		}
	}
	return refs
}

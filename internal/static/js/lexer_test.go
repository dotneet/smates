package js

import (
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/dotneet/smates/internal/static/host"
)

// scanCase is a source and the references scanSource must find in it.
type scanCase struct {
	name string
	src  string
	want []string
}

// runScanCases checks scan over cases.
func runScanCases(t *testing.T, cases []scanCase, scan func([]byte) []ref) {
	t.Helper()
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			got := refStrings(scan([]byte(tt.src)))
			want := tt.want
			if want == nil {
				want = []string{}
			}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("scan(%q)\n got: %q\nwant: %q", tt.src, got, want)
			}
		})
	}
}

// slashCases cover the regex-or-division decision for '/'. A wrong guess must never
// swallow (or expose) a backtick, which would invert the template state and hide every
// reference up to the next template literal.
var slashCases = []scanCase{
	// Divisions that used to be lexed as regexes.
	{"jsx self-closing after expression",
		"const C = () => <><Avatar src={u.a} /> <Link href={`/users/${u.id}`}>P</Link></>;\nimport('./Lazy')\nvi.mock('./api')",
		[]string{"m:./Lazy", "m:./api"}},
	{"jsx self-closing on one line",
		"const C = () => <><Image source={require('./a.png')} /><Image source={require('./b.png')} /></>",
		[]string{"m:./a.png", "m:./b.png"}},
	{"jsx self-closing after spread", "<Foo {...props}/>{`x`}\nrequire('./yes')", []string{"m:./yes"}},
	{"jsx closing tag then template", "const C = () => <p>Hi</p><a href={`/x`}>a</a>;\nrequire('./yes')",
		[]string{"m:./yes"}},
	{"jsx text division between expressions", "const C = () => <span>{page} / {`${total}`} / {n}</span>;\nimport('./yes')",
		[]string{"m:./yes"}},
	{"jsx text division with require", "<span>{a} / {require('./b')}</span>", []string{"m:./b"}},
	{"non-null assertion", "const half = width! / 2; const url = `${base}/x`;\nimport('./later')",
		[]string{"m:./later"}},
	{"non-null assertion after call and index", "const a = f()! / 2 + `/`; const b = xs[i]! / 2 + `/`; const c = this.w! / 2 + `/`;\nimport('./yes')",
		[]string{"m:./yes"}},
	{"postfix increment", "const h = i++ / 2; const u = `${a}/b`;\nimport('./yes')", []string{"m:./yes"}},
	{"postfix decrement", "const h = i-- / 2; const u = `${a}/b`;\nimport('./yes')", []string{"m:./yes"}},
	{"object literal then division", "const r = {a: 1}.a / 2 + `/`;\nrequire('./yes')", []string{"m:./yes"}},

	// Genuine regexes must still be recognised, so that the literals after them are.
	{"regex of backtick after assignment", "x = /`/; import './yes'; const t = `a`", []string{"m:./yes"}},
	{"regex of backtick as argument", "s.replace(/`/g, '\\\\`'); import('./yes'); const t = `a`", []string{"m:./yes"}},
	{"regex of gt as argument", `s.replace(/>/g, "&gt;"); require("./yes")`, []string{"m:./yes"}},
	{"regex after if head", "if (a) /re/.test(s) && require('./yes')", []string{"m:./yes"}},
	{"regex after while head", "while (x) /a/.exec(s); require('./yes')", []string{"m:./yes"}},
	{"regex after block", "function f() {}\n/foo/.test(x) && require('./yes')", []string{"m:./yes"}},
	{"regex after block with quote", "function f() {}\n/'/.test(x)\nrequire('./yes')", []string{"m:./yes"}},
	{"regex with escaped slash", "function f() { return /a\\/b/.test(s) ? require('./yes') : 0 }", []string{"m:./yes"}},
	{"regex with quote after return", `function f() { return /'/.test(s) ? "a" : require('./yes') }`, []string{"m:./yes"}},
	{"regex after minified return not", "function f(s){return!/'/.test(s)&&require('./yes')}", []string{"m:./yes"}},
	{"regex after arrow", "const f = () => /x/.test(s); import './yes'", []string{"m:./yes"}},
	{"regex with slash and backtick in class", "const re = /[/'`]/; require('./yes'); const t = `x`", []string{"m:./yes"}},
	{"regex after binary plus following increment", "n = i+++/`/.source.length; require('./yes'); t = `x`", []string{"m:./yes"}},
	{"regex after prefix not", "const ok = !/`/.test(s); require('./yes'); t = `x`", []string{"m:./yes"}},
	{"regex after keyword", "if (x instanceof /`/.constructor) require('./yes'); t = `x`", []string{"m:./yes"}},
	{"regex in template substitution", "const s = `${/`/.source}`; require('./yes')", []string{"m:./yes"}},

	// UTF-8 byte order mark.
	{"bom", "\xef\xbb\xbfimport './setup'", []string{"m:./setup"}},
	{"bom and hashbang", "\xef\xbb\xbf#!/usr/bin/env node\nrequire('./cli')", []string{"m:./cli"}},
}

func TestScanSourceSlash(t *testing.T) { runScanCases(t, slashCases, scanSource) }

// jsxCases cover JSX text, attribute values and tags: what they hold is not code, so a
// '/*', '//', quote, backtick or slash in them must neither hide nor expose anything.
var jsxCases = []scanCase{
	{"text with block comment opener",
		"export const A = () => <p>Run src/* now</p>;\nconst m = await import('./late');\njest.mock('./mocked');",
		[]string{"m:./late", "m:./mocked"}},
	{"text with line comment opener", "<a>http://x</a>; import('./ju')", []string{"m:./ju"}},
	{"text with backtick", "const C = () => <p>Run `npm test` first</p>;\nimport('./yes')", []string{"m:./yes"}},
	{"text with unbalanced quotes on two lines", "const C = () => <p>it's</p>;\nconst D = () => <b>\"</b>;\nimport('./yes')",
		[]string{"m:./yes"}},
	{"text with slashes", "const C = () => <p>a / b / `c` / d</p>;\nimport('./yes')", []string{"m:./yes"}},
	{"text with regex-like slashes after expression", "const C = () => <p>{a} /'/ {b}</p>;\nimport('./yes')", []string{"m:./yes"}},
	{"fragment text", "const C = () => <>// not a comment `x</>;\nimport('./yes')", []string{"m:./yes"}},
	{"attribute values with comment openers and quotes",
		"const C = () => <a href=\"http://x/*\" title='it\"s' data-x=\"`\">t</a>;\nimport('./yes')", []string{"m:./yes"}},
	{"multi-line attribute value", "const C = () => <a title=\"one\ntwo's\">t</a>;\nimport('./yes')", []string{"m:./yes"}},
	{"comment between attributes", "const C = () => <a /* it's */ b={1} // `x\n>t</a>;\nimport('./yes')", []string{"m:./yes"}},
	{"nested elements and containers",
		"const C = () => <div>{cond && <p>it's {x} `y`</p>}<br/>{list.map(i => <i key={i}>/*</i>)}</div>;\nimport('./yes')",
		[]string{"m:./yes"}},
	{"object literal in container", "const C = () => <p style={{a: '/*'}}>it's</p>;\nimport('./yes')", []string{"m:./yes"}},
	{"member expression tag", "const C = () => <Foo.Bar x:y=\"1\" data-a>it's</Foo.Bar>;\nimport('./yes')", []string{"m:./yes"}},
	{"generic component", "const C = () => <Select<Option> value={v}>it's</Select>;\nimport('./yes')", []string{"m:./yes"}},
	{"element in template substitution", "const s = `${<b>it's</b>}`; import('./yes')", []string{"m:./yes"}},
	{"element after return", "function C() { return <p>/* x</p> }\nimport('./yes')", []string{"m:./yes"}},
	{"element after await", "const C = async () => await <p>it's</p>;\nimport('./yes')", []string{"m:./yes"}},
	{"loads in containers are found", "const C = () => <p>{require('./img.png')} {import('./lazy')}</p>", []string{"m:./img.png", "m:./lazy"}},
	{"loads in text are not code", "const C = () => <p>import('./no') require('./no2')</p>", nil},

	// TypeScript type parameter lists in .tsx are not elements.
	{"type parameter with trailing comma", "const f = <T,>(x: T) => x;\nconst s = `a`; import('./yes')", []string{"m:./yes"}},
	{"type parameter with constraint", "const f = <T extends object>(x: T) => x;\nconst s = `a`; import('./yes')", []string{"m:./yes"}},
	{"type parameter with default", "const f = <T = string,>(x: T) => x;\nconst s = `a`; import('./yes')", []string{"m:./yes"}},
	{"const type parameter", "const f = <const T,>(x: T) => x;\nconst s = `a`; import('./yes')", []string{"m:./yes"}},
	{"comparison is no element", "if (a <b && c > d) x = 1;\nconst s = `a`; import('./yes')", []string{"m:./yes"}},
	{"generic call is no element", "const m = useState<string>('x');\nconst s = `a`; import('./yes')", []string{"m:./yes"}},
}

func TestScanSourceJSX(t *testing.T) { runScanCases(t, jsxCases, scanSource) }

// tsCases are TypeScript (.ts) sources, lexed without JSX: <T>x is a type assertion.
var tsCases = []scanCase{
	{"type assertion", "const x = <Foo>bar; const s = `a`; import('./yes')", []string{"m:./yes"}},
	{"type assertion in expression", "f(<any>window, <string>`t`);\nimport('./yes')", []string{"m:./yes"}},
	{"generic arrow", "const f = <T>(x: T): T => x;\nimport('./yes')", []string{"m:./yes"}},
}

func TestScanCodeTypeScript(t *testing.T) {
	runScanCases(t, tsCases, func(src []byte) []ref { return scanCode(src, false) })
}

// headCases cover a '/' after the ')' of a control statement's head and after the '}'
// of a block: a statement starts, so it is a regex even when its body holds a quote or
// backtick.
var headCases = []scanCase{
	{"backtick regex after if block", "if (a) {}\n/`/.test(s)\nconst x = require('./after');", []string{"m:./after"}},
	{"quote regex after if head", "if (x) /'/.test(s); import('./same')", []string{"m:./same"}},
	{"double quote regex after while head", "while (x) /\"/.test(s); import('./yes')", []string{"m:./yes"}},
	{"backtick regex after for head", "for (;;) /`/.test(s); require('./yes'); t = `x`", []string{"m:./yes"}},
	{"quote regex after for await head", "for await (const x of y) /'/.test(x); import('./yes')", []string{"m:./yes"}},
	{"quote regex after with head", "with (o) /'/.test(s); import('./yes')", []string{"m:./yes"}},
	{"regex after else block", "if (a) {} else {}\n/`/.test(s); require('./yes'); t = `x`", []string{"m:./yes"}},
	{"regex after try finally", "try {} finally {}\n/'/.test(s); import('./yes')", []string{"m:./yes"}},
	{"regex after do block", "do {} while (x)\n/`/.test(s); require('./yes'); t = `x`", []string{"m:./yes"}},
	{"regex after function body", "function f() {}\n/`/.test(x); require('./yes'); t = `x`", []string{"m:./yes"}},
	{"regex after class body", "class A extends B {}\n/'/.test(s); import('./yes')", []string{"m:./yes"}},
	{"regex after arrow body", "const f = () => {}\n/'/.test(s); import('./yes')", []string{"m:./yes"}},
	{"regex after switch body", "switch (x) {}\n/`/.test(s); require('./yes'); t = `x`", []string{"m:./yes"}},
	{"regex after nested block", "{ {} }\n/`/.test(s); require('./yes'); t = `x`", []string{"m:./yes"}},
	{"regex after typed function body", "function f(): void {}\n/'/.test(s); import('./yes')", []string{"m:./yes"}},
	{"regex after generic return type body", "function f(): Promise<T> {}\n/`/.test(s); require('./yes'); t = `x`", []string{"m:./yes"}},
	{"regex after interface body", "interface I { a: string }\n/`/.test(s); require('./yes'); t = `x`", []string{"m:./yes"}},
	// Not heads, not blocks: a slash divides.
	{"method named if", "x.if(a) / 2; const s = `a`; import('./yes')", []string{"m:./yes"}},
	{"method named for", "x.for(a) / 2; const s = `a`; import('./yes')", []string{"m:./yes"}},
	{"call then division", "f(a) / 2 + `/`; import('./yes')", []string{"m:./yes"}},
	{"object literal value then division", "const o = {a: 1}; const r = o / 2 + `/`; import('./yes')", []string{"m:./yes"}},
	{"division with quoted operand after object literal", "x = {} / 'a' / 2; import('./yes')", []string{"m:./yes"}},
	{"division with template operand after object literal", "x = {} / `a` / 2; import('./yes')", []string{"m:./yes"}},
	// After an object literal a slash may still start a regex followed like one.
	{"regex after object literal statement", "const o = {}\n/'/.test(s) && import('./yes')", []string{"m:./yes"}},
	{"regex after comparison", "if (a > /'/.lastIndex) import('./yes')", []string{"m:./yes"}},
}

func TestScanSourceHeadsAndBlocks(t *testing.T) { runScanCases(t, headCases, scanSource) }

// keywordCases cover contextual keywords used as identifiers, after which a slash
// divides: of (a keyword only in a for head), await and yield (in scripts).
var keywordCases = []scanCase{
	{"of as identifier", "const of = 4; const r = of / 2; const s = `x`; import('./a1')", []string{"m:./a1"}},
	{"of as identifier with quote", "const r = of / 2 + '/'; import('./yes')", []string{"m:./yes"}},
	{"of bound in a for head", "for (const of of xs) of / 2; t = `x`; import('./yes')", []string{"m:./yes"}},
	{"of as keyword before a regex", "for (const x of /'/.exec(s)) {}\nimport('./yes')", []string{"m:./yes"}},
	{"of property", "const r = a.of / 2 + `/`; import('./yes')", []string{"m:./yes"}},
	{"await as identifier", "let await = 4; const r = await / 2; const s = `x`; import('./yes')", []string{"m:./yes"}},
	{"yield as identifier", "var yield = 6; x = yield / 3; t = `x`; import('./yes')", []string{"m:./yes"}},
	{"await before a regex", "async function f() { return await /'/.exec(s) }\nimport('./yes')", []string{"m:./yes"}},
	{"yield before a regex", "function* g() { yield /`/; }\nrequire('./yes'); t = `x`", []string{"m:./yes"}},
	{"keyword property", "const r = x.return / 2 + `/`; import('./yes')", []string{"m:./yes"}},
}

func TestScanSourceContextualKeywords(t *testing.T) { runScanCases(t, keywordCases, scanSource) }

// TestScanSentinel appends an import of ./__sentinel__ after every source of the scan
// tables: whatever a source holds, what follows it is found. The lexer alone must find
// it unless the source leaves a comment open; refsOf, with the coarse scan, always.
func TestScanSentinel(t *testing.T) {
	const sentinel = "m:./__sentinel__"
	open := map[string]bool{"unterminated block comment": true} // the sentinel is in the comment
	var corpus []scanCase
	for _, cases := range [][]scanCase{scanSourceCases, typeOnlyCases, slashCases, jsxCases, headCases, keywordCases, mockArgCases} {
		corpus = append(corpus, cases...)
	}
	for i, src := range arrowRegexSrcs {
		corpus = append(corpus, scanCase{name: "arrow regex " + string(rune('a'+i)), src: src})
	}
	for _, tt := range corpus {
		src := tt.src + "\n;import('./__sentinel__')"
		if !open[tt.name] && !slices.Contains(refStrings(scanSource([]byte(src))), sentinel) {
			t.Errorf("%s: the lexer misses the sentinel after %q", tt.name, tt.src)
		}
		for _, file := range []string{"a.tsx", "a.ts", "a.js", "a.mts"} {
			if !slices.Contains(refStrings(refsOf(file, []byte(src))), sentinel) {
				t.Errorf("%s (%s): refsOf misses the sentinel after %q", tt.name, file, tt.src)
			}
		}
	}
	for _, tt := range tsCases {
		src := tt.src + "\n;import('./__sentinel__')"
		if !slices.Contains(refStrings(scanCode([]byte(src), false)), sentinel) {
			t.Errorf("%s: the lexer misses the sentinel after %q", tt.name, tt.src)
		}
	}
}

// TestLexerLinearJSX guards against pathological JSX: unterminated attribute values,
// unclosed elements and containers must lex in linear time.
func TestLexerLinearJSX(t *testing.T) {
	var b strings.Builder
	for i := 0; i < 20000; i++ {
		b.WriteString("x = <a b=\"oops\ny = <p>it's {z} <i>\n")
	}
	b.WriteString("import z from './z'")
	if refs := refStrings(refsOf("a.jsx", []byte(b.String()))); !slices.Contains(refs, "m:./z") {
		t.Errorf("got %q", refs)
	}
}

// TestCoarseRefs covers the safety net on its own: every module-loading form with a
// literal, wherever it is, and nothing that only resembles one.
func TestCoarseRefs(t *testing.T) {
	src := strings.Join([]string{
		`import a from "./a"`,
		`import './side'`,
		`export * from './star'`,
		`export { x } from "./named"`,
		`const l = await import(/* webpackChunkName: "c" */ './lazy')`,
		"const t = import(`./tmpl`)",
		`const r = require ( './req' )`,
		`require.resolve('./res')`,
		`import.meta.resolve("./meta")`,
		`vi.mock('./mocked')`,
		`jest.mock(import('./mocked-import'))`,
		`vitest.doMock(require.resolve('./mocked-resolve'))`,
		`jest.requireActual<typeof import('./t')>('./actual')`,
		`new Worker(new URL('./worker.ts', import.meta.url))`,
		// Not module loads.
		`obj.import('./no1'); myrequire('./no2'); vi.fn('./no3'); new URL('/no4', location.origin)`,
		"import(`./no5/${x}`)",
		`/* import x from './in-comment' */`,
	}, "\n")
	var got []string
	for _, r := range coarseRefs([]byte(src)) {
		if want := []byte(r.spec); string(src[r.pos:r.pos+len(want)]) != r.spec {
			t.Errorf("%q: pos %d is not the specifier's", r.spec, r.pos)
		}
		s := refStrings([]ref{r.ref})[0]
		if r.mock {
			s += " (mock)"
		}
		got = append(got, s)
	}
	// Each form is matched on its own: a load inside a mock call is found by both.
	want := []string{
		"m:./a", "m:./side", "m:./star", "m:./named", "m:./lazy", "m:./tmpl", "m:./req", "m:./res", "m:./meta",
		"m:./mocked-import", "m:./mocked-resolve", "m:./t", "m:./in-comment",
		"m:./mocked (mock)", "m:./mocked-import (mock)", "m:./mocked-resolve (mock)", "m:./actual",
		"f:./worker.ts",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("coarseRefs\n got: %q\nwant: %q", got, want)
	}
}

// TestWithCoarse checks that the coarse scan only adds what the lexer missed, marked
// coarse, and that a coarse reference is never reported as unresolved.
func TestWithCoarse(t *testing.T) {
	src := []byte("import type { A } from './t'\njest.mock('./m')\n// import x from './c'\n")
	refs := refsOf("a.ts", src)
	var got []string
	for _, r := range refs {
		s := refStrings([]ref{r})[0]
		if r.coarse {
			s += " (coarse)"
		}
		got = append(got, s)
	}
	want := []string{"m:./t (type)", "m:./m", "m:./c (coarse)"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("refsOf\n got: %q\nwant: %q", got, want)
	}
	h := newHost(t, map[string]string{"src/a.ts": "// import x from './gone'\nconst s = \"require('virtual:x')\"\n"})
	if u := New(h, Options{}).Unresolved("src/a.ts", h.Read("src/a.ts")); len(u) != 0 {
		t.Errorf("Unresolved = %q, want none", u)
	}
}

// TestJSXSpansKeepUnresolved checks the case where the lexer takes code for JSX (a
// Flow type parameter list in a .js file, <T>(x) => ..., reads as an element): what it
// then skips is still reported as unresolved or dynamic, as it was before JSX was
// recognized. Must keep: the lexer cannot tell it misjudged the element.
func TestJSXSpansKeepUnresolved(t *testing.T) {
	tests := []struct {
		name, src string
		want      []string
	}{
		{"unresolved import in skipped text",
			"const f = <T>(x: T): T => x;\nimport v from 'virtual:mod'\n", []string{"virtual:mod"}},
		{"dynamic load in skipped text",
			"const f = <T>(x: T): T => x;\nconst m = await import(name)\n", []string{host.DynamicSpec}},
		{"dynamic template load in skipped text",
			"const f = <T>(x: T): T => x;\nconst m = require(`${dir}/x.js`)\n", []string{host.DynamicSpec}},
		{"also mentioned in a comment",
			"// import v from 'virtual:mod'\nconst f = <T>(x: T): T => x;\nimport v from 'virtual:mod'\n", []string{"virtual:mod"}},
		// Real JSX text naming a module is reported too: more is safe.
		{"jsx text", "const C = () => <p>import('virtual:doc')</p>\n", []string{"virtual:doc"}},
		{"comment only", "// import v from 'virtual:mod'\nconst C = () => <p>hi</p>\n", nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHost(t, map[string]string{"src/a.js": tt.src})
			got := New(h, Options{}).Unresolved("src/a.js", h.Read("src/a.js"))
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("Unresolved = %q, want %q", got, tt.want)
			}
		})
	}
}

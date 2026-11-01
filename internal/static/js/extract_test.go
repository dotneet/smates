package js

import (
	"reflect"
	"strings"
	"testing"
)

// refStrings renders refs compactly: m:spec, f:spec, t:prefix|suffix, g:pattern,
// c:dir|r (recursive) or c:dir|n.
func refStrings(refs []ref) []string {
	out := []string{}
	for _, r := range refs {
		switch r.kind {
		case refModule:
			out = append(out, "m:"+r.spec)
		case refFile:
			out = append(out, "f:"+r.spec)
		case refTemplate:
			out = append(out, "t:"+r.spec+"|"+r.suffix)
		case refGlob:
			out = append(out, "g:"+r.spec)
		case refContext:
			flag := "n"
			if r.recursive {
				flag = "r"
			}
			out = append(out, "c:"+r.spec+"|"+flag)
		}
	}
	return out
}

func TestScanSource(t *testing.T) {
	tests := []struct {
		name string
		src  string
		want []string
	}{
		{"default import", `import a from "./a"`, []string{"m:./a"}},
		{"named and namespace imports", "import { a, b as c } from './a';\nimport * as ns from './b'", []string{"m:./a", "m:./b"}},
		{"multi-line import", "import {\n  a,\n  b,\n} from\n  './a'", []string{"m:./a"}},
		{"side-effect import", `import "./polyfill"; import './styles.css'`, []string{"m:./polyfill", "m:./styles.css"}},
		{"type import", `import type { T } from "./types"; import { type U } from "./u"`, []string{"m:./types", "m:./u"}},
		{"import attributes", `import data from "./d.json" with { type: "json" }`, []string{"m:./d.json"}},
		{"export from", `export { a } from "./a"; export * from './b'; export * as c from "./c"; export type { T } from "./t"`,
			[]string{"m:./a", "m:./b", "m:./c", "m:./t"}},
		{"minified export star", `export*from"./a";import{x}from"./b"`, []string{"m:./a", "m:./b"}},
		{"dynamic import", `const m = await import("./lazy"); import('./other').then(f)`, []string{"m:./lazy", "m:./other"}},
		{"dynamic import with options", `import("./x.json", { with: { type: "json" } })`, []string{"m:./x.json"}},
		{"dynamic import magic comment", `import(/* webpackChunkName: "x" */ "./chunk")`, []string{"m:./chunk"}},
		{"type-level import", `type T = typeof import("./types").T`, []string{"m:./types"}},
		{"template without substitutions", "import(`./plain`)", []string{"m:./plain"}},
		{"template with substitution", "import(`./plugins/${name}.ts`)", []string{"t:./plugins/|.ts"}},
		{"template with several substitutions", "import(`./l/${a}/x/${b}.json`)", []string{"t:./l/|.json"}},
		{"template with no static prefix", "import(`${base}/x.js`)", []string{"t:|/x.js"}},
		{"dynamic import of variable", `import(name)`, nil},
		{"require", `const a = require("./a"); const {b} = require('b')`, []string{"m:./a", "m:b"}},
		{"require template", "require(`./locale/${l}`)", []string{"t:./locale/|"}},
		{"concatenation", `require("./locale/" + lang); import("./p/" + a + "/" + b + ".js"); require("./x/" + f(a, b))`,
			[]string{"t:./locale/|", "t:./p/|.js", "t:./x/|"}},
		{"require.resolve", `require.resolve("./worker.js")`, []string{"m:./worker.js"}},
		{"module.require", `module.require("./a")`, []string{"m:./a"}},
		{"import equals require", `import fs = require("./fs-shim")`, []string{"m:./fs-shim"}},
		{"new URL", `new Worker(new URL("./worker.ts", import.meta.url))`, []string{"f:./worker.ts"}},
		{"new URL without import.meta.url", `new URL("/api", location.origin)`, nil},
		{"import.meta.resolve", `import.meta.resolve("./x")`, []string{"m:./x"}},
		{"import.meta.glob string", `import.meta.glob("./pages/*.tsx", { eager: true })`, []string{"g:./pages/*.tsx"}},
		{"import.meta.glob array", `import.meta.glob(["./a/*.ts", "!./a/skip.ts"])`, []string{"g:./a/*.ts", "g:!./a/skip.ts"}},
		{"require.context", `require.context("./icons", false, /\.svg$/); require.context('./c')`,
			[]string{"c:./icons|n", "c:./c|r"}},
		{"vitest mocks", `vi.mock("./db"); vi.doMock('./x'); await vi.importActual("./y"); vi.importMock("./z")`,
			[]string{"m:./db", "m:./x", "m:./y", "m:./z"}},
		{"jest mocks", `jest.mock("./db", () => ({})); jest.requireActual('./a'); jest.unmock("./b"); jest.setMock("./c", {})`,
			[]string{"m:./db", "m:./a", "m:./b", "m:./c"}},
		{"vi.mock with dynamic import", `vi.mock(import("./svc"))`, []string{"m:./svc"}},
		{"other method named mock", `server.mock("./x"); vi.fn("./y")`, nil},
		{"property named import", `loader.import("./x"); obj.from("./y")`, nil},
		{"triple-slash references", "/// <reference path=\"globals.d.ts\" />\n/// <reference types=\"vite/client\" />\nlet x",
			[]string{"f:globals.d.ts", "m:vite/client"}},
		{"hashbang", "#!/usr/bin/env node\nrequire('./cli')", []string{"m:./cli"}},
		{"string escapes", `import a from "./\x61b\u{63}"`, []string{"m:./abc"}},

		// Lexer edge cases: references must not be taken from comments, strings,
		// regular expressions or JSX text, nor hidden by them.
		{"line comment", "// import a from './no'\nimport b from './yes'", []string{"m:./yes"}},
		{"block comment", "/* require('./no')\n import './no2' */ require('./yes')", []string{"m:./yes"}},
		{"comment inside import", `import /* from "./no" */ a from "./yes"`, []string{"m:./yes"}},
		{"string containing require", `const s = "require('./no')"; const t = 'import "./no"'; require("./yes")`,
			[]string{"m:./yes"}},
		{"template containing import", "const s = `import x from './no'`; import y from './yes'", []string{"m:./yes"}},
		{"string with escaped quote", `const s = "a\" require('./no') \""; require('./yes')`, []string{"m:./yes"}},
		{"regex with quote", `const re = /"/; import a from "./yes"; const q = '"'`, []string{"m:./yes"}},
		{"regex with comment-like body", `const re = /\/*/; import a from "./yes" // */`, []string{"m:./yes"}},
		{"regex with slash in class", `const re = /[/"]/g; require("./yes")`, []string{"m:./yes"}},
		{"regex after return", "function f(){ return /'/.test(s) }\nrequire('./yes')", []string{"m:./yes"}},
		{"division is not regex", `const x = a / b; const y = (c) / 2; require("./yes"); const z = d / e`,
			[]string{"m:./yes"}},
		{"division then string", `const r = total / count + "/"; import "./yes"`, []string{"m:./yes"}},
		{"nested templates", "const s = `a ${`b ${import('./inner')} ${\"x\"}`} c`; import('./after')",
			[]string{"m:./inner", "m:./after"}},
		{"template with braces in substitution", "const s = `${ {a: 1}.a } ${fn({b: `x`})}`; require('./yes')",
			[]string{"m:./yes"}},
		{"template with import in substitution", "const s = `${await import('./sub')}`", []string{"m:./sub"}},
		{"jsx text with apostrophes", "const C = () => <p>Don't do it, it's bad</p>;\nimport x from './yes';",
			[]string{"m:./yes"}},
		{"jsx with expressions", "const C = () => (\n  <div className=\"a\" onClick={() => require('./h')}>\n    {items.map(i => <Item key={i} />)}\n  </div>\n);",
			[]string{"m:./h"}},
		{"typescript generics", "const m = new Map<string, Array<number>>();\nimport a from './yes'", []string{"m:./yes"}},
		{"unterminated string stops at newline", "const s = 'oops\nimport a from './yes'", []string{"m:./yes"}},
		{"unterminated block comment", "require('./yes') /* never closed", []string{"m:./yes"}},
		{"identifier containing keyword", `myimport("./no"); requireX("./no"); fromage "./no"`, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := refStrings(scanSource([]byte(tt.src)))
			want := tt.want
			if want == nil {
				want = []string{}
			}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("scanSource(%q)\n got: %q\nwant: %q", tt.src, got, want)
			}
		})
	}
}

func TestScanSFC(t *testing.T) {
	tests := []struct {
		name  string
		src   string
		astro bool
		want  []string
	}{
		{
			name: "vue script setup and script",
			src: `<template>
  <div :class="` + "`a-${b}`" + `">It's {{ msg }}</div>
  <img src="./logo.png" alt="">
  <img :src="require('./icon.svg')">
</template>
<script>
export default { name: "x" }
</script>
<script setup lang="ts">
import Child from './Child.vue'
import { useX } from "@/composables/x"
</script>
<style scoped src="./style.css"></style>
<style>.a { background: url("./no.png") }</style>`,
			want: []string{"m:./icon.svg", "f:./logo.png", "m:./Child.vue", "m:@/composables/x", "f:./style.css"},
		},
		{
			name: "script src attribute",
			src:  `<script src="./logic.ts" lang="ts"></script><template><div/></template>`,
			want: []string{"f:./logic.ts"},
		},
		{
			name: "uppercase tag and self-closing script",
			src:  "<SCRIPT>import a from './a'</SCRIPT>\n<script src='./b.js' />",
			want: []string{"m:./a", "f:./b.js"},
		},
		{
			name: "svelte module script and markup import",
			src: `<script context="module">
  export const prerender = true;
  import { load } from './load';
</script>
<script>
  import Button from '$lib/Button.svelte';
</script>
<!-- <script>import './commented'</script> -->
{#await import('./Lazy.svelte') then m}<svelte:component this={m.default}/>{/await}`,
			want: []string{"m:./load", "m:$lib/Button.svelte", "m:./Lazy.svelte"},
		},
		{
			name:  "astro frontmatter",
			src:   "---\nimport Layout from '../layouts/Layout.astro';\nconst x = 1;\n---\n<Layout><script>import './client.ts'</script></Layout>",
			astro: true,
			want:  []string{"m:../layouts/Layout.astro", "m:./client.ts"},
		},
		{
			name: "unterminated script",
			src:  "<script>import a from './a'",
			want: []string{"m:./a"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := refStrings(scanSFC([]byte(tt.src), tt.astro))
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("scanSFC\n got: %q\nwant: %q", got, tt.want)
			}
		})
	}
}

func TestUnescape(t *testing.T) {
	tests := map[string]string{
		`plain`:        "plain",
		`a\'b`:         "a'b",
		`\x41B\u{43}`:  "ABC",
		"line\\\ncont": "linecont",
		`\n\t`:         "\n\t",
		`bad\xZZ`:      "badxZZ",
		`bad\u{zz}`:    "badu{zz}",
	}
	for in, want := range tests {
		if got := unescape([]byte(in)); got != want {
			t.Errorf("unescape(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestAttrValue(t *testing.T) {
	tests := []struct {
		in, name, want string
		ok             bool
	}{
		{`<reference path="a.d.ts" />`, "path", "a.d.ts", true},
		{`<reference path='b' />`, "path", "b", true},
		{`<script src = "./x.ts">`, "src", "./x.ts", true},
		{`<img src=./y.png>`, "src", "./y.png", true},
		{`<img srcset="./z.png">`, "src", "", false},
		{`<img data-src="./z.png">`, "src", "", false},
		{`<img src="unterminated`, "src", "", false},
	}
	for _, tt := range tests {
		got, ok := attrValue([]byte(tt.in), tt.name)
		if got != tt.want || ok != tt.ok {
			t.Errorf("attrValue(%q, %q) = %q, %v; want %q, %v", tt.in, tt.name, got, ok, tt.want, tt.ok)
		}
	}
}

// TestLexerLinear guards against pathological inputs: deeply nested templates,
// thousands of unterminated literals and huge files must lex in linear time.
func TestLexerLinear(t *testing.T) {
	var b strings.Builder
	for i := 0; i < 20000; i++ {
		b.WriteString("a = b / c; d = '/ unterminated\n x = `${`${y}`}` /re[/]/g\n")
	}
	b.WriteString("import z from './z'")
	refs := refStrings(scanSource([]byte(b.String())))
	if len(refs) != 1 || refs[0] != "m:./z" {
		t.Errorf("got %q", refs)
	}
}

// TestArrowRegexBacktick checks that a regex after => may contain a backtick (it must
// not open a template literal) and that `x.return / 2` is a division.
func TestArrowRegexBacktick(t *testing.T) {
	for _, src := range []string{
		"export const isFence = (l) => /^```/.test(l)\nexport async function load() { return import('./lazy') }\n",
		"const g = (x: number): boolean => /`/.test(String(x))\nrequire('./a')",
		"const z = x.return / 2; require('./a') / 3",
	} {
		if got := refStrings(scanSource([]byte(src))); len(got) != 1 {
			t.Errorf("%q: got %q", src, got)
		}
	}
}

package js

import (
	"reflect"
	"testing"
)

func TestImports(t *testing.T) {
	cases := []struct {
		name string
		file string
		src  string
		want []Import
	}{
		{name: "value and type-only declarations", file: "a.ts",
			src: `import type { A } from "types-pkg"
import { b } from "./b"
import { type C } from "elided-pkg"
export type { D } from "./d"
const e = require("e-pkg")
const f = await import("f-pkg")`,
			want: []Import{{Spec: "types-pkg", TypeOnly: true}, {Spec: "./b"}, {Spec: "elided-pkg"}, {Spec: "./d", TypeOnly: true}, {Spec: "e-pkg"}, {Spec: "f-pkg"}}},
		{name: "JavaScript has no type-only imports", file: "a.js", src: `import x from "x"`, want: []Import{{Spec: "x"}}},
		// The coarse safety net reads comments and strings too: more is safe.
		{name: "comments and strings come last, from the coarse scan", file: "a.ts", src: `// import x from "x"
import z from "z"
const s = "from 'y'"`,
			want: []Import{{Spec: "z"}, {Spec: "x"}, {Spec: "y"}}},
		{name: "a type-only import stays type-only beside the coarse scan", file: "a.ts", src: `import type { A } from "t"`,
			want: []Import{{Spec: "t", TypeOnly: true}}},
	}
	for _, c := range cases {
		if got := Imports(c.file, []byte(c.src)); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: Imports = %+v, want %+v", c.name, got, c.want)
		}
	}
}

func TestResolve(t *testing.T) {
	h := newHost(t, map[string]string{
		"package.json":                `{"workspaces":["packages/*"]}`,
		"packages/cfg/package.json":   `{"name":"@ws/cfg","main":"src/index.ts"}`,
		"packages/cfg/src/index.ts":   ``,
		"web/src/a.ts":                ``,
		"web/node_modules/x/index.js": ``,
	})
	a := New(h, Options{})
	for spec, want := range map[string][]string{
		"./a":     {"web/src/a.ts"},
		"@ws/cfg": {"packages/cfg/package.json", "packages/cfg/src/index.ts"},
		"x":       nil,
		"./gone":  nil,
	} {
		if got := a.Resolve("web/src/main.ts", spec); !reflect.DeepEqual(got, want) {
			t.Errorf("Resolve(%q) = %v, want %v", spec, got, want)
		}
	}
}

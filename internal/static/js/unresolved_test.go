package js

import (
	"reflect"
	"testing"
)

func TestUnresolved(t *testing.T) {
	files := map[string]string{
		"web/src/a.ts":                         ``,
		"web/src/b/index.ts":                   ``,
		"web/node_modules/react/package.json":  `{}`,
		"node_modules/@scope/lib/package.json": `{}`,
		"web/tsconfig.json":                    `{"compilerOptions":{"paths":{"#lib/*":["./src/*"]}}}`,
	}
	cases := []struct {
		name string
		src  string
		want []string
	}{
		{name: "relative files and directories", src: `import a from "./a"; import b from "./b"; import "./a.ts?raw"`},
		{name: "installed packages, also above the file", src: `import React from "react"; import { x } from "react/jsx-runtime"; import l from "@scope/lib/sub"`},
		{name: "node builtins", src: `import fs from "fs"; import p from "node:path"; import { readFile } from "fs/promises"`},
		{name: "tsconfig paths", src: `import a from "#lib/a"`},
		{name: "a missing relative file", src: `import x from "./missing"`, want: []string{"./missing"}},
		{name: "a bundler alias", src: `import data from "~mocks/data"; import "@/thing"`, want: []string{"~mocks/data", "@/thing"}},
		{name: "a package that is not installed", src: `import x from "not-installed"`, want: []string{"not-installed"}},
		{name: "a virtual module", src: `import icons from "virtual:icons"`, want: []string{"virtual:icons"}},
		{name: "type-only imports are erased", src: `import type { X } from "~types/x"; export type { Y } from "./missing"`},
		{name: "dynamic import", src: `const m = await import("./gone")`, want: []string{"./gone"}},
		{name: "patterns are not checked", src: "const m = import(`./pages/${name}.ts`); import.meta.glob('./nowhere/*.ts')"},
	}
	h := newHost(t, files)
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			a := New(h, Options{})
			if got := a.Unresolved("web/src/main.ts", []byte(c.src)); !reflect.DeepEqual(got, c.want) {
				t.Errorf("Unresolved = %q, want %q", got, c.want)
			}
		})
	}
}

func TestCallArgsAndPathValue(t *testing.T) {
	src := `import { storybookTest } from "@storybook/addon-vitest/vitest-plugin"
const opts = { configDir: ".sb" }
function storybookTest() {}
export default {
  plugins: [
    storybookTest(),
    storybookTest({ configDir: path.join(dirname, ".storybook") }),
    storybookTest(opts),
    other.storybookTest({ configDir: "x" }),
  ],
}`
	got := CallArgs([]byte(src), "storybookTest")
	if len(got) != 3 {
		t.Fatalf("CallArgs = %#v, want 3 calls", got)
	}
	if got[0] != nil {
		t.Errorf("call without arguments: %#v", got[0])
	}
	if o, _ := got[1].(*Object); o == nil || o.Fields["configDir"] != Unknown(`path.join(dirname, ".storybook")`) {
		t.Errorf("call with options: %#v", got[1])
	}
	if o, _ := got[2].(*Object); o == nil || o.Fields["configDir"] != ".sb" {
		t.Errorf("options through a binding: %#v", got[2])
	}

	cases := []struct {
		expr     string
		p        string
		fromFile bool
		ok       bool
	}{
		{`".storybook"`, ".storybook", false, true},
		{`path.join(dirname, ".storybook")`, ".storybook", true, true},
		{`path.resolve(__dirname, "..", "sb",)`, "../sb", true, true},
		{`join(import.meta.dirname, "config/sb")`, "config/sb", true, true},
		{`resolve(__dirname)`, ".", true, true},
		{`path.join(process.cwd(), ".storybook")`, "", false, false},
		{`path.join(dirname, name)`, "", false, false},
		{`path.resolve(dirname, "/abs")`, "", false, false},
		{`process.env.DIR`, "", false, false},
		{"`.storybook`", ".storybook", false, true},
	}
	for _, c := range cases {
		p, fromFile, ok := PathValue(c.expr)
		if p != c.p || fromFile != c.fromFile || ok != c.ok {
			t.Errorf("PathValue(%s) = %q, %v, %v; want %q, %v, %v", c.expr, p, fromFile, ok, c.p, c.fromFile, c.ok)
		}
	}
}

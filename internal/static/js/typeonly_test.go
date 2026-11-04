package js

import (
	"testing"
)

// typeOnlySrc imports ./types only for its types (always erased), ./inline through a
// specifier list of types only (erased unless the compiler options keep it), ./both
// for a type and a value, and ./value as a value.
const typeOnlySrc = `import type { A } from "./types"
import { type B } from "./inline"
import type { C } from "./both"
import { c } from "./both"
import { v } from "./value"
`

// typeOnlyFiles returns a fixture with src/a.test.ts importing typeOnlySrc's modules,
// plus extra files.
func typeOnlyFiles(test string, extra map[string]string) map[string]string {
	files := map[string]string{
		test:                  typeOnlySrc,
		"src/types.ts":        ``,
		"src/inline.ts":       ``,
		"src/both.ts":         ``,
		"src/value.ts":        ``,
		"src/unused.ts":       ``,
		"other/tsconfig.json": `{"compilerOptions":{"verbatimModuleSyntax":true}}`, // governs other/ only
	}
	for p, c := range extra {
		files[p] = c
	}
	return files
}

func TestSplitDeps(t *testing.T) {
	all := []string{"src/both.ts", "src/inline.ts", "src/types.ts", "src/value.ts"}
	elided := []string{"src/both.ts", "src/value.ts"}
	kept := []string{"src/both.ts", "src/inline.ts", "src/value.ts"}
	tests := []struct {
		name    string
		test    string // the importing file (default src/a.test.ts)
		extra   map[string]string
		keepOut []string // node_modules files present on disk but not in the working tree
		runtime []string
	}{
		{name: "no tsconfig", runtime: elided},
		{name: "javascript has no types", test: "src/a.test.js", runtime: all},
		{name: "mts", test: "src/a.test.mts", runtime: elided},
		{name: "tsx", test: "src/a.test.tsx", runtime: elided},
		{name: "plain tsconfig", extra: map[string]string{"tsconfig.json": `{"compilerOptions":{"strict":true}}`}, runtime: elided},
		{name: "verbatimModuleSyntax", extra: map[string]string{"tsconfig.json": `{"compilerOptions":{"verbatimModuleSyntax":true}}`}, runtime: kept},
		{name: "verbatimModuleSyntax false", extra: map[string]string{"tsconfig.json": `{"compilerOptions":{"verbatimModuleSyntax":false}}`}, runtime: elided},
		{name: "preserveValueImports", extra: map[string]string{"src/tsconfig.json": `{"compilerOptions":{"preserveValueImports":true}}`}, runtime: kept},
		{name: "importsNotUsedAsValues preserve", extra: map[string]string{"tsconfig.json": `{"compilerOptions":{"importsNotUsedAsValues":"preserve"}}`}, runtime: kept},
		{name: "importsNotUsedAsValues remove", extra: map[string]string{"tsconfig.json": `{"compilerOptions":{"importsNotUsedAsValues":"remove"}}`}, runtime: elided},
		{name: "unexpected value", extra: map[string]string{"tsconfig.json": `{"compilerOptions":{"verbatimModuleSyntax":"yes"}}`}, runtime: kept},
		{name: "unreadable tsconfig", extra: map[string]string{"tsconfig.json": `{"compilerOptions":`}, runtime: kept},
		{name: "jsonc tsconfig", extra: map[string]string{"tsconfig.json": "{\n  // comment\n  \"compilerOptions\": {\"strict\": true,},\n}"}, runtime: elided},
		{name: "sibling tsconfig.app.json", extra: map[string]string{
			"tsconfig.json":     `{"files":[],"references":[{"path":"./tsconfig.app.json"}]}`,
			"tsconfig.app.json": `{"compilerOptions":{"verbatimModuleSyntax":true}}`,
		}, runtime: kept},
		{name: "extends a repo config", extra: map[string]string{
			"tsconfig.base.json": `{"compilerOptions":{"verbatimModuleSyntax":true}}`,
			"src/tsconfig.json":  `{"extends":"../tsconfig.base.json"}`,
		}, runtime: kept},
		{name: "override of an extended config", extra: map[string]string{
			"config/base.json": `{"compilerOptions":{"verbatimModuleSyntax":true}}`,
			"tsconfig.json":    `{"extends":"./config/base.json","compilerOptions":{"verbatimModuleSyntax":false}}`,
		}, runtime: elided},
		{name: "a sibling tsconfig.*.json may govern the file itself", extra: map[string]string{
			"tsconfig.base.json": `{"compilerOptions":{"verbatimModuleSyntax":true}}`,
			"tsconfig.json":      `{"extends":"./tsconfig.base.json","compilerOptions":{"verbatimModuleSyntax":false}}`,
		}, runtime: kept},
		{name: "later extends entry wins", extra: map[string]string{
			"a.json":        `{"compilerOptions":{"verbatimModuleSyntax":true}}`,
			"b.json":        `{"compilerOptions":{"verbatimModuleSyntax":false}}`,
			"tsconfig.json": `{"extends":["./a.json","./b.json"]}`,
		}, runtime: elided},
		{name: "extends an installed config", extra: map[string]string{
			"node_modules/@acme/tsconfig/tsconfig.json": `{"compilerOptions":{"verbatimModuleSyntax":true}}`,
			"tsconfig.json": `{"extends":"@acme/tsconfig/tsconfig.json"}`,
		}, keepOut: []string{"node_modules/@acme/tsconfig/tsconfig.json"}, runtime: kept},
		{name: "extends an installed package's default config", extra: map[string]string{
			"node_modules/@acme/tsconfig/tsconfig.json": `{"compilerOptions":{"strict":true}}`,
			"tsconfig.json": `{"extends":"@acme/tsconfig"}`,
		}, keepOut: []string{"node_modules/@acme/tsconfig/tsconfig.json"}, runtime: elided},
		{name: "installed config extends a relative one", extra: map[string]string{
			"node_modules/@acme/tsconfig/strict.json": `{"extends":"./base.json"}`,
			"node_modules/@acme/tsconfig/base.json":   `{"compilerOptions":{"preserveValueImports":true}}`,
			"tsconfig.json":                           `{"extends":"@acme/tsconfig/strict.json"}`,
		}, keepOut: []string{"node_modules/@acme/tsconfig/strict.json", "node_modules/@acme/tsconfig/base.json"}, runtime: kept},
		{name: "extends a config that is not there", extra: map[string]string{
			"tsconfig.json": `{"extends":"@acme/missing"}`,
		}, runtime: kept},
		{name: "missing extended config overridden on every option", extra: map[string]string{
			"tsconfig.json": `{"extends":"@acme/missing","compilerOptions":{"verbatimModuleSyntax":false,"preserveValueImports":false,"importsNotUsedAsValues":"remove"}}`,
		}, runtime: elided},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			test := tt.test
			if test == "" {
				test = "src/a.test.ts"
			}
			h := newHost(t, typeOnlyFiles(test, tt.extra))
			for _, p := range tt.keepOut {
				delete(h.files, p)
			}
			a := New(h, Options{})
			gotAll, gotRuntime := a.SplitDeps(test, h.Read(test))
			assertDeps(t, gotAll, all)
			assertDeps(t, gotRuntime, tt.runtime)
			assertDeps(t, a.Deps(test, h.Read(test)), all)
		})
	}
}

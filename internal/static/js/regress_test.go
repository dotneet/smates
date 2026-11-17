package js

import "testing"

func TestGlobTypeArguments(t *testing.T) {
	h := newHost(t, map[string]string{
		"src/a.test.ts": "const pages = import.meta.glob<{ default: string }>('./pages/*.ts', { eager: true })\n" +
			"const fns = import.meta.glob<() => Promise<unknown>>('./lazy/*.ts')\n" +
			"const objs = import.meta.glob<{ default: string; name: string }>('./objs/*.ts')\n",
		"src/pages/home.ts": "",
		"src/lazy/x.ts":     "",
		"src/objs/o.ts":     "",
	})
	assertDeps(t, deps(t, h, "src/a.test.ts"), []string{"src/lazy/x.ts", "src/objs/o.ts", "src/pages/home.ts"})
}

func TestMockFnTypeArguments(t *testing.T) {
	h := newHost(t, map[string]string{
		"src/a.test.ts": "const u = jest.requireActual<Record<string, unknown>>('./util')\n" +
			"const b = await vi.importActual<{ b: () => void }>('./b')\n",
		"src/util.ts": "",
		"src/b.ts":    "",
	})
	assertDeps(t, deps(t, h, "src/a.test.ts"), []string{"src/b.ts", "src/util.ts"})
}

func TestRequirePathJoin(t *testing.T) {
	h := newHost(t, map[string]string{
		"test/a.test.js": "const path = require('path')\nconst { join } = require('node:path')\n" +
			"const a = require(path.join(__dirname, '../src/a'))\n" +
			"const b = require(path.resolve(__dirname, '..', 'src', 'b.js'))\n" +
			"const c = require(join(import.meta.dirname, 'fixtures', name))\n" +
			"const d = require(path.join(__dirname, 'gen/p-' + n))\n" +
			"const e = require(path.join('/abs', 'src/e'))\n" +
			"const f = require(path.resolve(__dirname, '/src/f'))\n" +
			"const g = require(other.join(__dirname, 'src/g'))\n" +
			"const h = require(path.join(__dirname, name))\n" +
			"const i = require(path.join(__dirname, ['plug', 'in'].join('') + '.js'))\n",
		"test/plugin.js":      "",
		"test/handler.js":     "",
		"src/a.js":            "",
		"src/b.js":            "",
		"test/fixtures/x.js":  "",
		"test/gen/p-1.js":     "",
		"test/gen/q.js":       "",
		"test/src/e.js":       "",
		"src/f.js":            "",
		"test/src/g.js":       "",
		"test/fixtures/y.cjs": "",
	})
	assertDeps(t, deps(t, h, "test/a.test.js"), []string{
		"src/a.js", "src/b.js", "test/fixtures/x.js", "test/fixtures/y.cjs", "test/gen/p-1.js",
	})
}

func TestJestManualMock(t *testing.T) {
	h := newHost(t, map[string]string{
		"package.json":           `{"name":"x"}`,
		"src/a.test.ts":          "import { f } from './util'\njest.mock('./util')\nvi.mock('../lib/x')\nimport axios from 'axios'\njest.mock('fs')\n",
		"src/util.ts":            "",
		"src/__mocks__/util.ts":  "",
		"lib/x.ts":               "",
		"lib/__mocks__/x.ts":     "",
		"__mocks__/axios.js":     "",
		"__mocks__/fs.ts":        "",
		"__mocks__/unrelated.js": "",
	})
	assertDeps(t, deps(t, h, "src/a.test.ts"), []string{
		"__mocks__/axios.js", "__mocks__/fs.ts", "lib/__mocks__/x.ts", "lib/x.ts", "src/__mocks__/util.ts", "src/util.ts",
	})
}

// TestManualMockSpecifierForms checks that every way of naming the mocked module
// (string, template, import(), require(), require.resolve(), import.meta.resolve())
// loads the adjacent manual mock, and that importActual of import() does not.
func TestManualMockSpecifierForms(t *testing.T) {
	for _, call := range []string{
		`vi.mock("./util")`,
		"vi.mock(`./util`)",
		`vi.mock(import("./util"))`,
		`vi.mock(import("./util"), () => ({}))`,
		`vi.doMock(import('./util'))`,
		`vi.mock<typeof import("./util")>(import("./util"))`,
		`jest.mock(require.resolve("./util"))`,
		`jest.mock(require("./util"))`,
		`vi.mock(import.meta.resolve("./util"))`,
		`vitest.mock(import("./util"))`,
	} {
		t.Run(call, func(t *testing.T) {
			h := newHost(t, map[string]string{
				"src/a.test.ts":         call + "\n",
				"src/util.ts":           "",
				"src/__mocks__/util.ts": "",
			})
			assertDeps(t, deps(t, h, "src/a.test.ts"), []string{"src/__mocks__/util.ts", "src/util.ts"})
		})
	}
	for _, call := range []string{`await vi.importActual(import("./util"))`, `jest.requireActual(require.resolve("./util"))`} {
		t.Run(call, func(t *testing.T) {
			h := newHost(t, map[string]string{
				"src/a.test.ts":         call + "\n",
				"src/util.ts":           "",
				"src/__mocks__/util.ts": "",
			})
			assertDeps(t, deps(t, h, "src/a.test.ts"), []string{"src/util.ts"})
		})
	}
}

// TestExtensionMapping checks TypeScript's mapping of the extension a specifier ends
// in to the files it may load (with allowJs and allowImportingTsExtensions): each
// candidate alone is found.
func TestExtensionMapping(t *testing.T) {
	table := map[string][]string{
		".js":   {".ts", ".tsx", ".d.ts", ".js", ".jsx"},
		".jsx":  {".tsx", ".ts", ".d.ts", ".jsx", ".js"},
		".ts":   {".ts", ".tsx", ".d.ts", ".js", ".jsx"},
		".tsx":  {".tsx", ".ts", ".d.ts", ".jsx", ".js"},
		".mjs":  {".mts", ".d.mts", ".mjs"},
		".mts":  {".mts", ".d.mts", ".mjs"},
		".cjs":  {".cts", ".d.cts", ".cjs"},
		".cts":  {".cts", ".d.cts", ".cjs"},
		".json": {".d.json.ts", ".json"},
	}
	for specExt, cands := range table {
		for _, fileExt := range cands {
			t.Run(specExt+"->"+fileExt, func(t *testing.T) {
				h := newHost(t, map[string]string{
					"src/a.test.tsx":       "import B from './Button" + specExt + "'\n",
					"src/Button" + fileExt: "",
				})
				assertDeps(t, deps(t, h, "src/a.test.tsx"), []string{"src/Button" + fileExt})
			})
		}
	}
	// Every candidate present: all are kept (over-approximation).
	h := newHost(t, map[string]string{
		"src/a.js":      "import B from './Button.js'\n",
		"src/Button.js": "", "src/Button.jsx": "", "src/Button.ts": "", "src/Button.tsx": "", "src/Button.d.ts": "",
	})
	assertDeps(t, deps(t, h, "src/a.js"), []string{"src/Button.d.ts", "src/Button.js", "src/Button.jsx", "src/Button.ts", "src/Button.tsx"})
}

func TestRequireActualSkipsManualMock(t *testing.T) {
	// requireActual loads the real module, never the manual mock.
	h := newHost(t, map[string]string{
		"src/a.test.ts":         "const u = jest.requireActual('./util')\n",
		"src/util.ts":           "",
		"src/__mocks__/util.ts": "",
	})
	assertDeps(t, deps(t, h, "src/a.test.ts"), []string{"src/util.ts"})
}

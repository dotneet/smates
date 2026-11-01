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
			"const g = require(other.join(__dirname, 'src/g'))\n",
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

func TestRequireActualSkipsManualMock(t *testing.T) {
	// requireActual loads the real module, never the manual mock.
	h := newHost(t, map[string]string{
		"src/a.test.ts":         "const u = jest.requireActual('./util')\n",
		"src/util.ts":           "",
		"src/__mocks__/util.ts": "",
	})
	assertDeps(t, deps(t, h, "src/a.test.ts"), []string{"src/util.ts"})
}

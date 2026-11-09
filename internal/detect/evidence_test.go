package detect

import (
	"reflect"
	"testing"
)

const nodeTestSrc = "import test from 'node:test'\ntest('x', () => {})\n"

// suiteSpec is what a test checks of a detected suite.
type suiteSpec struct {
	Root, Runner, Command string
	Tests                 []string
}

func detectSpecs(t *testing.T, files map[string]string) []suiteSpec {
	t.Helper()
	_, suites := detect(t, files)
	var out []suiteSpec
	for _, s := range suites {
		out = append(out, suiteSpec{s.Root, s.Runner, s.Command, s.Tests})
	}
	return out
}

// Test files of Node's own runner outside every JS package with a test runner get a node
// suite: at their package, else at their directory (above a test/ directory).
func TestDetectNodeTestFiles(t *testing.T) {
	for name, c := range map[string]struct {
		files map[string]string
		want  []suiteSpec
	}{
		"scripts directory": {map[string]string{
			"tools/check.test.mjs": nodeTestSrc, "tools/lint.test.mjs": nodeTestSrc, "tools/check.mjs": "",
		}, []suiteSpec{{"tools", "node", "", []string{"*.test.mjs"}}}},
		"other default names": {map[string]string{
			"tools/a-test.js": nodeTestSrc, "tools/b_test.cjs": "const test = require('node:test')\n", "tools/test-c.mjs": nodeTestSrc, "tools/test.js": nodeTestSrc,
		}, []suiteSpec{{"tools", "node", "", []string{"*-test.js", "*_test.cjs", "test-*.mjs", "test.js"}}}},
		"test directory": {map[string]string{
			"lib/test/a.js": nodeTestSrc, "lib/test/deep/b.js": nodeTestSrc, "lib/src/x.js": "",
		}, []suiteSpec{{"lib", "node", "", []string{"test/**/*.js"}}}},
		"package without a test script": {map[string]string{
			"pkg/package.json": `{"name":"p"}`, "pkg/src/a.test.mjs": nodeTestSrc,
		}, []suiteSpec{{"pkg", "node", "", []string{"src/*.test.mjs"}}}},
		"nested directories join the outer one": {map[string]string{
			"tools/a.test.mjs": nodeTestSrc, "tools/sub/b.test.mjs": nodeTestSrc,
		}, []suiteSpec{{"tools", "node", "", []string{"*.test.mjs", "sub/*.test.mjs"}}}},
		"not a node:test file": {map[string]string{
			"tools/a.test.mjs": "import { test } from 'other'\n", "tools/b.test.js": "// node:test is mentioned only\n",
		}, nil},
		"inside a vitest package": {map[string]string{
			"web/package.json": vitestPkg, "web/src/a.test.ts": "", "web/scripts/x.test.mjs": nodeTestSrc,
		}, []suiteSpec{{"web", "vitest", "", nil}}},
		"fixtures and vendor": {map[string]string{
			"x/fixtures/a.test.mjs": nodeTestSrc, "vendor/m/a.test.mjs": nodeTestSrc, "y/node_modules/m/a.test.mjs": nodeTestSrc,
		}, nil},
		"a node package with its own suite": {map[string]string{
			"pkg/package.json": `{"name":"p","scripts":{"test":"node --test"}}`, "pkg/a.test.mjs": nodeTestSrc,
		}, []suiteSpec{{"pkg", "node", "", nil}}},
	} {
		t.Run(name, func(t *testing.T) {
			if got := detectSpecs(t, c.files); !reflect.DeepEqual(got, c.want) {
				t.Errorf("got %+v, want %+v", got, c.want)
			}
		})
	}
}

const unittestSrc = "import unittest\n\nclass T(unittest.TestCase):\n    def test_a(self):\n        pass\n"

// A directory a CI or task file discovers with python -m unittest discover -s gets a
// unittest suite, unless a Python suite already covers it.
func TestDetectUnittestDiscover(t *testing.T) {
	wf := func(run string) string {
		return "on: push\njobs:\n  t:\n    runs-on: ubuntu-latest\n    steps:\n      - run: " + run + "\n"
	}
	for name, c := range map[string]struct {
		files map[string]string
		want  []suiteSpec
	}{
		"workflow": {map[string]string{
			".github/workflows/ci.yml": wf("python3 -m unittest discover -s tools -p 'test_*.py'"), "tools/test_a.py": unittestSrc, "tools/a.py": "",
		}, []suiteSpec{{"tools", "unittest", "python3 -m unittest discover -p 'test_*.py'", nil}}},
		"default pattern, long options": {map[string]string{
			"Makefile": "check:\n\tpython -m unittest discover --start-directory=tools\n", "tools/test_a.py": unittestSrc,
		}, []suiteSpec{{"tools", "unittest", "", nil}}},
		"top-level directory kept": {map[string]string{
			".gitlab-ci.yml": "t:\n  script: python3 -m unittest discover -s pkg/tests -t pkg && echo done\n", "pkg/tests/test_a.py": unittestSrc, "pkg/__init__.py": "", "pkg/tests/__init__.py": "",
		}, []suiteSpec{{".", "unittest", "python3 -m unittest discover -s pkg/tests -t pkg", nil}}},
		"start directory not in the repository": {map[string]string{
			".github/workflows/ci.yml": wf("python3 -m unittest discover -s ../other"), "x/test_a.py": unittestSrc,
		}, nil},
		"variable start directory": {map[string]string{
			".github/workflows/ci.yml": wf("python3 -m unittest discover -s $DIR"), "x/test_a.py": unittestSrc,
		}, nil},
		"no start directory": {map[string]string{
			".github/workflows/ci.yml": wf("python3 -m unittest discover"), "x/test_a.py": unittestSrc,
		}, nil},
		"already a python project": {map[string]string{
			".github/workflows/ci.yml": wf("python3 -m unittest discover -s app/tests"), "app/pyproject.toml": "[project]\nname='a'\n", "app/tests/test_a.py": unittestSrc,
		}, []suiteSpec{{"app", "unittest", "", nil}}},
		"task file in a subdirectory does not count": {map[string]string{
			"sub/Makefile": "t:\n\tpython -m unittest discover -s tools\n", "tools/test_a.py": unittestSrc,
		}, nil},
		"twice": {map[string]string{
			".github/workflows/a.yml": wf("python3 -m unittest discover -s tools"), ".github/workflows/b.yml": wf("python -m unittest discover -s tools -v"), "tools/test_a.py": unittestSrc,
		}, []suiteSpec{{"tools", "unittest", "", nil}}},
	} {
		t.Run(name, func(t *testing.T) {
			if got := detectSpecs(t, c.files); !reflect.DeepEqual(got, c.want) {
				t.Errorf("got %+v, want %+v", got, c.want)
			}
		})
	}
}

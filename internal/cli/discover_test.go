package cli

import (
	"bytes"
	"strings"
	"testing"
)

// TestListShowsRunnerDiscovery checks that test files come from the runners' own
// configuration when smates.yml has no tests:, and that list and doctor say so.
func TestListShowsRunnerDiscovery(t *testing.T) {
	g := newGitRepo(t)
	writeTree(t, g.dir, map[string]string{
		"web/package.json": `{"devDependencies":{"jest":"^30.0.0"}}`,
		"web/jest.config.js": `module.exports = {
  testMatch: ['<rootDir>/specs/**/*.spec.js'],
  testPathIgnorePatterns: ['/node_modules/', '<rootDir>/specs/slow/'],
};`,
		"web/specs/a.spec.js":      "",
		"web/specs/slow/b.spec.js": "",
		"web/src/c.test.js":        "", // a default pattern jest is not configured with
		"api/package.json":         `{"devDependencies":{"vitest":"^3.0.0"}}`,
		"api/vitest.config.ts":     "const dirs = ['src']\nexport default defineConfig({ test: { include: dirs.map((d) => `${d}/**/*.test.ts`) } })\n",
		"api/src/d.test.ts":        "",
		"tools/pyproject.toml":     "[tool.pytest.ini_options]\ntestpaths = [\"checks\"]\npython_files = [\"check_*.py\"]\n",
		"tools/checks/check_e.py":  "",
		"tools/tests/test_f.py":    "",
	})
	g.git("add", ".")
	g.commit("files")

	var out, errb bytes.Buffer
	if code := Main([]string{"list", "-v", "-C", g.dir}, &out, &errb); code != 0 {
		t.Fatalf("list: exit %d: %s", code, errb.String())
	}
	got := out.String()
	for _, s := range []string{
		"tests=web/jest.config.js testMatch, testPathIgnorePatterns",
		"  tests from web/jest.config.js testMatch, testPathIgnorePatterns: specs/**/*.spec.js !regex /node_modules/ !regex /specs/slow/\n",
		"  web/specs/a.spec.js\n",
		"tests=runner defaults",
		"  api/src/d.test.ts\n",
		"tests=tools/pyproject.toml [tool.pytest.ini_options] testpaths, python_files",
		"  tools/checks/check_e.py\n",
	} {
		if !strings.Contains(got, s) {
			t.Errorf("list output lacks %q:\n%s", s, got)
		}
	}
	for _, s := range []string{"web/specs/slow/b.spec.js", "web/src/c.test.js", "tools/tests/test_f.py"} {
		if strings.Contains(got, s) {
			t.Errorf("list counts %s as a test:\n%s", s, got)
		}
	}
	if !strings.Contains(errb.String(), "api/vitest.config.ts: test.include is not a literal") {
		t.Errorf("list does not warn about the unreadable vitest config:\n%s", errb.String())
	}

	out.Reset()
	errb.Reset()
	Main([]string{"doctor", "-C", g.dir}, &out, &errb)
	for _, s := range []string{
		"ok    suite jest: tests from web/jest.config.js testMatch, testPathIgnorePatterns: specs/**/*.spec.js",
		"warn  suite vitest: api/vitest.config.ts: test.include is not a literal",
		"set tests: in smates.yml",
	} {
		if !strings.Contains(out.String(), s) {
			t.Errorf("doctor output lacks %q:\n%s", s, out.String())
		}
	}
}

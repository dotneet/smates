package cli

import (
	"bytes"
	"reflect"
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

	// The JSON forms carry the same information.
	code, js, errs := smatesCmd(t, g.dir, "list", "--format", "json")
	if code != 0 {
		t.Fatalf("list --format json: exit %d: %s", code, errs)
	}
	rep := decodeJSON[listReport](t, js)
	bySuite := map[string]listSuite{}
	for _, s := range rep.Suites {
		bySuite[s.Name] = s
	}
	if s := bySuite["jest"]; s.TestsSource != "web/jest.config.js testMatch, testPathIgnorePatterns" ||
		!reflect.DeepEqual(s.TestPatterns, []string{"specs/**/*.spec.js", "!regex /node_modules/", "!regex /specs/slow/"}) ||
		s.DiscoveryWarning != "" || !reflect.DeepEqual(s.Units, []string{"web/specs/a.spec.js"}) {
		t.Errorf("jest suite: %+v", s)
	}
	if s := bySuite["vitest"]; s.TestsSource != "runner defaults" || !strings.Contains(s.DiscoveryWarning, "api/vitest.config.ts: test.include is not a literal") {
		t.Errorf("vitest suite: %+v", s)
	}
	if s := bySuite["pytest"]; s.TestsSource != "tools/pyproject.toml [tool.pytest.ini_options] testpaths, python_files" ||
		!reflect.DeepEqual(s.TestPatterns, []string{"checks/**/check_*.py"}) {
		t.Errorf("pytest suite: %+v", s)
	}

	_, js, _ = smatesCmd(t, g.dir, "doctor", "--format", "json")
	doc := decodeJSON[doctorReport](t, js)
	var source, warning *doctorCheck
	for _, c := range doc.Checks {
		switch {
		case c.ID == "suite-tests-source" && c.Subject == "jest":
			source = c
		case c.ID == "suite-test-discovery" && c.Subject == "vitest":
			warning = c
		}
	}
	if source == nil || source.Status != "ok" || !strings.Contains(source.Message, "tests from web/jest.config.js testMatch") {
		t.Errorf("tests source check: %+v", source)
	}
	if warning == nil || warning.Status != "warn" || warning.Fix == nil || warning.Fix.Config == nil || warning.Fix.Config.Key != "suites.vitest.tests" ||
		!strings.Contains(warning.Message, "test.include is not a literal") {
		t.Errorf("discovery warning check: %+v", warning)
	}
}

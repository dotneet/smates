//go:build e2e

package e2e

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestCoverageTools records fixtures/coverage-tools, whose suites run with coverage tools
// (vitest --coverage with the v8 provider, c8, node --test --experimental-test-coverage
// with a user-set NODE_V8_COVERAGE, pytest-cov with pytest-xdist and with the sysmon
// core, go test -cover -coverpkg -coverprofile), and checks both sides: smates traced
// every test (the plugin tests depend on a module loaded by a computed path, which only
// tracing sees), and every tool still wrote its report, with data and without smates'
// own shim in it.
func TestCoverageTools(t *testing.T) {
	if !fixtureSelected("coverage-tools") {
		t.Skip("SMATES_E2E_FIXTURE excludes coverage-tools")
	}
	t.Parallel()
	r := prepare(t, "coverage-tools", nil)

	// A file of the user's own in the NODE_V8_COVERAGE directory survives the run.
	raw := filepath.Join(r.dir, "coverage", "raw")
	if err := os.MkdirAll(raw, 0o755); err != nil {
		t.Fatal(err)
	}
	keep := filepath.Join(raw, "coverage-1-1-0.json")
	if err := os.WriteFile(keep, []byte("mine"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, code := r.smates("run", "--all", "--no-agent", "--report", r.reportPath())
	rep := r.readReport()
	if code != 0 || rep.Exit != 0 || rep.Total != 16 {
		t.Fatalf("run failed (exit %d, %d tests):\n%s", code, rep.Total, tail(out, 60))
	}
	for _, bad := range []string{"left no trace", "could not copy V8 coverage", "tracing disabled"} {
		if strings.Contains(out, bad) {
			t.Errorf("run output reports %q:\n%s", bad, tail(out, 60))
		}
	}

	// smates' side: what only tracing can see.
	deps := func(test string) string {
		t.Helper()
		out, code := r.smates("deps", test)
		if code != 0 {
			t.Fatalf("smates deps %s: %s", test, out)
		}
		return out
	}
	for _, test := range []string{"vitest/plugin.test.js", "c8/plugin.test.js", "native/plugin.test.js"} {
		if d := deps(test); !strings.Contains(d, "src/plugin.js\trecorded\n") {
			t.Errorf("%s: src/plugin.js not recorded:\n%s", test, d)
		}
	}
	for _, test := range []string{"py/tests/test_plugin.py", "py/tests_sysmon/test_sysmon_plugin.py"} {
		if d := deps(test); !strings.Contains(d, "py/src/calc/answer.py\tstatic,recorded\n") {
			t.Errorf("%s: py/src/calc/answer.py not recorded:\n%s", test, d)
		}
	}
	// pytest-xdist workers run uname -p (platform.processor()): no dependency on the
	// whole tree. pytest-cov's report reads modules no test ran: no dependency either.
	if d := deps("py/tests/test_arith.py"); strings.Contains(d, "**") || strings.Contains(d, "calc/text.py") {
		t.Errorf("py/tests/test_arith.py depends on more than it used:\n%s", d)
	}
	if d := deps("py/tests_sysmon/test_sysmon_arith.py"); strings.Contains(d, "calc/text.py") {
		t.Errorf("py/tests_sysmon/test_sysmon_arith.py depends on a module only the coverage report read:\n%s", d)
	}
	if d := deps("go/text"); !strings.Contains(d, "go/text/greeting.txt\trecorded\n") {
		t.Errorf("go/text: greeting.txt not recorded:\n%s", d)
	}

	// The tools' side: every report exists and has data.
	read := func(rel string) string {
		t.Helper()
		data, err := os.ReadFile(filepath.Join(r.dir, filepath.FromSlash(rel)))
		if err != nil {
			t.Errorf("coverage output missing: %v", err)
		}
		return string(data)
	}
	for _, rel := range []string{"coverage/vitest/coverage-summary.json", "coverage/c8/coverage-summary.json"} {
		var sum struct {
			Total struct {
				Lines struct{ Covered int } `json:"lines"`
			} `json:"total"`
		}
		if data := read(rel); data != "" {
			if err := json.Unmarshal([]byte(data), &sum); err != nil || sum.Total.Lines.Covered == 0 {
				t.Errorf("%s: no covered lines (%v): %.300s", rel, err, data)
			}
		}
	}
	lcov := read("coverage/native.lcov")
	if !strings.Contains(lcov, "SF:src/math.js\n") || !strings.Contains(lcov, "SF:src/plugin.js\n") {
		t.Errorf("node --test coverage lacks the sources:\n%.2000s", lcov)
	}
	if strings.Contains(lcov, "node.cjs") || strings.Contains(lcov, "smates") {
		t.Errorf("node --test coverage reports smates' shim:\n%.2000s", lcov)
	}
	if b, _ := os.ReadFile(keep); string(b) != "mine" {
		t.Errorf("the user's file in NODE_V8_COVERAGE was changed or removed: %q", b)
	}
	files, _ := filepath.Glob(filepath.Join(raw, "coverage-*.json"))
	withSources := 0
	for _, f := range files {
		if b, _ := os.ReadFile(f); strings.Contains(string(b), "/src/math.js") {
			withSources++
		}
	}
	if withSources == 0 {
		t.Errorf("no V8 coverage of the sources in NODE_V8_COVERAGE (%d files)", len(files))
	}
	for _, rel := range []string{"py/cov-xdist.json", "py/cov-sysmon.json"} {
		var cov struct {
			Totals struct {
				CoveredLines int `json:"covered_lines"`
			} `json:"totals"`
		}
		if data := read(rel); data != "" {
			if err := json.Unmarshal([]byte(data), &cov); err != nil || cov.Totals.CoveredLines == 0 {
				t.Errorf("%s: no covered lines (%v): %.300s", rel, err, data)
			}
		}
	}
	profile := read("go/cover.out")
	if !strings.HasPrefix(profile, "mode: ") || !strings.Contains(profile, "calc/calc.go:") || !strings.Contains(profile, " 1\n") {
		t.Errorf("go/cover.out has no covered blocks:\n%.1000s", profile)
	}
}

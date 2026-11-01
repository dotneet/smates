package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/dotneet/smates/internal/config"
	"github.com/dotneet/smates/internal/runner"
	"github.com/dotneet/smates/internal/selector"
	"github.com/dotneet/smates/internal/state"
)

func TestPrioritizeAndPlan(t *testing.T) {
	web := &selector.Suite{Name: "web", Suite: &runner.Suite{Suite: config.Suite{Name: "web", Tests: []string{"**/*.test.ts"}}}, Tests: []*selector.Test{
		{ID: "new.test.ts", Selected: true, Reason: selector.ReasonNew},
		{ID: "deep.test.ts", Selected: true, Reason: selector.ReasonChanged, Changed: []string{"util.ts"}, Via: map[string]string{"util.ts": selector.ViaImport}, Duration: 10},
		{ID: "direct.test.ts", Selected: true, Reason: selector.ReasonChanged, Changed: []string{"lib.ts"}, Via: map[string]string{"lib.ts": selector.ViaImport}, Duration: 900},
		{ID: "own.test.ts", Selected: true, Reason: selector.ReasonChanged, Changed: []string{"own.test.ts"}, Via: map[string]string{"own.test.ts": selector.ViaTest}, Duration: 50},
		{ID: "data.test.ts", Selected: true, Reason: selector.ReasonChanged, Changed: []string{"fixtures/x.json"}, Via: map[string]string{"fixtures/x.json": selector.ViaTraced}, Duration: 5},
		{ID: "skip.test.ts", Reason: selector.ReasonUnaffected},
	}}
	py := &selector.Suite{Name: "py", Suite: &runner.Suite{Suite: config.Suite{Name: "py", Tests: []string{"test_*.py"}}}, Tests: []*selector.Test{
		{ID: "test_ref.py", Selected: true, Reason: selector.ReasonReference},
		// Failed last time and changed since: failures come first.
		{ID: "test_broken.py", Selected: true, Reason: selector.ReasonChanged, Changed: []string{"conftest.py"}, Via: map[string]string{"conftest.py": selector.ViaInput}, Duration: 2000},
	}}
	res := &selector.Result{Mode: selector.ModeState, Suites: []*selector.Suite{web, py}}
	st := &state.State{Suites: map[string]*state.Suite{
		"web": {Tests: map[string]*state.Entry{
			"deep.test.ts": {Status: state.Pass}, "direct.test.ts": {Status: state.Pass}, "own.test.ts": {Status: state.Pass},
			"data.test.ts": {Status: state.Pass}, "skip.test.ts": {Status: state.Pass},
		}},
		"py": {Tests: map[string]*state.Entry{"test_ref.py": {Status: state.Pass}, "test_broken.py": {Status: state.Fail}}},
	}}
	direct := func(f string) []string {
		return map[string][]string{"direct.test.ts": {"lib.ts"}, "deep.test.ts": {"mid.ts"}}[f]
	}
	prioritize(res, st, direct)
	want := map[string]int{
		"new.test.ts": prioOther, "deep.test.ts": prioIndirect, "direct.test.ts": prioDirect, "own.test.ts": prioDirect,
		"data.test.ts": prioIndirect, "skip.test.ts": 0, "test_ref.py": prioIndirect, "test_broken.py": prioFailed,
	}
	for _, sr := range res.Suites {
		for _, tt := range sr.Tests {
			if tt.Priority != want[tt.ID] {
				t.Errorf("%s: priority %d, want %d", tt.ID, tt.Priority, want[tt.ID])
			}
		}
	}

	type step struct {
		suite string
		units []string
	}
	planOf := func(failFast bool) []step {
		c := &cycle{p: &project{state: st}, f: &runFlags{failFast: failFast}, sel: &selection{res: res}}
		var out []step
		for _, b := range c.plan() {
			if b.full {
				t.Errorf("%s: a partial selection is not a full run", b.sr.Name)
			}
			out = append(out, step{b.sr.Name, b.units})
		}
		return out
	}
	// By default each suite runs once: suites by their best priority, units by priority,
	// then shortest recorded duration first, unrecorded last.
	got := planOf(false)
	wantDefault := []step{
		{"py", []string{"test_broken.py", "test_ref.py"}},
		{"web", []string{"own.test.ts", "direct.test.ts", "data.test.ts", "deep.test.ts", "new.test.ts"}},
	}
	if !slices.EqualFunc(got, wantDefault, func(a, b step) bool { return a.suite == b.suite && slices.Equal(a.units, b.units) }) {
		t.Errorf("default plan %v, want %v", got, wantDefault)
	}
	// --fail-fast: one batch per priority, across suites.
	got = planOf(true)
	wantFF := []step{
		{"py", []string{"test_broken.py"}},
		{"web", []string{"own.test.ts", "direct.test.ts"}},
		{"web", []string{"data.test.ts", "deep.test.ts"}},
		{"py", []string{"test_ref.py"}},
		{"web", []string{"new.test.ts"}},
	}
	if !slices.EqualFunc(got, wantFF, func(a, b step) bool { return a.suite == b.suite && slices.Equal(a.units, b.units) }) {
		t.Errorf("fail-fast plan %v, want %v", got, wantFF)
	}
}

func TestPlanFullRun(t *testing.T) {
	sr := &selector.Suite{Name: "go", All: true, Suite: &runner.Suite{Suite: config.Suite{Name: "go"}}, Tests: []*selector.Test{
		{ID: "pkg/a", Selected: true, Reason: selector.ReasonAll},
		{ID: "pkg/b", Selected: true, Reason: selector.ReasonAll},
	}}
	res := &selector.Result{Mode: selector.ModeAll, Suites: []*selector.Suite{sr}}
	st := &state.State{Suites: map[string]*state.Suite{"go": {Tests: map[string]*state.Entry{"pkg/b": {Status: state.Fail}}}}}
	prioritize(res, st, nil)
	for _, failFast := range []bool{false, true} {
		c := &cycle{p: &project{state: st}, f: &runFlags{failFast: failFast}, sel: &selection{res: res}, runAll: true}
		plan := c.plan()
		switch {
		case !failFast && (len(plan) != 1 || !plan[0].full || !slices.Equal(plan[0].units, []string{"pkg/b", "pkg/a"})):
			t.Errorf("full run: %+v", plan[0])
		case failFast && (len(plan) != 2 || plan[0].full || plan[1].full || !slices.Equal(plan[0].units, []string{"pkg/b"})):
			// Split in two, a full run passes explicit units (the runner's discovery would
			// run the failed package twice).
			t.Errorf("full run with --fail-fast: %d batches", len(plan))
		}
	}
}

func TestParseRunFlagsFailFast(t *testing.T) {
	if _, err := parseRunFlags([]string{"--fail-fast", "--shadow"}, &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "--fail-fast") {
		t.Errorf("--shadow --fail-fast: %v", err)
	}
	f, err := parseRunFlags([]string{"--fail-fast", "--shard", "1/2"}, &bytes.Buffer{})
	if err != nil || !f.failFast {
		t.Errorf("--fail-fast --shard: %v", err)
	}
}

// TestRunFailFast drives `smates run --fail-fast` through Main: the likeliest failure
// runs first, the rest is not started, and only what ran is recorded.
func TestRunFailFast(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses sh")
	}
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not on PATH")
	}
	clearCI(t)
	noAgentCredentials(t)
	t.Setenv("PYTHONDONTWRITEBYTECODE", "1")
	t.Setenv("GITHUB_OUTPUT", "")
	tmp := t.TempDir()
	summary := filepath.Join(tmp, "summary.md")
	t.Setenv("GITHUB_STEP_SUMMARY", summary)
	g := newGitRepo(t)
	write := func(rel, content string) {
		p := filepath.Join(g.dir, filepath.FromSlash(rel))
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// Two suites; the runner writes a JUnit report so outcomes are per file, and logs
	// the files it was given.
	write("smates.yml", `version: 1
orphans: ignore
suites:
  - name: one
    runner: custom
    root: one
    tests: ["*_check.py"]
    command: python3 ../run.py
    junit: .smates/junit.xml
  - name: two
    runner: custom
    root: two
    tests: ["*_check.py"]
    command: python3 ../run.py
    junit: .smates/junit.xml
`)
	write(".gitignore", ".smates/\n")
	runsLog := filepath.Join(tmp, "runs.log")
	t.Setenv("SMATES_RUNS_LOG", runsLog)
	write("run.py", `import os, subprocess, sys
files = sys.argv[1:] or sorted(f for f in os.listdir(".") if f.endswith("_check.py"))
with open(os.environ["SMATES_RUNS_LOG"], "a") as log:
    log.write(os.path.basename(os.getcwd()) + ": " + " ".join(files) + "\n")
cases, failed = [], False
for f in files:
    ok = subprocess.call([sys.executable, f]) == 0
    failed = failed or not ok
    cases.append('<testcase file="%s" name="%s">%s</testcase>' % (f, f, "" if ok else "<failure/>"))
os.makedirs(".smates", exist_ok=True)
open(".smates/junit.xml", "w").write("<testsuite>" + "".join(cases) + "</testsuite>")
sys.exit(1 if failed else 0)
`)
	write("data/a.txt", "a\n")
	write("data/b.txt", "b\n")
	write("one/a_check.py", "assert open('../data/a.txt').read().strip().lower() == 'a'\n")
	write("one/c_check.py", "assert True\n")
	write("two/b_check.py", "assert open('../data/b.txt').read() == 'b\\n'\n")
	g.git("add", "-A")
	g.commit("fixture")

	run := func(args ...string) (int, string, string) {
		var out, errb bytes.Buffer
		code := Main(append(args[:1:1], append([]string{"-C", g.dir, "--no-agent"}, args[1:]...)...), &out, &errb)
		return code, out.String(), errb.String()
	}
	runs := func() string {
		data, _ := os.ReadFile(runsLog)
		os.Remove(runsLog)
		return string(data)
	}
	selected := func() map[string]string {
		t.Helper()
		code, out, errs := run("select", "--format", "json")
		if code != 0 {
			t.Fatalf("select: %d %s", code, errs)
		}
		var js struct {
			Suites []struct {
				Tests []struct {
					ID       string `json:"id"`
					Selected bool   `json:"selected"`
					Reason   string `json:"reason"`
				} `json:"tests"`
			} `json:"suites"`
		}
		if err := json.Unmarshal([]byte(out), &js); err != nil {
			t.Fatalf("%v\n%s", err, out)
		}
		m := map[string]string{}
		for _, s := range js.Suites {
			for _, tt := range s.Tests {
				if tt.Selected {
					m[tt.ID] = tt.Reason
				}
			}
		}
		return m
	}

	if code, _, errs := run("run", "--all"); code != 0 {
		t.Fatalf("baseline: %d\n%s", code, errs)
	}
	runs()

	// The test file of two/b changed (priority 2) and breaks it; a's traced data
	// changed (priority 3). Suite two runs first, suite one is never started.
	write("two/b_check.py", "assert open('../data/b.txt').read() == 'B\\n'\n")
	write("data/a.txt", "A\n")
	if code, out, _ := run("run", "--fail-fast", "--dry-run"); !strings.Contains(out, "two") || strings.Index(out, "(cd two") > strings.Index(out, "(cd one") {
		t.Errorf("dry run: %d\n%s", code, out)
	}
	report := filepath.Join(tmp, "report.json")
	code, _, errs := run("run", "--fail-fast", "--report", report)
	if code == 0 {
		t.Fatalf("fail-fast run must fail\n%s", errs)
	}
	if got := runs(); got != "two: b_check.py\n" {
		t.Errorf("runner calls %q\n%s", got, errs)
	}
	var rep struct {
		Results  map[string]map[string]string `json:"results"`
		NotRun   map[string][]string          `json:"not_run"`
		FailFast bool                         `json:"fail_fast"`
		Suites   []struct {
			Tests []struct {
				ID       string `json:"id"`
				Priority int    `json:"priority"`
			} `json:"tests"`
		} `json:"suites"`
	}
	data, err := os.ReadFile(report)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &rep); err != nil {
		t.Fatal(err)
	}
	if !rep.FailFast || len(rep.Results) != 1 || rep.Results["two"]["two/b_check.py"] != state.Fail || !slices.Equal(rep.NotRun["one"], []string{"one/a_check.py"}) {
		t.Errorf("report: %+v", rep)
	}
	if !strings.Contains(errs, "--fail-fast left 1 selected unit(s) not started") {
		t.Errorf("run log:\n%s", errs)
	}
	if s, _ := os.ReadFile(summary); !strings.Contains(string(s), "**Fail fast:** 1 selected test unit not started") || !strings.Contains(string(s), "one: one/a_check.py") {
		t.Errorf("summary:\n%s", s)
	}
	// What did not run keeps its entry: it is selected again, for the same reason.
	if got := selected(); len(got) != 2 || got["one/a_check.py"] != selector.ReasonChanged || got["two/b_check.py"] != selector.ReasonFailed {
		t.Errorf("after fail-fast: %v", got)
	}

	// Fixed: the failure runs first, then the rest; everything passes and is recorded.
	write("data/b.txt", "B\n")
	if code, _, errs := run("run", "--fail-fast"); code != 0 {
		t.Fatalf("fixed run: %d\n%s", code, errs)
	}
	if got := runs(); got != "two: b_check.py\none: a_check.py\n" {
		t.Errorf("runner calls %q", got)
	}
	if got := selected(); len(got) != 0 {
		t.Errorf("after fix: %v", got)
	}
}

package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"testing"

	"github.com/dotneet/smates/internal/runner"
	"github.com/dotneet/smates/internal/state"
)

// TestRunSelectEndToEnd drives `smates run` / `smates select` through Main on a small
// repository whose "tests" are Python scripts run by a custom runner, traced by the
// embedded Python shim. It checks recording, selection by changed inputs, failure
// retries, and that shadow mode reports a failing test the selection missed.
func TestRunSelectEndToEnd(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses sh")
	}
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not on PATH")
	}
	clearCI(t)
	t.Setenv("GITHUB_OUTPUT", "")
	t.Setenv("GITHUB_STEP_SUMMARY", "")
	noAgentCredentials(t)
	// Edits below swap same-size contents within one second, which a cached .pyc
	// (validated by mtime and size) would not notice.
	t.Setenv("PYTHONDONTWRITEBYTECODE", "1")
	g := newGitRepo(t)
	write := func(rel, content string) {
		p := filepath.Join(g.dir, filepath.FromSlash(rel))
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("smates.yml", `version: 1
orphans: ignore
suites:
  - name: py
    runner: custom
    tests: ["tests/*_check.py"]
    command: sh -c '[ $# -gt 0 ] || set -- tests/*_check.py; for f; do python3 "$f" || exit 1; done' sh
`)
	write(".gitignore", ".smates/\n")
	write("src/mod_a.py", "X = 1\n")
	write("src/mod_b.py", "Y = 2\n")
	write("data/a.txt", "hello\n")
	write("tests/a_check.py", `import sys
sys.path.insert(0, "src")
import mod_a
assert open("data/a.txt").read().startswith("hello")
assert mod_a.X == 1
`)
	// b depends on an environment variable, which tracing cannot see.
	write("tests/b_check.py", `import os, sys
sys.path.insert(0, "src")
import mod_b
assert os.environ.get("SMATES_TEST_MODE") != "broken"
`)
	g.git("add", "-A")
	g.commit("fixture")

	run := func(args ...string) (int, string, string) {
		var out, errb bytes.Buffer
		code := Main(append(args[:1:1], append([]string{"-C", g.dir, "--no-agent"}, args[1:]...)...), &out, &errb)
		return code, out.String(), errb.String()
	}
	selected := func() map[string]string {
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
	if got := selected(); len(got) != 0 {
		t.Fatalf("nothing changed, selected %v", got)
	}

	write("data/a.txt", "hello again\n")
	if got := selected(); len(got) != 1 || got["tests/a_check.py"] != "changed" {
		t.Errorf("traced data file: %v", got)
	}
	write("data/a.txt", "hello\n")
	write("src/mod_b.py", "Y = 3\n")
	if got := selected(); len(got) != 1 || got["tests/b_check.py"] != "changed" {
		t.Errorf("traced module: %v", got)
	}
	write("src/mod_b.py", "Y = 2\n")

	// A failing test stays selected until it passes again.
	write("src/mod_a.py", "X = 2\n")
	if code, _, errs := run("run"); code == 0 {
		t.Fatalf("run with a broken module must fail\n%s", errs)
	}
	// (The custom runner has no per-file status, so b is marked failed too.)
	if got := selected(); got["tests/a_check.py"] != "failed" {
		t.Errorf("after failure: %v", got)
	}
	write("src/mod_a.py", "X = 1\n")
	if code, _, errs := run("run"); code != 0 {
		t.Fatalf("fixed run: %d\n%s", code, errs)
	}
	if got := selected(); len(got) != 0 {
		t.Errorf("after fix: %v", got)
	}

	// Shadow mode: the environment is invisible to tracing, so the selection misses b;
	// shadow mode runs everything and reports the escape.
	t.Setenv("SMATES_TEST_MODE", "broken")
	report := filepath.Join(t.TempDir(), "report.json")
	code, _, errs := run("run", "--shadow", "--report", report)
	if code == 0 {
		t.Fatalf("shadow run must fail\n%s", errs)
	}
	data, err := os.ReadFile(report)
	if err != nil {
		t.Fatal(err)
	}
	var rep struct {
		Escapes  []string                     `json:"escapes"`
		Possible []string                     `json:"possible_escapes"`
		Orphans  []string                     `json:"orphans"`
		Results  map[string]map[string]string `json:"results"`
		Shadow   bool                         `json:"shadow"`
	}
	if err := json.Unmarshal(data, &rep); err != nil {
		t.Fatal(err)
	}
	sort.Strings(rep.Escapes)
	// The custom runner has no per-file status: a failing run marks every unit failed by
	// inference, so b is reported as a possible escape rather than a certain one.
	if !rep.Shadow || !contains(rep.Possible, "py: tests/b_check.py") || len(rep.Escapes) != 0 {
		t.Errorf("escapes %v (results %v, orphans %v)\n%s", rep.Escapes, rep.Results, rep.Orphans, errs)
	}
	if rep.Results["py"]["tests/b_check.py"] != "fail" {
		t.Errorf("results %v", rep.Results)
	}

	// Every recorded run is in the stats; --dry-run and --no-record add none.
	run("run", "--dry-run")
	run("run", "--no-record")
	var out, errb bytes.Buffer
	if code := Main([]string{"stats", "-C", g.dir, "--format", "json"}, &out, &errb); code != 0 {
		t.Fatalf("stats: %d %s", code, errb.String())
	}
	var st statsReport
	if err := json.Unmarshal(out.Bytes(), &st); err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	if st.Runs != 4 || st.ByMode["all"] != 1 || st.ByMode["state"] != 2 || st.ByMode["shadow"] != 1 || st.FailedRuns != 2 || st.BySource["local"] != 4 {
		t.Errorf("stats runs: %+v", st)
	}
	if st.Shadow.Runs != 1 || st.Shadow.Possible != 2 || st.Shadow.Escapes != 0 || st.Shadow.Failed != 2 {
		t.Errorf("stats shadow: %+v", st.Shadow)
	}
	if st.Total != 6 || st.Selected != 4 {
		t.Errorf("stats selection: %d of %d", st.Selected, st.Total)
	}
}

func contains(s []string, x string) bool {
	for _, v := range s {
		if v == x {
			return true
		}
	}
	return false
}

func TestSettleOutcomes(t *testing.T) {
	obs := func(st ...string) map[string]*runner.Observation {
		m := map[string]*runner.Observation{}
		for i, s := range st {
			m[string(rune('a'+i))] = &runner.Observation{Status: s}
		}
		return m
	}
	statuses := func(r *runner.RunResult) map[string]string {
		m := map[string]string{}
		for u, o := range r.Observations {
			m[u] = o.Status
		}
		return m
	}
	units := []string{"a", "b", "c"}

	// A passing run is recorded as reported.
	r := &runner.RunResult{Observations: obs(state.Pass, state.Fail)}
	settleOutcomes(r, units, true, false)
	if got := statuses(r); len(got) != 2 || got["a"] != state.Pass || got["b"] != state.Fail {
		t.Errorf("passing run: %v", got)
	}

	// A failed run: untraced units are unknown; reported outcomes stay.
	r = &runner.RunResult{ExitCode: 1, Observations: obs(state.Pass, state.Fail)}
	settleOutcomes(r, units, true, false)
	if got := statuses(r); got["a"] != state.Pass || got["b"] != state.Fail || got["c"] != state.Unknown || !r.Observations["c"].Inferred {
		t.Errorf("failed run: %v", got)
	}

	// A failed run in which no unit failed records no pass.
	r = &runner.RunResult{ExitCode: 1, Observations: obs(state.Pass)}
	settleOutcomes(r, units[:1], false, false)
	if o := r.Observations["a"]; o.Status != state.Unknown || !o.Inferred {
		t.Errorf("crash after the tests: %+v", o)
	}

	// Arguments passed to the runner may filter tests: no pass is recorded.
	r = &runner.RunResult{Observations: obs(state.Pass, state.Fail)}
	settleOutcomes(r, units, true, true)
	if got := statuses(r); got["a"] != state.Unknown || got["b"] != state.Fail {
		t.Errorf("runner arguments: %v", got)
	}

	// No observations at all.
	r = &runner.RunResult{}
	settleOutcomes(r, nil, true, false)
	if r.Observations == nil {
		t.Error("observations map not created")
	}
}

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
	t.Setenv("ANTHROPIC_API_KEY", "")
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
}

func contains(s []string, x string) bool {
	for _, v := range s {
		if v == x {
			return true
		}
	}
	return false
}

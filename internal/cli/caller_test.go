package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/dotneet/smates/internal/config"
	"github.com/dotneet/smates/internal/runner"
	"github.com/dotneet/smates/internal/selector"
	"github.com/dotneet/smates/internal/state"
)

type callerSelection struct {
	Orphans    []string            `json:"orphans"`
	Handling   map[string]string   `json:"orphan_handling"`
	Tests      map[string][]string `json:"selected_tests"`
	AgentNote  string              `json:"agent_note"`
	Candidates []orphanCandidate   `json:"orphan_candidates"`
	Suites     []struct {
		Name  string `json:"name"`
		Tests []struct {
			ID     string `json:"id"`
			Reason string `json:"reason"`
			Detail string `json:"detail"`
		} `json:"tests"`
	} `json:"suites"`
}

func (s *callerSelection) reason(suite, id string) string {
	for _, su := range s.Suites {
		for _, t := range su.Tests {
			if su.Name == suite && t.ID == id {
				return t.Reason
			}
		}
	}
	return ""
}

// TestCallerDecidesOrphans drives the flags a calling agent uses to decide about the
// changes no test depends on: the candidates `smates select --format json` reports,
// --add and --orphans.
func TestCallerDecidesOrphans(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses sh")
	}
	clearCI(t)
	noAgentCredentials(t)
	t.Setenv("GITHUB_OUTPUT", "")
	t.Setenv("GITHUB_STEP_SUMMARY", "")
	g := newGitRepo(t)
	write := func(rel, content string) {
		p := filepath.Join(g.dir, filepath.FromSlash(rel))
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// orphans: agent (the default) without an API key falls back to running the
	// affected suites in full.
	write("smates.yml", `version: 1
suites:
  - name: a
    runner: custom
    root: a
    tests: ["*_check.sh"]
    command: sh -c 'for f; do sh "$f" || exit 1; done' sh
    rules: [{tests: ["*"], deps: ["src/a/**"]}]
  - name: b
    runner: custom
    root: b
    tests: ["*_check.sh"]
    command: sh -c 'for f; do sh "$f" || exit 1; done' sh
    rules: [{tests: ["*"], deps: ["src/b/**"]}]
`)
	write(".gitignore", ".smates/\n")
	write("a/x_check.sh", "# loads fixtures/users.json\ntrue\n")
	write("a/y_check.sh", "true\n")
	write("b/z_check.sh", "true\n")
	write("src/a/lib", "1")
	write("src/b/lib", "1")
	g.git("add", "-A")
	g.commit("fixture")

	main := func(args ...string) (int, string, string) {
		var out, errb bytes.Buffer
		code := Main(append(args[:1:1], append([]string{"-C", g.dir}, args[1:]...)...), &out, &errb)
		return code, out.String(), errb.String()
	}
	if code, _, errs := main("run", "--all"); code != 0 {
		t.Fatalf("run --all: %d %s", code, errs)
	}
	// Two orphans: a fixture a test of a names, and a file under a no test names.
	write("b/fixtures/users.json", "[]")
	write("a/data.cfg", "x")

	sel := func(args ...string) (*callerSelection, string) {
		t.Helper()
		code, out, errs := main(append([]string{"select", "--format", "json"}, args...)...)
		if code != 0 {
			t.Fatalf("select %v: %d %s", args, code, errs)
		}
		var js callerSelection
		if err := json.Unmarshal([]byte(out), &js); err != nil {
			t.Fatalf("%v\n%s", err, out)
		}
		return &js, errs
	}

	// The candidates: status, the tests naming the file, the suites the policy runs,
	// and how this run handled it. Existing fields stay.
	js, _ := sel()
	if !reflect.DeepEqual(js.Orphans, []string{"a/data.cfg", "b/fixtures/users.json"}) || js.Tests == nil || js.AgentNote == "" {
		t.Fatalf("selection: %+v", js)
	}
	want := []orphanCandidate{
		{Path: "a/data.cfg", Status: "added", ReferencedBy: []candidateRef{}, Suites: []string{"a"}, Handling: selector.OrphanPolicy},
		{Path: "b/fixtures/users.json", Status: "added", ReferencedBy: []candidateRef{{Suite: "a", Test: "a/x_check.sh", File: "a/x_check.sh", Selected: true}},
			Suites: []string{"b"}, Handling: selector.OrphanReference},
	}
	if !reflect.DeepEqual(js.Candidates, want) {
		t.Errorf("candidates:\n%+v\nwant\n%+v", js.Candidates, want)
	}
	if got := js.Tests["a"]; !reflect.DeepEqual(got, []string{"a/x_check.sh", "a/y_check.sh"}) {
		t.Errorf("default policy selects all of a: %v", got)
	}
	// The text output tells a calling agent how to decide.
	if code, _, errs := main("select"); code != 0 || !strings.Contains(errs, "orphan_candidates") || !strings.Contains(errs, "--orphans=skip") {
		t.Errorf("text hint: %d %s", code, errs)
	}

	// --orphans=skip with --add: only the named tests are added; the skip is recorded.
	js, errs := sel("--orphans=skip", "--add", "a:a/y_check.sh")
	if got := js.Tests["a"]; !reflect.DeepEqual(got, []string{"a/x_check.sh", "a/y_check.sh"}) {
		t.Errorf("skip + add: a %v", got)
	}
	if js.Handling["a/data.cfg"] != selector.OrphanSkipped || js.AgentNote != "" || strings.Contains(errs, "orphan_candidates") {
		t.Errorf("skip: handling %v, note %q\n%s", js.Handling, js.AgentNote, errs)
	}
	if r := js.reason("a", "a/y_check.sh"); r != selector.ReasonCaller {
		t.Errorf("added test reason %q", r)
	}
	if r := js.reason("a", "a/x_check.sh"); r != selector.ReasonReference {
		t.Errorf("reference test reason %q", r)
	}
	js, _ = sel("--orphans=skip")
	if got := js.Tests["a"]; !reflect.DeepEqual(got, []string{"a/x_check.sh"}) {
		t.Errorf("skip alone: a %v", got)
	}

	// --add by test file path: repository-relative, or relative to -C.
	js, _ = sel("--orphans=skip", "--add", "b/z_check.sh")
	if got := js.Tests["b"]; !reflect.DeepEqual(got, []string{"b/z_check.sh"}) {
		t.Errorf("add by path: b %v", got)
	}
	var out, errb bytes.Buffer
	if code := Main([]string{"select", "-C", filepath.Join(g.dir, "b"), "--orphans=skip", "--add", "./z_check.sh", "--format", "json"}, &out, &errb); code != 0 {
		t.Fatalf("add relative to -C: %d %s", code, errb.String())
	} else if !strings.Contains(out.String(), `"b/z_check.sh"`) {
		t.Errorf("add relative to -C: %s", out.String())
	}

	// --add never deselects: with the full policy everything stays selected.
	full, _ := sel("--orphans=full")
	plus, _ := sel("--orphans=full", "--add", "b:z_check.sh", "--add", "a:a/x_check.sh")
	for suite, ids := range full.Tests {
		for _, id := range ids {
			if !slices.Contains(plus.Tests[suite], id) {
				t.Errorf("--add deselected %s:%s", suite, id)
			}
		}
	}
	if !slices.Contains(plus.Tests["b"], "b/z_check.sh") || plus.reason("a", "a/x_check.sh") != selector.ReasonReference {
		t.Errorf("--add with full: %+v", plus.Tests)
	}
	// --orphans=full applies the policy without asking the agent.
	if full.AgentNote != "" || full.Handling["a/data.cfg"] != selector.OrphanPolicy {
		t.Errorf("full: note %q handling %v", full.AgentNote, full.Handling)
	}
	if agent, _ := sel("--orphans=agent"); !strings.Contains(agent.AgentNote, "agent unavailable") {
		t.Errorf("--orphans=agent: note %q", agent.AgentNote)
	}

	// Errors: unknown tests and suites, a bad --orphans.
	for _, c := range []struct {
		args []string
		msg  string
	}{
		{[]string{"--add", "a:nope_check.sh"}, "no test unit or test file"},
		{[]string{"--add", "zz:x"}, "unknown suite"},
		{[]string{"--add", "src/a/lib"}, "no test unit or test file"},
		{[]string{"--orphans=maybe"}, "invalid --orphans"},
	} {
		for _, cmd := range []string{"select", "run"} {
			code, _, errs := main(append([]string{cmd}, c.args...)...)
			if code != 2 || !strings.Contains(errs, c.msg) {
				t.Errorf("%s %v: %d %s", cmd, c.args, code, errs)
			}
		}
	}

	// A run records the skip, and a run without a decision says how to make one.
	code, _, errs := main("run", "--dry-run")
	if code != 0 || !strings.Contains(errs, "agent unavailable") || !strings.Contains(errs, "--add") {
		t.Errorf("run without a decision: %d %s", code, errs)
	}
	report := filepath.Join(t.TempDir(), "report.json")
	code, _, errs = main("run", "--orphans=skip", "--add", "a/y_check.sh", "--report", report)
	if code != 0 || !strings.Contains(errs, "skipped by caller") {
		t.Fatalf("run --orphans=skip: %d %s", code, errs)
	}
	data, _ := os.ReadFile(report)
	var rep callerSelection
	if err := json.Unmarshal(data, &rep); err != nil || !reflect.DeepEqual(rep.Tests["a"], []string{"a/x_check.sh", "a/y_check.sh"}) || len(rep.Tests["b"]) != 0 || len(rep.Candidates) != 2 {
		t.Errorf("report: %v %+v", err, rep)
	}
	st, err := state.Load(filepath.Join(g.dir, ".smates", "state.json.gz"))
	if err != nil || len(st.Runs) == 0 {
		t.Fatalf("state: %v", err)
	}
	if last := st.Runs[len(st.Runs)-1]; last.Orphans[selector.OrphanSkipped] != 1 || last.Orphans[selector.OrphanReference] != 1 {
		t.Errorf("run record orphans: %v", last.Orphans)
	}
}

func TestUnitFor(t *testing.T) {
	su := &selector.Suite{
		Name:  "go",
		Suite: &runner.Suite{Suite: config.Suite{Root: ".", Tests: []string{"**/*_test.go"}}},
		Tests: []*selector.Test{{ID: "pkg/a"}, {ID: "pkg/b"}},
		Known: map[string][]string{"pkg/a": {"pkg/a/x_test.go", "pkg/a/x.go"}},
	}
	for in, want := range map[string]string{
		"pkg/a":           "pkg/a",
		"pkg/a/x_test.go": "pkg/a", // known test file of the unit
		"pkg/b/y_test.go": "pkg/b", // a test file of a package unit, not recorded yet
		"pkg/a/x.go":      "",      // not a test file
		"pkg/c/z_test.go": "",
	} {
		if got := strings.Join(unitFor(su, in), ","); got != want {
			t.Errorf("unitFor(%q) = %q, want %q", in, got, want)
		}
	}

	// Go with granularity: func: a test file adds every test function it declares.
	fn := &selector.Suite{
		Name:  "go",
		Suite: &runner.Suite{Suite: config.Suite{Root: ".", Tests: []string{"**/*_test.go"}}},
		Tests: []*selector.Test{{ID: "pkg/a#TestOne"}, {ID: "pkg/a#TestTwo"}, {ID: "pkg/a#TestOther"}},
		Files: map[string][]string{
			"pkg/a#TestOne":   {"pkg/a/x_test.go"},
			"pkg/a#TestTwo":   {"pkg/a/x_test.go"},
			"pkg/a#TestOther": {"pkg/a/y_test.go"},
		},
	}
	if got := strings.Join(unitFor(fn, "pkg/a/x_test.go"), ","); got != "pkg/a#TestOne,pkg/a#TestTwo" {
		t.Errorf("func granularity: unitFor(x_test.go) = %q, want both functions", got)
	}
	if got := strings.Join(unitFor(fn, "pkg/a#TestOther"), ","); got != "pkg/a#TestOther" {
		t.Errorf("func granularity: unitFor(unit) = %q", got)
	}
}

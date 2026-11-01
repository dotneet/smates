package cli

import (
	"bytes"
	"encoding/json"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/dotneet/smates/internal/state"
	"github.com/dotneet/smates/internal/trace"
)

// TestMain lets this test binary act as smates' `go test -exec` wrapper, as the smates
// binary does (package-granularity recording runs the test binaries through it).
func TestMain(m *testing.M) {
	if trace.IsGoExec() {
		os.Exit(trace.GoExec(os.Args[1:]))
	}
	os.Exit(m.Run())
}

// goFuncRepo is a Go module whose tests are selected per function.
type goFuncRepo struct {
	t *testing.T
	g *gitRepo
}

func newGoFuncRepo(t *testing.T, command string) *goFuncRepo {
	t.Helper()
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go not on PATH")
	}
	clearCI(t)
	t.Setenv("GITHUB_OUTPUT", "")
	t.Setenv("GITHUB_STEP_SUMMARY", "")
	t.Setenv("GOFLAGS", "")
	noAgentCredentials(t)
	r := &goFuncRepo{t: t, g: newGitRepo(t)}
	r.write("smates.yml", "version: 1\norphans: ignore\nsuites:\n  - name: go\n    runner: go\n    granularity: func\n    command: "+command+"\n")
	r.write(".gitignore", ".smates/\n")
	r.write("go.mod", "module example.com/fx\n\ngo 1.21\n")
	r.write("calc/add.go", "package calc\n\nfunc Add(a, b int) int { return a + b }\n")
	r.write("calc/mul.go", "package calc\n\nimport \"example.com/fx/util\"\n\nfunc Mul(a, b int) int { return util.Id(a * b) }\n")
	r.write("util/util.go", "package util\n\nfunc Id(x int) int { return x }\n")
	r.write("calc/testdata/add.txt", "3\n")
	r.write("calc/add_test.go", `package calc

import (
	"os"
	"strings"
	"testing"
)

func TestAdd(t *testing.T) {
	want, _ := os.ReadFile("testdata/add.txt")
	if got := Add(1, 2); strings.TrimSpace(string(want)) != "3" || got != 3 {
		t.Fatalf("Add = %d", got)
	}
}
`)
	r.write("calc/mul_test.go", "package calc\n\nimport \"testing\"\n\nfunc TestMul(t *testing.T) { check(t, Mul(2, 3), 6) }\n")
	r.write("calc/helper_test.go", "package calc\n\nimport \"testing\"\n\nfunc check(t *testing.T, got, want int) {\n\tif got != want {\n\t\tt.Fatalf(\"got %d, want %d\", got, want)\n\t}\n}\n")
	r.g.git("add", "-A")
	r.g.commit("fixture")
	return r
}

func (r *goFuncRepo) write(rel, content string) {
	r.t.Helper()
	p := filepath.Join(r.g.dir, filepath.FromSlash(rel))
	os.MkdirAll(filepath.Dir(p), 0o755)
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		r.t.Fatal(err)
	}
}

func (r *goFuncRepo) run(args ...string) (int, string, string) {
	var out, errb bytes.Buffer
	code := Main(append(args[:1:1], append([]string{"-C", r.g.dir, "--no-agent"}, args[1:]...)...), &out, &errb)
	return code, out.String(), errb.String()
}

// selected returns the selected units and their reasons.
func (r *goFuncRepo) selected() map[string]string {
	r.t.Helper()
	code, out, errs := r.run("select", "--format", "json")
	if code != 0 {
		r.t.Fatalf("select: %d %s", code, errs)
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
		r.t.Fatalf("%v\n%s", err, out)
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

// runReport runs `smates run` with a report and returns the per-unit results.
func (r *goFuncRepo) runReport(args ...string) (int, map[string]string, string) {
	r.t.Helper()
	report := filepath.Join(r.t.TempDir(), "report.json")
	code, out, errs := r.run(append([]string{"run", "--report", report}, args...)...)
	data, err := os.ReadFile(report)
	if err != nil {
		r.t.Fatalf("run %v: %d %v\n%s\n%s", args, code, err, out, errs)
	}
	var rep struct {
		Results map[string]map[string]string `json:"results"`
	}
	if err := json.Unmarshal(data, &rep); err != nil {
		r.t.Fatal(err)
	}
	return code, rep.Results["go"], out + errs
}

func (r *goFuncRepo) state() *state.State {
	r.t.Helper()
	st, err := state.Load(filepath.Join(r.g.dir, ".smates", "state.json.gz"))
	if err != nil {
		r.t.Fatal(err)
	}
	return st
}

func keys(m map[string]string) []string { return slices.Sorted(maps.Keys(m)) }

func TestGoFuncSelection(t *testing.T) {
	r := newGoFuncRepo(t, "go test")
	code, res, out := r.runReport("--all")
	if code != 0 || len(res) != 2 || res["calc#TestAdd"] != state.Pass || res["calc#TestMul"] != state.Pass {
		t.Fatalf("baseline: %d %v\n%s", code, res, out)
	}
	tests := r.state().Suites["go"].Tests
	add, mul := tests["calc#TestAdd"], tests["calc#TestMul"]
	if add == nil || mul == nil {
		t.Fatalf("entries %v", slices.Sorted(maps.Keys(tests)))
	}
	// Coverage: each function depends on the bodies it executed, and on the
	// declarations of the rest of what it imports.
	for key, want := range map[string]bool{"calc/add.go": true, "util/util.go": false, "util/util.go#decls": true, "calc/testdata/add.txt": true, "calc/add_test.go": true, "calc/mul_test.go": false, "calc/mul_test.go#decls": true, "calc/": true} {
		if _, ok := add.Deps[key]; ok != want {
			t.Errorf("TestAdd depends on %s: %v, want %v", key, ok, want)
		}
	}
	for key, want := range map[string]bool{"calc/mul.go": true, "util/util.go": true, "calc/add.go": false, "calc/add.go#decls": true, "calc/helper_test.go": true, "calc/testdata/add.txt": false} {
		if _, ok := mul.Deps[key]; ok != want {
			t.Errorf("TestMul depends on %s: %v, want %v", key, ok, want)
		}
	}
	if got := r.selected(); len(got) != 0 {
		t.Fatalf("nothing changed, selected %v", got)
	}

	step := func(name string, want ...string) {
		t.Helper()
		got := r.selected()
		if !slices.Equal(keys(got), want) {
			t.Errorf("%s: selected %v, want %v", name, got, want)
		}
	}
	// A function body only TestMul executed.
	r.write("util/util.go", "package util\n\nfunc Id(x int) int { return x + 0 }\n")
	step("util body", "calc#TestMul")
	code, res, out = r.runReport()
	if code != 0 || len(res) != 1 || res["calc#TestMul"] != state.Pass {
		t.Fatalf("run after util edit: %d %v\n%s", code, res, out)
	}
	if !strings.Contains(out, "calc#TestMul") || strings.Contains(out, "calc#TestAdd") {
		t.Errorf("only TestMul should run:\n%s", out)
	}
	step("after recording")

	// A body only TestAdd executed; data only TestAdd read; helpers only TestMul uses.
	r.write("calc/add.go", "package calc\n\nfunc Add(a, b int) int { return b + a }\n")
	step("add body", "calc#TestAdd")
	r.write("calc/testdata/add.txt", "3 \n")
	step("add data", "calc#TestAdd")
	r.write("calc/helper_test.go", "package calc\n\nimport \"testing\"\n\nfunc check(t *testing.T, got, want int) {\n\tt.Helper()\n\tif got != want {\n\t\tt.Fatalf(\"got %d, want %d\", got, want)\n\t}\n}\n")
	step("helper", "calc#TestAdd", "calc#TestMul")
	if code, res, out := r.runReport(); code != 0 || len(res) != 2 {
		t.Fatalf("run: %d %v\n%s", code, res, out)
	}
	step("after recording")

	// Declarations count for every function of the importing packages.
	r.write("calc/add.go", "package calc\n\nconst Two = 2\n\nfunc Add(a, b int) int { return b + a }\n")
	step("add declarations", "calc#TestAdd", "calc#TestMul")
	r.write("calc/add.go", "package calc\n\nfunc Add(a, b int) int { return b + a }\n")
	step("reverted")

	// A changed test file selects its functions; a new function has no entry (it is
	// selected as new, or locally by a diff against the default branch).
	r.write("calc/add_test.go", strings.Replace(mustRead(t, filepath.Join(r.g.dir, "calc/add_test.go")), "func TestAdd", "func TestSub(t *testing.T) {\n\tif Add(1, -1) != 0 {\n\t\tt.Fatal(\"sub\")\n\t}\n}\n\nfunc TestAdd", 1))
	got := r.selected()
	if !slices.Equal(keys(got), []string{"calc#TestAdd", "calc#TestSub"}) || got["calc#TestSub"] == "changed" {
		t.Errorf("new function: %v", got)
	}

	// A failing function: only it fails, the others are not run.
	r.write("util/util.go", "package util\n\nfunc Id(x int) int { return x + 1 }\n")
	code, res, out = r.runReport()
	if code == 0 || res["calc#TestMul"] != state.Fail || res["calc#TestAdd"] != state.Pass || res["calc#TestSub"] != state.Pass {
		t.Errorf("failing run: %d %v\n%s", code, res, out)
	}
	step("after a failure", "calc#TestMul")

	// Without recording, the selected functions run with -run and report their outcome.
	code, res, out = r.runReport("--no-record")
	if code == 0 || len(res) != 1 || res["calc#TestMul"] != state.Fail {
		t.Errorf("--no-record: %d %v\n%s", code, res, out)
	}
	r.write("util/util.go", "package util\n\nfunc Id(x int) int { return x }\n")
	code, res, out = r.runReport("--no-record")
	if code != 0 || len(res) != 1 || res["calc#TestMul"] != state.Pass {
		t.Errorf("--no-record after fix: %d %v\n%s", code, res, out)
	}
	if _, out, _ := r.run("run", "--dry-run"); !strings.Contains(out, "-run ^(TestMul)$ ./calc") {
		t.Errorf("dry run: %s", out)
	}

	// A file added to the package directory selects every function of the package.
	r.write("calc/notes.txt", "x\n")
	step("new file", "calc#TestAdd", "calc#TestMul", "calc#TestSub")
}

// When the suite command cannot be used to build a coverage binary, a package is
// recorded as a whole: every function depends on all of it.
func TestGoFuncFallback(t *testing.T) {
	r := newGoFuncRepo(t, "go test -run .")
	code, res, out := r.runReport("--all")
	if code != 0 || res["calc#TestAdd"] != state.Pass || res["calc#TestMul"] != state.Pass {
		t.Fatalf("baseline: %d %v\n%s", code, res, out)
	}
	if !strings.Contains(out, "recorded at package granularity") {
		t.Errorf("no fallback warning:\n%s", out)
	}
	add := r.state().Suites["go"].Tests["calc#TestAdd"]
	if _, ok := add.Deps["util/util.go"]; !ok {
		t.Errorf("fallback entry lacks the package's dependencies: %v", add.Deps)
	}
	r.write("util/util.go", "package util\n\nfunc Id(x int) int { return x + 0 }\n")
	if got := r.selected(); !slices.Equal(keys(got), []string{"calc#TestAdd", "calc#TestMul"}) {
		t.Errorf("fallback selection: %v", got)
	}
}

func mustRead(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// TestGoFuncDeps: `smates deps` takes a test function unit (dir#TestName), with the
// directory given relative to the repository or as a path.
func TestGoFuncDeps(t *testing.T) {
	r := newGoFuncRepo(t, "go test")
	deps := func(arg string) (int, string, string) {
		var out, errb bytes.Buffer
		code := Main([]string{"deps", "-C", r.g.dir, arg}, &out, &errb)
		return code, out.String(), errb.String()
	}
	for _, arg := range []string{"calc#TestAdd", "./calc#TestAdd", filepath.Join(r.g.dir, "calc") + "#TestAdd"} {
		code, out, errs := deps(arg)
		if code != 0 || !strings.Contains(out, "calc/add_test.go") || !strings.Contains(out, "calc/add.go") {
			t.Errorf("deps %s: exit %d\n%s%s", arg, code, out, errs)
		}
	}
	// The package directory alone names the functions to pick from.
	if code, _, errs := deps("calc"); code == 0 || !strings.Contains(errs, "calc#TestAdd") {
		t.Errorf("deps calc: exit %d\n%s", code, errs)
	}
}

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

	"github.com/dotneet/smates/internal/runner"
	"github.com/dotneet/smates/internal/state"
	"github.com/dotneet/smates/internal/trace"
)

// TestMain lets this test binary act as smates' `go test -exec` wrapper, as the smates
// binary does (package-granularity recording runs the test binaries through it, and
// compile checks do not run them), and as smates itself with SMATES_TEST_MAIN=1 (for
// tests that signal a smates process).
func TestMain(m *testing.M) {
	if runner.IsGoCheck() {
		os.Exit(0)
	}
	if trace.IsGoExec() {
		os.Exit(trace.GoExec(os.Args[1:]))
	}
	if os.Getenv("SMATES_TEST_MAIN") == "1" {
		os.Exit(Main(os.Args[1:], os.Stdout, os.Stderr))
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
	if code != 0 || len(res) != 3 || res["calc#TestAdd"] != state.Pass || res["calc#TestMul"] != state.Pass || res["calc#(build)"] != state.Pass {
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
	// A function body only TestMul executed: TestMul and the package's compile check.
	r.write("util/util.go", "package util\n\nfunc Id(x int) int { return x + 0 }\n")
	step("util body", "calc#(build)", "calc#TestMul")
	code, res, out = r.runReport()
	if code != 0 || len(res) != 2 || res["calc#TestMul"] != state.Pass || res["calc#(build)"] != state.Pass {
		t.Fatalf("run after util edit: %d %v\n%s", code, res, out)
	}
	if !strings.Contains(out, "calc#TestMul") || strings.Contains(out, "calc#TestAdd") {
		t.Errorf("only TestMul should run:\n%s", out)
	}
	step("after recording")

	// A body only TestAdd executed; data only TestAdd read; helpers only TestMul uses.
	r.write("calc/add.go", "package calc\n\nfunc Add(a, b int) int { return b + a }\n")
	step("add body", "calc#(build)", "calc#TestAdd")
	r.write("calc/testdata/add.txt", "3 \n")
	step("add data", "calc#(build)", "calc#TestAdd")
	r.write("calc/helper_test.go", "package calc\n\nimport \"testing\"\n\nfunc check(t *testing.T, got, want int) {\n\tt.Helper()\n\tif got != want {\n\t\tt.Fatalf(\"got %d, want %d\", got, want)\n\t}\n}\n")
	step("helper", "calc#(build)", "calc#TestAdd", "calc#TestMul")
	if code, res, out := r.runReport(); code != 0 || len(res) != 3 {
		t.Fatalf("run: %d %v\n%s", code, res, out)
	}
	step("after recording")
	// Data only TestAdd reads is no input of the compile check.
	r.write("calc/testdata/add.txt", "3\n")
	step("add data", "calc#TestAdd")
	if code, res, out := r.runReport(); code != 0 || len(res) != 1 {
		t.Fatalf("run: %d %v\n%s", code, res, out)
	}

	// Declarations count for every function of the importing packages.
	r.write("calc/add.go", "package calc\n\nconst Two = 2\n\nfunc Add(a, b int) int { return b + a }\n")
	step("add declarations", "calc#(build)", "calc#TestAdd", "calc#TestMul")
	r.write("calc/add.go", "package calc\n\nfunc Add(a, b int) int { return b + a }\n")
	step("reverted")

	// A changed test file selects its functions; a new function has no entry (it is
	// selected as new, or locally by a diff against the default branch).
	r.write("calc/add_test.go", strings.Replace(mustRead(t, filepath.Join(r.g.dir, "calc/add_test.go")), "func TestAdd", "func TestSub(t *testing.T) {\n\tif Add(1, -1) != 0 {\n\t\tt.Fatal(\"sub\")\n\t}\n}\n\nfunc TestAdd", 1))
	got := r.selected()
	if !slices.Equal(keys(got), []string{"calc#(build)", "calc#TestAdd", "calc#TestSub"}) || got["calc#TestSub"] == "changed" {
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
	if code != 0 || len(res) != 2 || res["calc#TestMul"] != state.Pass || res["calc#(build)"] != state.Pass {
		t.Errorf("--no-record after fix: %d %v\n%s", code, res, out)
	}
	if _, out, _ := r.run("run", "--dry-run"); !strings.Contains(out, "-run '^(TestMul)$' ./calc") || !strings.Contains(out, "env GOFLAGS= SMATES_GOCHECK=1 go test '-run=^$' -count=1 -exec=") {
		t.Errorf("dry run: %s", out)
	}

	// A file added to the package directory selects every function of the package (the
	// compile check is selected for util.go, which no recorded run saw as it is now).
	r.write("calc/notes.txt", "x\n")
	step("new file", "calc#(build)", "calc#TestAdd", "calc#TestMul", "calc#TestSub")
}

// A body no test executes still has to compile: an edit that breaks it selects the
// compile check of its package and of every package importing it, which fail like go
// test does (a vet failure only fails the package itself, as in go test); once fixed,
// the checks pass and nothing stays selected. The test binaries are not run for it.
func TestGoFuncCompileCheck(t *testing.T) {
	r := newGoFuncRepo(t, "go test")
	marker := filepath.Join(t.TempDir(), "testmain-ran")
	t.Setenv("SMATES_TEST_MARKER", marker)
	r.write("calc/unused.go", "package calc\n\n// Double is called by no test.\nfunc Double(x int) int { return x * 2 }\n")
	r.write("shop/shop.go", "package shop\n\nimport \"example.com/fx/calc\"\n\nfunc Total(a, b int) int { return calc.Add(a, b) }\n")
	r.write("shop/shop_test.go", "package shop\n\nimport (\n\t\"os\"\n\t\"testing\"\n)\n\n"+
		"func TestMain(m *testing.M) {\n\tos.WriteFile(os.Getenv(\"SMATES_TEST_MARKER\"), nil, 0o644)\n\tos.Exit(m.Run())\n}\n\n"+
		"func TestTotal(t *testing.T) {\n\tif Total(1, 2) != 3 {\n\t\tt.Fatal(\"total\")\n\t}\n}\n")
	if code, res, out := r.runReport("--all"); code != 0 || res["calc#(build)"] != state.Pass || res["shop#(build)"] != state.Pass || res["shop#TestTotal"] != state.Pass {
		t.Fatalf("baseline: %d %v\n%s", code, res, out)
	}
	if got := r.selected(); len(got) != 0 {
		t.Fatalf("nothing changed, selected %v", got)
	}
	for _, c := range []struct {
		name, file, broken, msg string
		shop                    string // the importer's check
	}{
		{"type error in an unexecuted body", "calc/unused.go", "package calc\n\n// Double is called by no test.\nfunc Double(x int) int { return \"x\" }\n", "cannot use", state.Fail},
		{"vet failure", "calc/unused.go", "package calc\n\n// Double is called by no test.\nfunc Double(x int) int {\n\tif x == 1 || x == 1 {\n\t\treturn 2\n\t}\n\treturn x * 2\n}\n", "redundant or", state.Pass},
	} {
		orig := mustRead(t, filepath.Join(r.g.dir, c.file))
		r.write(c.file, c.broken)
		if got := r.selected(); !slices.Equal(keys(got), []string{"calc#(build)", "shop#(build)"}) {
			t.Errorf("%s: selected %v", c.name, got)
		}
		if c.shop == state.Fail {
			// Shadow mode: the functions of the packages that do not build fail too, but
			// they are no escapes: their packages' checks were selected and failed.
			report := filepath.Join(t.TempDir(), "shadow.json")
			code, out, errs := r.run("run", "--shadow", "--no-record", "--report", report)
			var rep struct {
				Results map[string]map[string]string `json:"results"`
				Escapes []string                     `json:"escapes"`
			}
			data, _ := os.ReadFile(report)
			json.Unmarshal(data, &rep)
			if code != 1 || rep.Results["go"]["shop#TestTotal"] != state.Fail || rep.Results["go"]["calc#TestAdd"] != state.Fail || len(rep.Escapes) != 0 {
				t.Errorf("%s: shadow: %d %v escapes %v\n%s%s", c.name, code, rep.Results, rep.Escapes, out, errs)
			}
		}
		os.Remove(marker)
		code, res, out := r.runReport()
		if code != 1 || len(res) != 2 || res["calc#(build)"] != state.Fail || res["shop#(build)"] != c.shop {
			t.Errorf("%s: run %d %v\n%s", c.name, code, res, out)
		}
		if !strings.Contains(out, c.msg) {
			t.Errorf("%s: the compiler's output is not shown:\n%s", c.name, out)
		}
		if _, err := os.Stat(marker); err == nil {
			t.Errorf("%s: a compile check ran a test binary", c.name)
		}
		want := map[string]string{"calc#(build)": "failed"}
		if c.shop == state.Fail {
			want["shop#(build)"] = "failed"
		}
		if got := r.selected(); !maps.Equal(got, want) {
			t.Errorf("%s: after the failure, selected %v, want %v", c.name, got, want)
		}
		r.write(c.file, orig)
		if code, res, out := r.runReport(); code != 0 || len(res) != 2 || res["calc#(build)"] != state.Pass || res["shop#(build)"] != state.Pass {
			t.Errorf("%s: fixed: %d %v\n%s", c.name, code, res, out)
		}
		if got := r.selected(); len(got) != 0 {
			t.Errorf("%s: after the fix, selected %v", c.name, got)
		}
	}

	// A test helper importing a package it does not use (imports are declarations: the
	// package's functions run too), and fail.
	orig := mustRead(t, filepath.Join(r.g.dir, "calc/helper_test.go"))
	r.write("calc/helper_test.go", strings.Replace(orig, `import "testing"`, "import (\n\t\"strings\"\n\t\"testing\"\n)", 1))
	if got := r.selected(); !slices.Equal(keys(got), []string{"calc#(build)", "calc#TestAdd", "calc#TestMul"}) {
		t.Errorf("unused import: selected %v", got)
	}
	if code, res, out := r.runReport(); code != 1 || res["calc#(build)"] != state.Fail || res["calc#TestMul"] != state.Fail || !strings.Contains(out, `"strings" imported and not used`) {
		t.Errorf("unused import: run %d %v\n%s", code, res, out)
	}
	r.write("calc/helper_test.go", orig)
	if code, res, out := r.runReport(); code != 0 || len(res) != 3 {
		t.Errorf("unused import fixed: %d %v\n%s", code, res, out)
	}
	if got := r.selected(); len(got) != 0 {
		t.Errorf("unused import fixed: selected %v", got)
	}
}

// A test that re-executes its own binary (the helper-process pattern) depends on the
// code the child ran: a change there selects it.
func TestGoFuncReexec(t *testing.T) {
	r := newGoFuncRepo(t, "go test")
	r.write("calc/crash.go", "package calc\n\nimport \"fmt\"\n\n// Crash runs in a child process of its test.\nfunc Crash() { fmt.Println(\"fine\") }\n")
	r.write("calc/crash_test.go", `package calc

import (
	"os"
	"os/exec"
	"testing"
)

func TestCrasher(t *testing.T) {
	if os.Getenv("BE_CRASHER") == "1" {
		Crash()
		return
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestCrasher$")
	cmd.Env = append(os.Environ(), "BE_CRASHER=1")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("child failed: %v\n%s", err, out)
	}
}
`)
	if code, res, out := r.runReport("--all"); code != 0 || res["calc#TestCrasher"] != state.Pass {
		t.Fatalf("baseline: %d %v\n%s", code, res, out)
	}
	if e := r.state().Suites["go"].Tests["calc#TestCrasher"]; e == nil || e.Deps["calc/crash.go"] == "" {
		t.Fatalf("TestCrasher does not depend on crash.go: %+v", e)
	}
	r.write("calc/crash.go", "package calc\n\nimport \"fmt\"\n\n// Crash runs in a child process of its test.\nfunc Crash() { fmt.Println(\"fine\"); panic(\"boom\") }\n")
	if got := r.selected(); !slices.Equal(keys(got), []string{"calc#(build)", "calc#TestCrasher"}) {
		t.Errorf("crash.go body: selected %v", got)
	}
	if code, res, out := r.runReport(); code != 1 || res["calc#TestCrasher"] != state.Fail {
		t.Errorf("crash.go body: run %d %v\n%s", code, res, out)
	}
	// The failing run still depends on crash.go: the child that panicked wrote no
	// coverage, so the function depends on every file it could have run.
	if e := r.state().Suites["go"].Tests["calc#TestCrasher"]; e == nil || e.Deps["calc/crash.go"] == "" || e.Deps["calc/mul.go"] == "" {
		t.Errorf("failed TestCrasher deps: %+v", e)
	}
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
	if got := r.selected(); !slices.Equal(keys(got), []string{"calc#(build)", "calc#TestAdd", "calc#TestMul"}) {
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
	// A subtest names its function's unit.
	for _, arg := range []string{"calc#TestAdd", "./calc#TestAdd", filepath.Join(r.g.dir, "calc") + "#TestAdd", "calc#TestAdd/sub_case"} {
		code, out, errs := deps(arg)
		if code != 0 || !strings.Contains(out, "calc/add_test.go") || !strings.Contains(out, "calc/add.go") {
			t.Errorf("deps %s: exit %d\n%s%s", arg, code, out, errs)
		}
	}
	// A function of the root package (unit ".#TestRoot"), named by an absolute path
	// to the repository or by ".." from a subdirectory.
	r.write("root.go", "package fx\n\nfunc Root() int { return 1 }\n")
	r.write("root_test.go", "package fx\n\nimport \"testing\"\n\nfunc TestRoot(t *testing.T) { _ = Root() }\n")
	for _, arg := range []string{".#TestRoot", r.g.dir + "#TestRoot"} {
		if code, out, errs := deps(arg); code != 0 || !strings.Contains(out, "root_test.go") {
			t.Errorf("deps %s: exit %d\n%s%s", arg, code, out, errs)
		}
	}

	// The package directory alone names the functions to pick from.
	if code, _, errs := deps("calc"); code == 0 || !strings.Contains(errs, "calc#TestAdd") {
		t.Errorf("deps calc: exit %d\n%s", code, errs)
	}

	t.Chdir(filepath.Join(r.g.dir, "calc"))
	if code, out, errs := deps("..#TestRoot"); code != 0 || !strings.Contains(out, "root_test.go") {
		t.Errorf("deps ..#TestRoot from calc/: exit %d\n%s%s", code, out, errs)
	}
}

package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/dotneet/smates/internal/config"
	"github.com/dotneet/smates/internal/state"
	"github.com/dotneet/smates/internal/testutil"
)

// goModule writes a Go module into a fresh repository and returns its func-granularity
// run context.
func goModule(t *testing.T, files map[string]string) *RunContext {
	t.Helper()
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go not on PATH")
	}
	t.Setenv("GOWORK", "")
	t.Setenv("GOFLAGS", "")
	repo := tempRepo(t)
	testutil.Write(t, repo.Root, "go.mod", "module example.com/m\n\ngo 1.22\n")
	for rel, content := range files {
		testutil.Write(t, repo.Root, rel, content)
	}
	suites, err := Resolve(repo, []config.Suite{{Name: "go", Root: ".", Runner: "go", Granularity: config.GranularityFunc}})
	if err != nil {
		t.Fatal(err)
	}
	return &RunContext{Repo: repo, Suite: suites[0], Record: true, TraceDir: t.TempDir(), ShimDir: t.TempDir(), Stdout: &bytes.Buffer{}, Stderr: &bytes.Buffer{}}
}

// A compile check fails the packages go test cannot build or vet, and every package
// importing one of them, with the compiler's output; it passes the others. The test
// binaries are not run: a TestMain leaves no trace.
func TestRunBuildChecks(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "testmain-ran")
	t.Setenv("SMATES_TEST_MARKER", marker)
	const testMain = "import (\"os\"; \"testing\")\n\nfunc TestMain(m *testing.M) {\n\tos.WriteFile(os.Getenv(\"SMATES_TEST_MARKER\"), nil, 0o644)\n\tos.Exit(m.Run())\n}\n"
	rc := goModule(t, map[string]string{
		// a: a body no test executes no longer type-checks.
		"a/a.go":      "package a\n\nfunc Used() int { return 1 }\n\nfunc Unused(x int) int { return \"x\" }\n",
		"a/a_test.go": "package a\n\n" + testMain + "\nfunc TestUsed(t *testing.T) { Used() }\n",
		// b imports a: it does not build either.
		"b/b.go":      "package b\n\nimport \"example.com/m/a\"\n\nfunc B() int { return a.Used() }\n",
		"b/b_test.go": "package b\n\nimport \"testing\"\n\nfunc TestB(t *testing.T) { B() }\n",
		// c builds; its TestMain must not run.
		"c/c.go":      "package c\n\nfunc C() int { return 3 }\n",
		"c/c_test.go": "package c\n\n" + testMain + "\nfunc TestC(t *testing.T) { C() }\n",
		// v builds but fails go test's vet checks; u has a test helper importing a
		// package it does not use.
		"v/v.go":         "package v\n\nimport \"fmt\"\n\nfunc V() string { return fmt.Sprintf(\"%d\", \"s\") }\n",
		"v/v_test.go":    "package v\n\nimport \"testing\"\n\nfunc TestV(t *testing.T) {}\n",
		"u/u.go":         "package u\n",
		"u/u_test.go":    "package u\n\nimport \"testing\"\n\nfunc TestU(t *testing.T) {}\n",
		"u/help_test.go": "package u\n\nimport \"strings\"\n\nfunc help() int { return 1 }\n",
	})
	check := func(self string) map[string]*Observation {
		t.Helper()
		rc.Self = self
		res, err := runBuildChecks(context.Background(), rc, []string{"a", "b", "c", "v", "u"})
		if err != nil {
			t.Fatal(err)
		}
		if res.ExitCode == 0 {
			t.Errorf("exit code 0 with packages that do not build")
		}
		return res.Observations
	}
	// Without smates' path the binaries run with -run '^$' (TestMain runs); with it, an
	// -exec program that does nothing stands in for smates' check mode.
	for _, self := range []string{"", "true"} {
		if self != "" {
			if runtime.GOOS == "windows" {
				continue
			}
			p, err := exec.LookPath(self)
			if err != nil {
				continue
			}
			self = p
			os.Remove(marker)
		}
		obs := check(self)
		for unit, want := range map[string]string{"a#(build)": state.Fail, "b#(build)": state.Fail, "c#(build)": state.Pass, "v#(build)": state.Fail, "u#(build)": state.Fail} {
			if o := obs[unit]; o == nil || o.Status != want {
				t.Errorf("self %q: %s: %+v, want %s\n%s", self, unit, o, want, rc.Stdout)
			}
		}
		if f := obs["a#(build)"].Failure; !strings.Contains(f, "a/a.go") {
			t.Errorf("a: failure %q", f)
		}
		if f := obs["b#(build)"].Failure; !strings.Contains(f, "a/a.go") {
			t.Errorf("b: failure %q (should show why the package it imports does not build)", f)
		}
		if f := obs["v#(build)"].Failure; !strings.Contains(f, "Sprintf") {
			t.Errorf("v: failure %q", f)
		}
		if f := obs["u#(build)"].Failure; !strings.Contains(f, `"strings" imported and not used`) {
			t.Errorf("u: failure %q", f)
		}
		if _, err := os.Stat(marker); self != "" && err == nil {
			t.Errorf("a compile check ran a TestMain")
		}
	}

	// Fixed, everything passes.
	testutil.Write(t, rc.Repo.Root, "a/a.go", "package a\n\nfunc Used() int { return 1 }\n\nfunc Unused(x int) int { return x }\n")
	testutil.Write(t, rc.Repo.Root, "v/v.go", "package v\n\nimport \"fmt\"\n\nfunc V() string { return fmt.Sprintf(\"%s\", \"s\") }\n")
	os.Remove(filepath.Join(rc.Repo.Root, "u/help_test.go"))
	res, err := runBuildChecks(context.Background(), rc, []string{"a", "b", "c", "v", "u"})
	if err != nil || res.ExitCode != 0 {
		t.Fatalf("fixed: %d %v\n%s", res.ExitCode, err, rc.Stdout)
	}
	for _, u := range []string{"a#(build)", "b#(build)", "c#(build)", "v#(build)", "u#(build)"} {
		if o := res.Observations[u]; o == nil || o.Status != state.Pass {
			t.Errorf("fixed: %s: %+v", u, o)
		}
	}
}

// The compile check takes the build flags and -vet setting of GOFLAGS, the command and
// the arguments passed through, but no test flags that would change what it runs.
func TestCheckArgv(t *testing.T) {
	rc := goRC(t, "go test -race -vet=off -json -count=3 -run TestX", map[string]string{"GOFLAGS": "-mod=mod -tags=a -v -coverprofile=c.out -exec=wrap -vet=all"})
	rc.ExtraArgs = []string{"-tags=integration", "-failfast"}
	rc.Self = "/opt/my tools/smates"
	argv, env := checkArgv(rc, []string{"./p", "./q"})
	want := []string{"go", "test", "-run=^$", "-count=1", "-exec='/opt/my tools/smates'", "-race", "-tags=integration", "-vet=off", "./p", "./q"}
	if !slices.Equal(argv, want) {
		t.Errorf("argv\n got %q\nwant %q", argv, want)
	}
	if !slices.Equal(env, []string{"GOFLAGS=-mod=mod -tags=a -vet=all", GoCheckEnv + "=1"}) {
		t.Errorf("env %q", env)
	}
	rc.Self = ""
	if argv, env := checkArgv(rc, []string{"./p"}); slices.ContainsFunc(argv, func(a string) bool { return strings.HasPrefix(a, "-exec") }) || len(env) != 1 {
		t.Errorf("without smates' path: %q %q", argv, env)
	}
}

// A user's -overlay (of the command, else of GOFLAGS) is merged into the one adding the
// child-coverage file; relative paths in it are relative to the go command's directory.
func TestChildCoverFlags(t *testing.T) {
	goDir, work := t.TempDir(), t.TempDir()
	pkgDir := filepath.Join(goDir, "p")
	testutil.Write(t, goDir, "p/zz_smates_cover_test.go", "package p\n") // taken
	testutil.Write(t, goDir, "ov.json", `{"Replace": {"p/gen.go": "gen/p.go"}}`)
	testutil.Write(t, goDir, "env.json", `{"Replace": {"p/env.go": "gen/env.go"}}`)
	flags, err := childCoverFlags([]string{"-race", "-overlay", "ov.json", "-tags=x"}, "-overlay=env.json", goDir, pkgDir, "p", work)
	if err != nil {
		t.Fatal(err)
	}
	overlay := filepath.Join(work, "overlay.json")
	if !slices.Equal(flags, []string{"-race", "-tags=x", "-overlay=" + overlay}) {
		t.Fatalf("flags %q", flags)
	}
	var ov struct{ Replace map[string]string }
	data, _ := os.ReadFile(overlay)
	if err := json.Unmarshal(data, &ov); err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(work, "smates_cover_test.go")
	want := map[string]string{filepath.Join(goDir, "p/gen.go"): filepath.Join(goDir, "gen/p.go"), filepath.Join(pkgDir, "zz_smates_cover1_test.go"): src}
	if len(ov.Replace) != len(want) {
		t.Errorf("overlay %v, want %v", ov.Replace, want)
	}
	for k, v := range want {
		if ov.Replace[k] != v {
			t.Errorf("overlay %v, want %v", ov.Replace, want)
		}
	}
	if b, _ := os.ReadFile(src); !strings.Contains(string(b), "package p\n") || !strings.Contains(string(b), childCoverEnv) {
		t.Errorf("source:\n%s", b)
	}
	// An -overlay only in GOFLAGS is merged too.
	if _, err := childCoverFlags([]string{"-race"}, "-mod=mod -overlay=env.json", goDir, pkgDir, "p", work); err != nil {
		t.Fatal(err)
	}
	data, _ = os.ReadFile(overlay)
	if !strings.Contains(string(data), "env.go") {
		t.Errorf("GOFLAGS overlay not merged: %s", data)
	}
	// Without a package name nothing is added.
	if flags, err := childCoverFlags([]string{"-race"}, "", goDir, pkgDir, "", work); err != nil || !slices.Equal(flags, []string{"-race"}) {
		t.Errorf("no name: %q %v", flags, err)
	}
}

// A test that re-executes its own binary (the helper-process pattern) depends on what
// the child ran, whether the child returns or calls os.Exit (as the classic
// TestHelperProcess does: go's exit hook writes its counters to $GOCOVERDIR). A child
// that writes no coverage (it panics) makes its function's coverage uncertain: it
// depends on the content of every file of its closure. A child running the program's
// own command line (a TestMain dispatching to a CLI) gets its arguments untouched: one
// rejecting unknown arguments, or parsing them with a strict FlagSet, still passes.
func TestRecordFuncsReexec(t *testing.T) {
	rc := goModule(t, map[string]string{
		"p/crash.go": "package p\n\nimport \"fmt\"\n\nfunc Crash() { fmt.Println(\"fine\") }\n",
		"p/exit.go":  "package p\n\nfunc Exit() int { return 0 }\n",
		"p/panic.go": "package p\n\nfunc Panic() { panic(\"boom\") }\n",
		"p/cli.go": `package p

import (
	"flag"
	"fmt"
	"io"
)

// Run takes positional arguments only.
func Run(args []string) int {
	for _, a := range args {
		if len(a) > 0 && a[0] == '-' {
			fmt.Println("unknown flag", a)
			return 2
		}
	}
	return 0
}

// RunFlags parses its arguments with a FlagSet of its own.
func RunFlags(args []string) int {
	fs := flag.NewFlagSet("tool", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	name := fs.String("name", "", "")
	if err := fs.Parse(args); err != nil || *name != "x" || fs.NArg() != 1 {
		fmt.Println("bad arguments", args, err)
		return 2
	}
	return 0
}
`,
		"p/main_test.go": `package p

import (
	"os"
	"os/exec"
	"testing"
)

func TestMain(m *testing.M) {
	switch os.Getenv("BE_CLI") {
	case "positional":
		os.Exit(Run(os.Args[1:]))
	case "flags":
		os.Exit(RunFlags(os.Args[1:]))
	}
	os.Exit(m.Run())
}

func runCLI(t *testing.T, mode string, args ...string) {
	cmd := exec.Command(os.Args[0], args...)
	cmd.Env = append(os.Environ(), "BE_CLI="+mode)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("cli failed: %v\n%s", err, out)
	}
}

func TestCLIPositional(t *testing.T) { runCLI(t, "positional", "hello", "world") }

func TestCLIFlags(t *testing.T) { runCLI(t, "flags", "-name=x", "arg") }
`,
		"p/other.go": "package p\n\nfunc Other() int { return 1 }\n",
		"p/crash_test.go": `package p

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

func TestHelperProcess(t *testing.T) {
	if os.Getenv("BE_HELPER") != "1" {
		return
	}
	os.Exit(Exit())
}

func TestExiter(t *testing.T) {
	cmd := exec.Command(os.Args[0], "-test.run=^TestHelperProcess$")
	cmd.Env = append(os.Environ(), "BE_HELPER=1")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("child failed: %v\n%s", err, out)
	}
}

func TestPanicker(t *testing.T) {
	if os.Getenv("BE_PANICKER") == "1" {
		Panic()
		return
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestPanicker$")
	cmd.Env = append(os.Environ(), "BE_PANICKER=1")
	if out, err := cmd.CombinedOutput(); err == nil {
		t.Fatalf("child did not fail\n%s", out)
	}
}

func TestPlain(t *testing.T) { Other() }
`,
	})
	rc.Known = func(string) []string {
		return []string{"p/cli.go#decls", "p/crash.go#decls", "p/exit.go#decls", "p/other.go#decls", "p/panic.go#decls"}
	}
	res, err := recordFuncs(context.Background(), rc, []string{"p"}, map[string][]string{"p": {"TestCrasher", "TestExiter", "TestPanicker", "TestPlain", "TestCLIPositional", "TestCLIFlags"}})
	if err != nil {
		t.Fatal(err)
	}
	files := func(unit string) []string {
		o := res.Observations[unit]
		if o == nil || o.Status != state.Pass {
			t.Fatalf("%s: %+v\n%s", unit, o, rc.Stdout)
		}
		var out []string
		for f := range o.Files {
			if strings.HasSuffix(f, ".go") {
				out = append(out, f)
			}
		}
		slices.Sort(out)
		return out
	}
	for unit, want := range map[string][]string{
		"p#TestCrasher":  {"p/crash.go"},
		"p#TestExiter":   {"p/exit.go"},
		"p#TestPanicker": {"p/cli.go", "p/crash.go", "p/exit.go", "p/other.go", "p/panic.go"}, // the child wrote no coverage
		"p#TestPlain":    {"p/other.go"},
		// The children's code, which only they ran.
		"p#TestCLIPositional": {"p/cli.go"},
		"p#TestCLIFlags":      {"p/cli.go"},
	} {
		if got := files(unit); !slices.Equal(got, want) {
			t.Errorf("%s: %v, want %v", unit, got, want)
		}
	}
	if _, err := os.Stat(filepath.Join(rc.Repo.Root, "p/zz_smates_cover_test.go")); err == nil {
		t.Error("the child-coverage file was written into the package")
	}
}

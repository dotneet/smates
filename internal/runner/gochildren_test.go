package runner

import (
	"context"
	"os"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/dotneet/smates/internal/state"
	"github.com/dotneet/smates/internal/trace"
)

// TestMain lets this test binary act as smates' `go test -exec` wrapper, as the smates
// binary does (package recording runs the test binaries through rc.Self).
func TestMain(m *testing.M) {
	if IsGoCheck() {
		os.Exit(0)
	}
	if trace.IsGoExec() {
		os.Exit(trace.GoExec(os.Args[1:]))
	}
	os.Exit(m.Run())
}

// childTests are tests whose children read files: a re-execution of the test binary
// that ends in os.Exit (the classic TestHelperProcess: its test log is lost), one that
// returns (it writes its own), and a shell. TestPlain starts nothing.
const childTests = `package p

import (
	"os"
	"os/exec"
	"testing"
)

func TestHelperProcess(t *testing.T) {
	switch os.Getenv("BE_HELPER") {
	case "exit":
		os.ReadFile("../shared/exit.txt")
		os.Exit(0)
	case "return":
		os.ReadFile("../shared/return.txt")
	}
}

func helper(t *testing.T, mode string) {
	cmd := exec.Command(os.Args[0], "-test.run=^TestHelperProcess$")
	cmd.Env = append(os.Environ(), "BE_HELPER="+mode)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("child failed: %v\n%s", err, out)
	}
}

func TestExiter(t *testing.T) { helper(t, "exit") }

func TestReturner(t *testing.T) { helper(t, "return") }

func TestShell(t *testing.T) {
	if out, err := exec.Command("sh", "-c", "cat testdata/sh.txt").CombinedOutput(); err != nil {
		t.Fatalf("sh: %v\n%s", err, out)
	}
}

func TestPlain(t *testing.T) { os.ReadFile("testdata/plain.txt") }
`

func childModule(t *testing.T, tests string) *RunContext {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("runs sh")
	}
	rc := goModule(t, map[string]string{
		"p/p.go":               "package p\n",
		"p/p_test.go":          tests,
		"p/testdata/sh.txt":    "sh\n",
		"p/testdata/plain.txt": "plain\n",
		"shared/exit.txt":      "exit\n",
		"shared/return.txt":    "return\n",
	})
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	rc.Self = self
	return rc
}

// nonGo lists the dependencies of an observation other than Go files and the package
// directory's listing.
func nonGo(t *testing.T, o *Observation, out string) []string {
	t.Helper()
	if o == nil || o.Status != state.Pass {
		t.Fatalf("observation %+v\n%s", o, out)
	}
	var deps []string
	for f := range o.Files {
		if !strings.Contains(f, ".go") && f != "p/" && f != "go.mod" {
			deps = append(deps, f)
		}
	}
	slices.Sort(deps)
	return deps
}

// Package recording: what the children of a package's tests read is part of the
// package's dependencies. A re-execution that returns has a test log of its own; one
// that exits early leaves its reads unknown, and the package depends on its module's
// tree; a shell depends on the tree of the directory it runs in.
func TestRecordPackageChildren(t *testing.T) {
	for name, c := range map[string]struct {
		run  string
		want []string
	}{
		"exit":   {"^(TestHelperProcess|TestExiter)$", []string{"$BE_HELPER", "./**"}},
		"return": {"^(TestHelperProcess|TestReturner)$", []string{"$BE_HELPER", "shared/return.txt"}},
		"shell":  {"^TestShell$", []string{"$PATH", "p/**"}}, // exec.LookPath reads PATH
		"plain":  {"^TestPlain$", []string{"p/testdata/plain.txt"}},
	} {
		t.Run(name, func(t *testing.T) {
			rc := childModule(t, childTests)
			res, err := runPackages(context.Background(), rc, []string{"p"}, false, c.run, nil)
			if err != nil {
				t.Fatal(err)
			}
			got := nonGo(t, res.Observations["p"], rc.Stdout.(interface{ String() string }).String())
			// The parent's own TestHelperProcess looks up BE_HELPER too.
			if !slices.Equal(got, c.want) {
				t.Errorf("deps %v, want %v", got, c.want)
			}
		})
	}
}

// Function recording: the same, per function.
func TestRecordFuncChildren(t *testing.T) {
	rc := childModule(t, childTests)
	res, err := recordFuncs(context.Background(), rc, []string{"p"}, map[string][]string{"p": {"TestExiter", "TestReturner", "TestShell", "TestPlain"}})
	if err != nil {
		t.Fatal(err)
	}
	out := rc.Stdout.(interface{ String() string }).String()
	for unit, want := range map[string][]string{
		"p#TestExiter":   {"./**"},
		"p#TestReturner": {"$BE_HELPER", "shared/return.txt"},
		"p#TestShell":    {"$PATH", "p/**"},
		"p#TestPlain":    {"p/testdata/plain.txt"},
	} {
		if got := nonGo(t, res.Observations[unit], out); !slices.Equal(got, want) {
			t.Errorf("%s: deps %v, want %v", unit, got, want)
		}
	}
}

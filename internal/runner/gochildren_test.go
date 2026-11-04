package runner

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/dotneet/smates/internal/depkey"
	"github.com/dotneet/smates/internal/state"
	"github.com/dotneet/smates/internal/testutil"
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
		if !strings.Contains(f, ".go") && f != "p/" && f != "go.mod" && depkey.KindOf(f) != depkey.KindShape && depkey.KindOf(f) != depkey.KindMethods {
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

// A repository reached through a symlink (/tmp on macOS): go trusts an inherited $PWD,
// so overlay paths (smates' and the user's, made absolute against the resolved root)
// would match nothing; smates sets PWD for the commands it runs. The tree of the
// module, reported through the symlink or not, is the repository's.
func TestRecordPackageChildrenSymlinkedRepo(t *testing.T) {
	rc := childModule(t, childTests)
	for rel, content := range map[string]string{
		"user/u.go":       "package user\n",
		"user/u_test.go":  "package user\n\nimport \"testing\"\n\nfunc TestU(t *testing.T) {\n\tif V != 2 {\n\t\tt.Fatal(V)\n\t}\n}\n",
		"user/v.go.txt":   "package user\n\nconst V = 2\n",
		"user/ov.json":    `{"Replace": {"user/v.go": "user/v.go.txt"}}`,
		"user/testdata/x": "",
	} {
		testutil.Write(t, rc.Repo.Root, rel, content)
	}
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(rc.Repo.Root, link); err != nil {
		t.Skip(err)
	}
	t.Setenv("PWD", link)
	t.Setenv("GOFLAGS", "-overlay=user/ov.json")
	res, err := runPackages(context.Background(), rc, []string{"p", "user"}, false, "^(TestHelperProcess|TestExiter|TestU)$", nil)
	if err != nil {
		t.Fatal(err)
	}
	out := rc.Stdout.(interface{ String() string }).String()
	if o := res.Observations["user"]; o == nil || o.Status != state.Pass {
		t.Fatalf("the user's overlay was not applied: %+v\n%s", o, out)
	}
	if got := nonGo(t, res.Observations["p"], out); !slices.Equal(got, []string{"$BE_HELPER", "./**"}) {
		t.Errorf("p: deps %v, want [$BE_HELPER ./**]", got)
	}
	// Function recording builds its binary with the same overlays.
	fres, err := recordFuncs(context.Background(), rc, []string{"p", "user"}, map[string][]string{"p": {"TestExiter"}, "user": {"TestU"}})
	if err != nil {
		t.Fatal(err)
	}
	out = rc.Stdout.(interface{ String() string }).String()
	if got := nonGo(t, fres.Observations["p#TestExiter"], out); !slices.Equal(got, []string{"./**"}) {
		t.Errorf("p#TestExiter: deps %v, want [./**]", got)
	}
	if o := fres.Observations["user#TestU"]; o == nil || o.Status != state.Pass || strings.Contains(out, "recorded at package granularity") {
		t.Errorf("user#TestU: %+v\n%s", o, out)
	}
	// Reported through the symlink (a go command run with the user's PWD).
	o := &Observation{Files: map[string]bool{}}
	addTestLog(o, rc.Repo.Root, []string{filepath.Join(link, "**"), filepath.Join(link, "p", "**")}, nil)
	if !o.Files["./**"] {
		t.Errorf("module tree through a symlink: %v", o.Files)
	}
}

// Processes of the binary that do not run as test binaries (a TestMain dispatching to
// the program's own command line) and a test reading its own binary depend on the tree
// of the directory they ran in, not on the module's; a package whose test files the
// suite's build tags exclude still has none (go test runs no binary for it).
func TestRecordPackageChildrenNotTests(t *testing.T) {
	rc := childModule(t, `package p

import (
	"debug/buildinfo"
	"os"
	"os/exec"
	"testing"
)

func TestMain(m *testing.M) {
	if os.Getenv("BE_CLI") == "1" {
		os.ReadFile("../shared/exit.txt")
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func TestCLI(t *testing.T) {
	cmd := exec.Command(os.Args[0], "serve", "-v")
	cmd.Env = append(os.Environ(), "BE_CLI=1")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("cli: %v\n%s", err, out)
	}
}

func TestSelf(t *testing.T) {
	if _, err := buildinfo.ReadFile(os.Args[0]); err != nil {
		t.Fatal(err)
	}
}
`)
	testutil.Write(t, rc.Repo.Root, "tagged/a.go", "package tagged\n")
	testutil.Write(t, rc.Repo.Root, "tagged/a_test.go", "//go:build !integration\n\npackage tagged\n\nimport \"testing\"\n\nfunc TestT(t *testing.T) {}\n")
	t.Setenv("GOFLAGS", "-tags=integration")
	for _, run := range []string{"^TestCLI$", "^TestSelf$"} {
		res, err := runPackages(context.Background(), rc, []string{"p", "tagged"}, false, run, nil)
		if err != nil {
			t.Fatal(err)
		}
		out := rc.Stdout.(interface{ String() string }).String()
		if got := nonGo(t, res.Observations["p"], out); !slices.Equal(got, []string{"$BE_CLI", "p/**"}) && !slices.Equal(got, []string{"p/**"}) {
			t.Errorf("%s: deps %v, want p/** (and no ./**)", run, got)
		}
		if o := res.Observations["tagged"]; o != nil {
			t.Errorf("%s: tagged ran a test binary: %+v\n%s", run, o, out)
		}
	}
}

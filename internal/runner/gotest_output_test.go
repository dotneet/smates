package runner

import (
	"bytes"
	"context"
	"io"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/dotneet/smates/internal/config"
	"github.com/dotneet/smates/internal/state"
	"github.com/dotneet/smates/internal/testutil"
)

func TestGoFailureOutput(t *testing.T) {
	b := &buildFailures{}
	out := "--- FAIL: TestAdd (0.00s)\n    add_test.go:9: got 3, want 4\nFAIL\nFAIL\texample.com/m/calc\t0.012s\n" +
		"ok  \texample.com/m/util\t0.003s\n" +
		"# example.com/m/broken\nbroken/b.go:3:2: undefined: x\nFAIL\texample.com/m/broken [build failed]\n" +
		"?   \texample.com/m/empty\t[no test files]\nFAIL\n"
	// Written in odd chunks, as a pipe delivers it.
	for i := 0; i < len(out); i += 7 {
		io.WriteString(b, out[i:min(i+7, len(out))])
	}
	if got := b.output["example.com/m/calc"]; !strings.Contains(got, "add_test.go:9: got 3, want 4") || strings.Contains(got, "util") {
		t.Errorf("calc output %q", got)
	}
	if got := b.output["example.com/m/broken"]; !strings.Contains(got, "undefined: x") || strings.Contains(got, "add_test") {
		t.Errorf("broken output %q", got)
	}
	if _, ok := b.output["example.com/m/util"]; ok || !b.paths["example.com/m/broken"] {
		t.Errorf("output %v paths %v", b.output, b.paths)
	}
}

// A suite below the module root still maps its packages to import paths, so build
// failures are attributed.
func TestFindModuleImportPath(t *testing.T) {
	repo := tempRepo(t, "svc/api/x.go", "svc/api/handlers/h.go", "other/y.go")
	os.WriteFile(filepath.Join(repo.Root, "svc", "go.mod"), []byte("module example.com/svc\n\ngo 1.22\n"), 0o644)
	modDir, mod := findModule(repo.Root, filepath.Join(repo.Root, "svc", "api"))
	if modDir != filepath.Join(repo.Root, "svc") || mod != "example.com/svc" {
		t.Fatalf("findModule = %q %q", modDir, mod)
	}
	for pkg, want := range map[string]string{
		"svc":              "example.com/svc",
		"svc/api":          "example.com/svc/api",
		"svc/api/handlers": "example.com/svc/api/handlers",
		"other":            "",
	} {
		if got := importPath(modDir, mod, filepath.Join(repo.Root, filepath.FromSlash(pkg))); got != want {
			t.Errorf("importPath(%s) = %q, want %q", pkg, got, want)
		}
	}
	if d, m := findModule(repo.Root, filepath.Join(repo.Root, "other")); d != "" || m != "" {
		t.Errorf("no go.mod: %q %q", d, m)
	}
}

// A nested module is not part of the outer module's suite (go test cannot build its
// packages there) unless the go.work workspace has it; then its packages run, and
// their outcomes are found under their own import paths.
func TestGoNestedModule(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go not on PATH")
	}
	t.Setenv("GOWORK", "")
	repo := tempRepo(t)
	write := func(rel, content string) { testutil.Write(t, repo.Root, rel, content) }
	write("go.mod", "module example.com/m\n\ngo 1.22\n")
	write("a/a_test.go", "package a\n\nimport \"testing\"\n\nfunc TestA(t *testing.T) {}\n")
	write("tools/go.mod", "module example.com/tools\n\ngo 1.22\n")
	write("tools/x/x_test.go", "package x\n\nimport \"testing\"\n\nfunc TestX(t *testing.T) {}\n")
	write("tools/.hidden/go.mod", "module example.com/hidden\n")
	files := []string{"go.mod", "a/a_test.go", "tools/go.mod", "tools/x/x_test.go"}
	run := func(units []string, all bool) *RunResult {
		t.Helper()
		var out, errb bytes.Buffer
		suites, err := Resolve(repo, []config.Suite{{Name: "go", Root: ".", Runner: "go", Env: map[string]string{"GOFLAGS": ""}}})
		if err != nil {
			t.Fatal(err)
		}
		rc := &RunContext{Repo: repo, Suite: suites[0], Units: units, All: all, Fresh: true, TraceDir: t.TempDir(), Stdout: &out, Stderr: &errb}
		res, err := goTest{}.Run(context.Background(), rc)
		if err != nil {
			t.Fatalf("run: %v\n%s%s", err, &out, &errb)
		}
		if res.ExitCode != 0 {
			t.Errorf("units %v all=%v: exit %d\n%s%s", units, all, res.ExitCode, &out, &errb)
		}
		return res
	}
	units := func(env map[string]string) []string {
		suites, err := Resolve(repo, []config.Suite{{Name: "go", Root: ".", Runner: "go", Env: env}})
		if err != nil {
			t.Fatal(err)
		}
		return suites[0].Adapter.Units(suites[0], suites[0].TestFiles(files))
	}
	if got := units(nil); !slices.Equal(got, []string{"a"}) {
		t.Errorf("units without a workspace: %v", got)
	}
	if res := run([]string{"a"}, false); res.Observations["a"] == nil {
		t.Errorf("observations %v", res.Observations)
	}

	write("go.work", "go 1.22\n\nuse (\n\t.\n\t./tools // the tools\n)\n")
	if got := units(nil); !slices.Equal(got, []string{"a", "tools/x"}) {
		t.Fatalf("units in a workspace: %v", got)
	}
	for _, all := range []bool{false, true} {
		res := run([]string{"a", "tools/x"}, all)
		for _, u := range []string{"a", "tools/x"} {
			if o := res.Observations[u]; o == nil || o.Status != state.Pass {
				t.Errorf("all=%v %s: %+v", all, u, o)
			}
		}
	}
	// GOWORK=off in the suite's env: the workspace does not apply, as when it runs.
	if got := units(map[string]string{"GOWORK": "off"}); !slices.Equal(got, []string{"a"}) {
		t.Errorf("units with GOWORK=off: %v", got)
	}
}

// git lists the go.mod files (untracked ones too, not ignored or deleted ones); what
// go ignores is left out.
func TestGoModDirs(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	repo := tempRepo(t)
	for _, f := range []string{"go.mod", "tools/go.mod", "tools/sub/go.mod", "build/go.mod", "gone/go.mod", "vendor/v/go.mod", "x/testdata/go.mod", "_old/go.mod", "node_modules/m/go.mod"} {
		testutil.Write(t, repo.Root, f, "module m\n")
	}
	testutil.Write(t, repo.Root, ".gitignore", "/build/\n")
	for _, args := range [][]string{{"init", "-q"}, {"add", "gone/go.mod"}} {
		if _, err := repo.Git(args...); err != nil {
			t.Fatal(err)
		}
	}
	os.Remove(filepath.Join(repo.Root, "gone", "go.mod"))
	want := []string{filepath.Join(repo.Root, "tools"), filepath.Join(repo.Root, "tools", "sub")}
	if got := goModDirs(repo, repo.Root); !slices.Equal(got, want) {
		t.Errorf("goModDirs = %v, want %v", got, want)
	}
	if got := goModDirs(repo, filepath.Join(repo.Root, "tools")); !slices.Equal(got, want[1:]) {
		t.Errorf("goModDirs(tools) = %v", got)
	}
	if other, _ := nestedGoModules(repo, repo.Root, "off"); !slices.Equal(other, []string{"tools"}) {
		t.Errorf("nested %v", other)
	}
}

func TestGoWorkModules(t *testing.T) {
	root := t.TempDir()
	testutil.Write(t, root, "go.work", "go 1.22\n\nuse ./a\nuse \"b/c\" // comment\nuse (\n\t.\n\t// ./skipped\n\t../outside\n)\nreplace x => ./y\n")
	got := goWorkModules(root, filepath.Join(root, "sub"), "")
	want := map[string]bool{filepath.Join(root, "a"): true, filepath.Join(root, "b", "c"): true, root: true, filepath.Join(filepath.Dir(root), "outside"): true}
	if !maps.Equal(got, want) {
		t.Errorf("goWorkModules = %v, want %v", got, want)
	}
	if got := goWorkModules(root, root, "off"); got != nil {
		t.Errorf("GOWORK=off: %v", got)
	}
}

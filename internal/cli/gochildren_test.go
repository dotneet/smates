package cli

import (
	"runtime"
	"slices"
	"testing"
)

// What children of Go tests read counts, at either granularity: a re-execution of the
// test binary that exits through os.Exit (the classic TestHelperProcess, whose test log
// is lost: the test depends on its module's tree) and a shell (the tree of the directory
// it runs in). Editing what they read selects the test, which fails; restoring it and
// running settles the state (no perpetual reruns).
func TestGoChildrenSelection(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("runs sh")
	}
	for _, gran := range []string{"package", "func"} {
		t.Run(gran, func(t *testing.T) {
			r := newGoFuncRepo(t, "go test")
			unit := func(name string) string { return "tool#" + name }
			if gran == "package" {
				r.write("smates.yml", "version: 1\norphans: ignore\nsuites:\n  - name: go\n    runner: go\n")
				unit = func(string) string { return "tool" }
			}
			r.write("testdata/version.txt", "v1\n")
			r.write("tool/tool.go", "package tool\n")
			r.write("tool/testdata/sh.txt", "sh\n")
			r.write("tool/tool_test.go", `package tool

import (
	"fmt"
	"os"
	"os/exec"
	"testing"
)

func TestHelperProcess(t *testing.T) {
	if os.Getenv("GO_WANT_HELPER") != "1" {
		return
	}
	data, _ := os.ReadFile("../testdata/version.txt")
	fmt.Print(string(data))
	os.Exit(0)
}

func TestVersion(t *testing.T) {
	cmd := exec.Command(os.Args[0], "-test.run=^TestHelperProcess$")
	cmd.Env = append(os.Environ(), "GO_WANT_HELPER=1")
	out, err := cmd.Output()
	if err != nil || string(out) != "v1\n" {
		t.Fatalf("version %q %v", out, err)
	}
}

func TestShell(t *testing.T) {
	if out, err := exec.Command("sh", "-c", "grep -q sh testdata/sh.txt").CombinedOutput(); err != nil {
		t.Fatalf("sh: %v\n%s", err, out)
	}
}
`)
			r.g.git("add", "-A")
			r.g.commit("children")
			if code, _, out := r.runReport("--all"); code != 0 {
				t.Fatalf("baseline: %d\n%s", code, out)
			}
			if got := r.selected(); len(got) != 0 {
				t.Fatalf("after recording: selected %v", got)
			}
			for _, c := range []struct {
				file, edit, orig string
				want             []string
			}{
				{"testdata/version.txt", "v2\n", "v1\n", []string{unit("TestVersion")}},
				// TestVersion depends on the whole module, tool/ included.
				{"tool/testdata/sh.txt", "nope\n", "sh\n", slices.Compact([]string{unit("TestShell"), unit("TestVersion")})},
			} {
				r.write(c.file, c.edit)
				// The test file names ../testdata/version.txt: the reference scan selects
				// its other functions too, which does not count here.
				sel := r.selected()
				for _, u := range c.want {
					if sel[u] == "" || sel[u] == "reference" {
						t.Errorf("%s edited: %s not selected by its dependencies: %v", c.file, u, sel)
					}
				}
				for u, why := range sel {
					if !slices.Contains(c.want, u) && why != "reference" {
						t.Errorf("%s edited: %s selected (%s): %v", c.file, u, why, sel)
					}
				}
				if code, res, out := r.runReport(); code != 1 {
					t.Errorf("%s edited: run %d %v\n%s", c.file, code, res, out)
				}
				r.write(c.file, c.orig)
				if code, res, out := r.runReport(); code != 0 {
					t.Errorf("%s restored: run %d %v\n%s", c.file, code, res, out)
				}
				if got := r.selected(); len(got) != 0 {
					t.Errorf("%s restored and run: selected %v", c.file, got)
				}
			}
		})
	}
}

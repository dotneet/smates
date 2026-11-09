package runner

import (
	"context"
	"os/exec"
	"runtime"
	"strings"
	"testing"

	"github.com/dotneet/smates/internal/testutil"
	"github.com/dotneet/smates/internal/trace"
)

// langChildTests start an interpreter that loads a module of the repository, directly or
// through a shell, with the test's own environment (cmd.Env nil) or a copy of it.
const langChildTests = `package p

import (
	"os"
	"os/exec"
	"testing"
)

func run(t *testing.T, cmd *exec.Cmd) {
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("child failed: %v\n%s", err, out)
	}
}

func TestPython(t *testing.T) { run(t, exec.Command("python3", "../tools/main.py")) }

func TestPythonModule(t *testing.T) {
	cmd := exec.Command("python3", "-m", "pytool")
	cmd.Dir = "../tools"
	run(t, cmd)
}

func TestPythonCopiedEnv(t *testing.T) {
	cmd := exec.Command("python3", "../tools/main.py")
	cmd.Env = append(os.Environ(), "EXTRA=1")
	run(t, cmd)
}

func TestPythonShell(t *testing.T) { run(t, exec.Command("sh", "-c", "python3 ../tools/main.py")) }

func TestNode(t *testing.T) { run(t, exec.Command("node", "../tools/main.cjs")) }

func TestNodeShell(t *testing.T) { run(t, exec.Command("sh", "-c", "node ../tools/main.cjs")) }

func TestBun(t *testing.T) { run(t, exec.Command("bun", "../tools/main.cjs")) }
`

// The Node, Bun and Python processes a Go test starts trace themselves, and what they
// load is the test's dependency, in package and in function recording alike.
func TestRecordGoInterpreterChildren(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("runs sh")
	}
	files := map[string]string{
		"tools/main.py":            "import os, sys\nsys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))\nimport pyhelper\n",
		"tools/pyhelper.py":        "VALUE = 1\n",
		"tools/pytool/__init__.py": "",
		"tools/pytool/__main__.py": "from pytool import helper\n",
		"tools/pytool/helper.py":   "VALUE = 2\n",
		"tools/main.cjs":           "require('./jshelper.cjs')\n",
		"tools/jshelper.cjs":       "module.exports = 1\n",
	}
	cases := []struct {
		test, tool, want string
	}{
		{"TestPython", "python3", "tools/pyhelper.py"},
		{"TestPythonModule", "python3", "tools/pytool/helper.py"},
		{"TestPythonCopiedEnv", "python3", "tools/pyhelper.py"},
		{"TestPythonShell", "python3", "tools/pyhelper.py"},
		{"TestNode", "node", "tools/jshelper.cjs"},
		{"TestNodeShell", "node", "tools/jshelper.cjs"},
		{"TestBun", "bun", "tools/jshelper.cjs"},
	}
	for _, mode := range []string{"package", "func"} {
		for _, c := range cases {
			t.Run(mode+"/"+c.test, func(t *testing.T) {
				if _, err := exec.LookPath(c.tool); err != nil {
					t.Skip(c.tool + " not on PATH")
				}
				rc := childModule(t, langChildTests)
				testutil.WriteTree(t, rc.Repo.Root, files)
				if _, err := trace.ExtractShims(rc.ShimDir); err != nil {
					t.Fatal(err)
				}
				var o *Observation
				if mode == "package" {
					res, err := runPackages(context.Background(), rc, []string{"p"}, false, "^"+c.test+"$", nil)
					if err != nil {
						t.Fatal(err)
					}
					o = res.Observations["p"]
				} else {
					res, err := recordFuncs(context.Background(), rc, []string{"p"}, map[string][]string{"p": {c.test}})
					if err != nil {
						t.Fatal(err)
					}
					o = res.Observations["p#"+c.test]
				}
				out := rc.Stdout.(interface{ String() string }).String()
				deps := nonGo(t, o, out)
				found := false
				for _, d := range deps {
					found = found || d == c.want
				}
				if !found {
					t.Errorf("deps %v, want %s among them\n%s", deps, c.want, out)
				}
				// A shell is a program smates cannot see into (it depends on its directory's
				// tree); an interpreter started directly traces itself.
				for _, d := range deps {
					if strings.HasSuffix(d, "/**") && !strings.HasSuffix(c.test, "Shell") {
						t.Errorf("deps %v: a traced child must not fall back to a tree", deps)
					}
				}
			})
		}
	}
}

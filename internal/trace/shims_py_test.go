package trace

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"
)

// These tests run pytest (and plain scripts) under the Python shims. pytest comes from
// $SMATES_TEST_PYTHON (an interpreter that has it) or python3 on PATH; without it they
// are skipped.

func pytestPython(t *testing.T) string {
	t.Helper()
	py := os.Getenv("SMATES_TEST_PYTHON")
	if py == "" {
		var err error
		if py, err = exec.LookPath("python3"); err != nil {
			t.Skip("python3 not on PATH")
		}
	}
	if err := exec.Command(py, "-c", "import pytest").Run(); err != nil {
		t.Skipf("%s has no pytest (set SMATES_TEST_PYTHON to an interpreter that has it)", py)
	}
	return py
}

// testPython returns $SMATES_TEST_PYTHON or python3 from PATH.
func testPython(t *testing.T) string {
	t.Helper()
	if py := os.Getenv("SMATES_TEST_PYTHON"); py != "" {
		return py
	}
	py, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 not on PATH")
	}
	return py
}

// runPy runs argv in root with env added and returns its output and exit code.
func runPy(t *testing.T, root string, env []string, argv ...string) (string, int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir = root
	cmd.Env = append(os.Environ(), append(env, "PYTHONDONTWRITEBYTECODE=1")...)
	out, err := cmd.CombinedOutput()
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return string(out), ee.ExitCode()
	} else if err != nil {
		t.Fatalf("%v: %v\n%s", argv, err, out)
	}
	return string(out), 0
}

func unitOfPy(f string) string {
	if b := filepath.Base(f); strings.HasPrefix(b, "test_") && strings.HasSuffix(b, ".py") {
		return f
	}
	return ""
}

// tracePytest runs pytest with tracing in root and attributes what it recorded.
func tracePytest(t *testing.T, root string, args ...string) *Attribution {
	t.Helper()
	py := pytestPython(t)
	sh, traceDir := extractShims(t)
	argv := append([]string{py, "-m", "pytest", "-q", "-p", "no:cacheprovider"}, args...)
	out, code := runPy(t, root, PythonEnv(traceDir, root, sh, os.Environ()), argv...)
	if code != 0 {
		t.Fatalf("pytest exited %d:\n%s", code, out)
	}
	procs, err := Collect(traceDir, root)
	if err != nil {
		t.Fatal(err)
	}
	return Attribute(procs, unitOfPy, Options{})
}

func checkUnit(t *testing.T, a *Attribution, id string, want, notWant []string) {
	t.Helper()
	u := a.Units[id]
	if u == nil {
		var ids []string
		for id := range a.Units {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		t.Errorf("no unit %s (units: %v)", id, ids)
		return
	}
	checkDeps(t, u.Files, want, notWant)
}

// Without tracing (smates run --no-record) the plugin only writes results.
func TestPytestPluginResultsOnly(t *testing.T) {
	py := pytestPython(t)
	root := shimRepo(t, map[string]string{
		"test_a.py": "def test_a():\n    pass\n",
		"test_b.py": "def test_b():\n    assert False\n",
	})
	sh, traceDir := extractShims(t)
	out, code := runPy(t, root, PytestResultsEnv(traceDir, sh, os.Environ()), py, "-m", "pytest", "-q", "-p", "no:cacheprovider")
	if code != 1 || strings.Contains(out, "INTERNALERROR") {
		t.Fatalf("pytest exited %d, want 1 (one failed test):\n%s", code, out)
	}
	res := Results(traceDir, root)
	if res["test_a.py"].Status != "pass" || res["test_b.py"].Status != "fail" || len(res) != 2 {
		t.Errorf("results %+v", res)
	}
}

// A session a test runs in-process (pytester) is part of that test.
func TestPytestNestedSession(t *testing.T) {
	root := shimRepo(t, map[string]string{
		"test_plug.py": `import os

HERE = os.path.dirname(os.path.abspath(__file__))


def test_inner(pytester):
    pytester.makepyfile("def test_x():\n    assert 0\n")
    pytester.runpytest().assert_outcomes(failed=1)
    with open(os.path.join(HERE, "after.txt")) as f:
        f.read()
`,
		"test_other.py": "def test_o():\n    pass\n",
		"after.txt":     "",
	})
	a := tracePytest(t, root, "-p", "pytester")
	checkUnit(t, a, "test_plug.py", []string{"after.txt"}, nil)
	// The inner session's failing test_inner.py (in pytester's directory) is not a
	// result of the outer run.
	if u := a.Units["test_inner.py"]; u != nil {
		t.Errorf("unit of the nested session: %+v", u)
	}
	if u := a.Units["test_plug.py"]; u == nil || u.Status != "pass" {
		t.Errorf("test_plug.py: %+v", u)
	}
}

// A fixture one test declares and another requests dynamically is used by both.
func TestPytestDynamicFixture(t *testing.T) {
	root := shimRepo(t, map[string]string{
		"conftest.py": `import os

import pytest

HERE = os.path.dirname(os.path.abspath(__file__))


@pytest.fixture(scope="session")
def data():
    with open(os.path.join(HERE, "data.txt")) as f:
        return f.read()
`,
		"test_a.py": "def test_a(data):\n    assert data\n",
		"test_b.py": "def test_b(request):\n    assert request.getfixturevalue('data')\n",
		"test_c.py": "def test_c():\n    pass\n",
		"data.txt":  "",
	})
	a := tracePytest(t, root)
	checkUnit(t, a, "test_a.py", []string{"data.txt"}, nil)
	checkUnit(t, a, "test_b.py", []string{"data.txt"}, nil)
	checkUnit(t, a, "test_c.py", nil, []string{"data.txt"})
}

// A child a session fixture starts without waiting for it belongs to the fixture, not to
// the test running when it starts up.
func TestPytestFixtureChildContext(t *testing.T) {
	root := shimRepo(t, map[string]string{
		"conftest.py": `import os
import subprocess
import sys

import pytest

HERE = os.path.dirname(os.path.abspath(__file__))


@pytest.fixture(scope="session")
def server():
    p = subprocess.Popen([sys.executable, os.path.join(HERE, "server.py")])
    yield p
    p.wait()
`,
		"server.py": `import os

with open(os.path.join(os.path.dirname(os.path.abspath(__file__)), "served.txt")) as f:
    f.read()
`,
		"test_a.py":  "import time\n\n\ndef test_a(server):\n    time.sleep(0.5)  # the server starts up meanwhile\n",
		"test_b.py":  "def test_b(server):\n    pass\n",
		"served.txt": "",
	})
	a := tracePytest(t, root)
	for _, id := range []string{"test_a.py", "test_b.py"} {
		checkUnit(t, a, id, []string{"served.txt", "server.py"}, nil)
	}
}

// Workers a session fixture forks (multiprocessing's fork start method) record what
// they do for every test.
func TestPytestForkedWorkers(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no fork")
	}
	root := shimRepo(t, map[string]string{
		"conftest.py": `import multiprocessing

import pytest

import helper

helper.work(helper.path("warm.txt"))  # its code ran in the parent before the fork


@pytest.fixture(scope="session")
def pool():
    p = multiprocessing.get_context("fork").Pool(1)
    yield p
    p.close()
    p.join()
`,
		"helper.py": `import os


def path(name):
    return os.path.join(os.path.dirname(os.path.abspath(__file__)), name)


def work(p):
    with open(p) as f:
        return f.read()
`,
		"test_a.py":  "import helper\n\n\ndef test_a(pool):\n    pool.apply(helper.work, (helper.path('shared.txt'),))\n",
		"test_b.py":  "import helper\n\n\ndef test_b(pool):\n    pool.apply(helper.work, (helper.path('shared.txt'),))\n",
		"warm.txt":   "",
		"shared.txt": "",
	})
	py := pytestPython(t)
	sh, traceDir := extractShims(t)
	out, code := runPy(t, root, PythonEnv(traceDir, root, sh, os.Environ()), py, "-m", "pytest", "-q", "-p", "no:cacheprovider", "-W", "ignore::DeprecationWarning")
	if code != 0 {
		t.Fatalf("pytest exited %d:\n%s", code, out)
	}
	procs, err := Collect(traceDir, root)
	if err != nil {
		t.Fatal(err)
	}
	a := Attribute(procs, unitOfPy, Options{})
	for _, id := range []string{"test_a.py", "test_b.py"} {
		checkUnit(t, a, id, []string{"shared.txt"}, nil)
	}
	// The worker has a trace of its own, and code the parent had run is seen again.
	var worker *Proc
	for _, p := range procs {
		if p.Lang == "python" && !p.HasContexts() && p.Reads["shared.txt"] {
			worker = p
		}
	}
	if worker == nil || !worker.Code["helper.py"] {
		t.Errorf("no worker process with its own trace of helper.py and shared.txt: %+v", worker)
	}
}

// pytest-forked runs each test (its hooks included) in a forked child.
func TestPytestForkedTests(t *testing.T) {
	py := pytestPython(t)
	if runtime.GOOS == "windows" || exec.Command(py, "-c", "import pytest_forked").Run() != nil {
		t.Skip("no pytest-forked")
	}
	root := shimRepo(t, map[string]string{
		"conftest.py": `import os

import pytest

HERE = os.path.dirname(os.path.abspath(__file__))


@pytest.fixture
def data():
    with open(os.path.join(HERE, "data.txt")) as f:
        return f.read()
`,
		"test_a.py": "def test_a(data):\n    open('a.txt').read()\n",
		"test_b.py": "def test_b():\n    open('b.txt').read()\n",
		"data.txt":  "", "a.txt": "", "b.txt": "",
	})
	plain := shimRepo(t, map[string]string{"test_p.py": "def test_p():\n    pass\n"})
	if out, code := runPy(t, plain, nil, py, "-m", "pytest", "-q", "-p", "no:cacheprovider", "--forked"); code != 0 {
		t.Skipf("pytest-forked does not work with this pytest:\n%s", out)
	}
	a := tracePytest(t, root, "--forked")
	checkUnit(t, a, "test_a.py", []string{"a.txt", "data.txt"}, []string{"b.txt"})
	checkUnit(t, a, "test_b.py", []string{"b.txt"}, []string{"a.txt", "data.txt"})
}

func runPyTraced(t *testing.T, root string, files ...string) map[string]bool {
	t.Helper()
	py := testPython(t)
	sh, traceDir := extractShims(t)
	env := append(PythonEnv(traceDir, root, sh, os.Environ()), "PYTHONDONTWRITEBYTECODE=1")
	return runTraced(t, root, "python", env, traceDir, append([]string{py}, files...)...)
}

// Python children given an environment without the tracing variables are traced anyway.
func TestPythonShimChildWithOwnEnv(t *testing.T) {
	root := shimRepo(t, map[string]string{
		"main.py": `import os
import subprocess
import sys

env = {"PATH": os.environ.get("PATH", "")}
subprocess.run([sys.executable, "child.py", "c1.txt"], env=env, check=True)
if hasattr(os, "posix_spawn"):
    pid = os.posix_spawn(sys.executable, [sys.executable, "child.py", "c2.txt"], env)
    os.waitpid(pid, 0)
os.spawnve(os.P_WAIT, sys.executable, [sys.executable, "child.py", "c3.txt"], env)
`,
		"child.py": "import sys\n\nwith open(sys.argv[1]) as f:\n    f.read()\n",
		"c1.txt":   "", "c2.txt": "", "c3.txt": "",
	})
	want := []string{"c1.txt", "c3.txt"}
	if runtime.GOOS != "windows" {
		want = append(want, "c2.txt")
	}
	checkDeps(t, runPyTraced(t, root, "main.py"), want, nil)
}

// Listings of the repository root, children started by os.posix_spawn/os.spawn*/
// os.exec*, sqlite databases, and listings by a module whose name has "importlib".
func TestPythonShimRootSpawnSqlite(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX commands")
	}
	root := shimRepo(t, map[string]string{
		"main.py": `import glob
import os
import sqlite3

import my_importlib_compat

os.listdir(".")
glob.glob("*.md")
pid = os.posix_spawn("/bin/cat", ["cat", "p1.txt"], os.environ)
os.waitpid(pid, 0)
os.spawnv(os.P_WAIT, "/bin/cat", ["cat", "p2.txt"])
sqlite3.connect("fixture.db").close()
sqlite3.connect("file:uri.db?mode=ro", uri=True).close()
sqlite3.connect(":memory:").close()
sqlite3.connect("created.db").close()
os.execv("/bin/cat", ["cat", "p3.txt"])
`,
		"my_importlib_compat.py": "import os\n\nCASES = os.listdir('data5')\n",
		"README.md":              "",
		"p1.txt":                 "", "p2.txt": "", "p3.txt": "",
		"fixture.db": "", "uri.db": "",
		"data5/x": "",
	})
	reads := runPyTraced(t, root, "main.py")
	checkDeps(t, reads,
		[]string{"./", "data5/", "p1.txt", "p2.txt", "p3.txt", "fixture.db", "uri.db"},
		[]string{"created.db", ":memory:"})
}

// Replacing os.stat/lstat/access keeps the os.supports_* sets (and shutil's choice of
// the symlink-attack resistant rmtree) as they are without tracing.
func TestPythonShimSupportsSets(t *testing.T) {
	py := testPython(t)
	root := shimRepo(t, map[string]string{
		"main.py": `import os
import shutil

print(sorted(n for n in ("stat", "lstat", "access", "execve", "posix_spawn")
             if getattr(os, n, None) in os.supports_dir_fd | os.supports_fd | os.supports_follow_symlinks | os.supports_effective_ids))
print(shutil._use_fd_functions, shutil.rmtree.avoids_symlink_attacks)
`,
	})
	sh, traceDir := extractShims(t)
	plain, code := runPy(t, root, nil, py, "main.py")
	if code != 0 {
		t.Fatal(plain)
	}
	traced, code := runPy(t, root, PythonEnv(traceDir, root, sh, os.Environ()), py, "main.py")
	if code != 0 {
		t.Fatal(traced)
	}
	if traced != plain {
		t.Errorf("traced:\n%s\nwithout tracing:\n%s", traced, plain)
	}
	if m, _ := filepath.Glob(filepath.Join(traceDir, "proc-*.jsonl")); len(m) == 0 {
		t.Errorf("the traced run left no trace")
	}
}

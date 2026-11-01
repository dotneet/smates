package trace

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/dotneet/smates/internal/testutil"
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

// A pool the first test needing it forks and later tests reuse: the workers follow the
// contexts of the process that forked them.
func TestPytestLazyForkedPool(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no fork")
	}
	root := shimRepo(t, map[string]string{
		"conftest.py": `import pytest

import helper


@pytest.fixture(scope="session", autouse=True)
def close_pool():
    yield
    helper.close()
`,
		"helper.py": `import multiprocessing
import os

_pool = None


def path(name):
    return os.path.join(os.path.dirname(os.path.abspath(__file__)), name)


def work(name):
    with open(path(name)) as f:
        return f.read()


def run(name):
    global _pool
    if _pool is None:
        _pool = multiprocessing.get_context("fork").Pool(1)
    return _pool.apply(work, (name,))


def close():
    if _pool is not None:
        _pool.close()
        _pool.join()
`,
		"test_a.py": "import helper\n\n\ndef test_a():\n    helper.run('a.txt')\n    helper.run('shared.txt')\n",
		"test_b.py": "import helper\n\n\ndef test_b():\n    helper.run('b.txt')\n    helper.run('shared.txt')\n",
		"test_c.py": "def test_c():\n    pass\n",
		"a.txt":     "", "b.txt": "",
		"shared.txt": "",
	})
	a := tracePytest(t, root, "-W", "ignore::DeprecationWarning")
	checkUnit(t, a, "test_a.py", []string{"a.txt", "shared.txt"}, []string{"b.txt"})
	checkUnit(t, a, "test_b.py", []string{"b.txt", "shared.txt"}, []string{"a.txt"})
	checkUnit(t, a, "test_c.py", nil, []string{"a.txt", "b.txt", "shared.txt"})
}

// A pool forked when the conftest is imported, before any context: its workers follow
// the contexts of the tests that use it, and one that only runs code and is terminated
// reports that code still.
func TestPytestPoolForkedAtImport(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no fork")
	}
	root := shimRepo(t, map[string]string{
		"conftest.py": `import multiprocessing

import pytest

import calc  # imported (run) here: in test_c only the worker runs its code
import helper

POOL = multiprocessing.get_context("fork").Pool(1)
CODE_POOL = multiprocessing.get_context("fork").Pool(1)  # its worker only runs code


@pytest.fixture
def pool():
    return POOL


@pytest.fixture
def code_pool():
    return CODE_POOL


@pytest.fixture(scope="session", autouse=True)
def stop_pool():
    yield
    for p in (POOL, CODE_POOL):
        p.terminate()
        p.join()
`,
		"helper.py": `import os


def work(name):
    with open(os.path.join(os.path.dirname(os.path.abspath(__file__)), name)) as f:
        return f.read()
`,
		"calc.py":   "def double(x):\n    return 2 * x\n",
		"test_a.py": "import helper\n\n\ndef test_a(pool):\n    pool.apply(helper.work, ('a.txt',))\n",
		"test_b.py": "import helper\n\n\ndef test_b(pool):\n    pool.apply(helper.work, ('b.txt',))\n",
		"test_c.py": "def test_c(code_pool):\n    import calc\n\n    assert code_pool.apply(calc.double, (2,)) == 4\n",
		"a.txt":     "", "b.txt": "",
	})
	a := tracePytest(t, root, "-W", "ignore::DeprecationWarning")
	checkUnit(t, a, "test_a.py", []string{"a.txt"}, []string{"b.txt"})
	checkUnit(t, a, "test_b.py", []string{"b.txt"}, []string{"a.txt"})
	checkUnit(t, a, "test_c.py", []string{"calc.py"}, []string{"a.txt", "b.txt"})
}

// The shim chains to the next sitecustomize on sys.path: another copy of the shim (smates
// run under smates) is skipped, and one doing file and process work at import runs once.
func TestPythonShimChainedSitecustomize(t *testing.T) {
	py := testPython(t)
	root := shimRepo(t, map[string]string{
		"main.py":  "import pathlib\n\nprint(pathlib.Path('main.py').exists())\n",
		"data.txt": "",
	})
	sh, traceDir := extractShims(t)
	outer, err := ExtractShims(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	user := t.TempDir()
	testutil.Write(t, user, "sitecustomize.py", `import os
import pathlib
import subprocess
import sys

os.stat(".")
os.close(os.open(os.devnull, os.O_RDONLY))
pathlib.Path(".").exists()
subprocess.run([sys.executable, "-c", "pass"], env={"PATH": os.environ.get("PATH", "")}, check=True)
print("user sitecustomize")
`)
	base := append(os.Environ(), "PYTHONPATH="+outer.Python+string(os.PathListSeparator)+user)
	out, code := runPy(t, root, PythonEnv(traceDir, root, sh, base), py, "main.py")
	if code != 0 || out != "user sitecustomize\nTrue\n" {
		t.Errorf("exit %d, output:\n%s", code, out)
	}
	if m, _ := filepath.Glob(filepath.Join(traceDir, "proc-*.jsonl")); len(m) == 0 {
		t.Errorf("not traced")
	}
}

// A child forked to exec another program (preexec_fn) is that program: what Python ran
// in between (the preexec function of a test file) is no part of it.
func TestPytestPreexecChild(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no preexec_fn")
	}
	root := shimRepo(t, map[string]string{
		"test_a.py": `import subprocess
import sys


def test_a():
    r = subprocess.run([sys.executable, "-c", "import sys; sys.exit(3)"], preexec_fn=lambda: None)
    assert r.returncode == 3
`,
	})
	py := pytestPython(t)
	sh, traceDir := extractShims(t)
	out, code := runPy(t, root, PythonEnv(traceDir, root, sh, os.Environ()), py, "-m", "pytest", "-q", "-p", "no:cacheprovider")
	if code != 0 {
		t.Fatalf("pytest exited %d:\n%s", code, out)
	}
	procs, err := Collect(traceDir, root)
	if err != nil {
		t.Fatal(err)
	}
	if u := Attribute(procs, unitOfPy, Options{}).Units["test_a.py"]; u == nil || u.Status != "pass" {
		t.Errorf("test_a.py: %+v", u)
	}
	for _, p := range procs {
		if p.Anc == p.Key() || (!p.HasContexts() && p.Code["test_a.py"]) {
			t.Errorf("child %s: anc %s, code %v, argv %v", p.Key(), p.Anc, keys(p.Code), p.Argv)
		}
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
# subprocess stays patched when reloaded.
import importlib

importlib.reload(subprocess)
subprocess.run([sys.executable, "child.py", "c4.txt"], env=env, check=True)
`,
		"child.py": "import sys\n\nwith open(sys.argv[1]) as f:\n    f.read()\n",
		"c1.txt":   "", "c2.txt": "", "c3.txt": "", "c4.txt": "",
	})
	want := []string{"c1.txt", "c3.txt", "c4.txt"}
	if runtime.GOOS != "windows" {
		want = append(want, "c2.txt")
	}
	checkDeps(t, runPyTraced(t, root, "main.py"), want, []string{"$SMATES_TRACE_PARENT", "$SMATES_TRACE_DIR", "$PYTHONPATH"})
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
# shutil.rmtree opens and stats entries relative to a directory's fd.
import shutil
import tempfile

d = tempfile.mkdtemp()
os.makedirs(os.path.join(d, "x", "sub_in_tmp"))
open(os.path.join(d, "x", "sub_in_tmp", "f"), "w").close()
shutil.rmtree(d)
os.execv("/bin/cat", ["cat", "p3.txt"])
`,
		"my_importlib_compat.py": "import os\n\nCASES = os.listdir('data5')\n",
		"README.md":              "",
		"p1.txt":                 "", "p2.txt": "", "p3.txt": "",
		"fixture.db": "", "uri.db": "",
		"data5/x": "",
	})
	reads := runPyTraced(t, root, "main.py")
	want := []string{"./", "data5/", "p1.txt", "p2.txt", "p3.txt"}
	if pyAtLeast(t, testPython(t), 3, 10) { // the sqlite3.connect audit event
		want = append(want, "fixture.db", "uri.db")
	}
	checkDeps(t, reads, want,
		[]string{"created.db", ":memory:", "x", "x?", "sub_in_tmp", "sub_in_tmp?", "sub_in_tmp/", "f", "f?",
			// the shim's own lookups
			"$SMATES_TRACE_PARENT", "$SMATES_TRACE_CTX", "$SMATES_TRACE_DIR", "$PYTHONPATH"})
}

func pyAtLeast(t *testing.T, py string, major, minor int) bool {
	t.Helper()
	code := fmt.Sprintf("import sys; sys.exit(0 if sys.version_info >= (%d, %d) else 1)", major, minor)
	return exec.Command(py, "-c", code).Run() == nil
}

// Libraries scanning sys.path (pkg_resources, the importlib_metadata backport) do not
// make the repository root, on sys.path with `python -m pytest`, a listed directory;
// what they list for the program otherwise is.
func TestPythonShimSysPathScan(t *testing.T) {
	py := testPython(t)
	root := shimRepo(t, map[string]string{
		"main.py": `import os
import sys

sys.path.insert(0, os.getcwd())
import scanner

scanner.scan()
scanner.list_dir("data6")
`,
		"data6/x": "",
	})
	lib := filepath.Join(t.TempDir(), "lib", "site-packages")
	testutil.Write(t, lib, "scanner.py", `import os
import sys


def scan():
    for p in sys.path:
        if os.path.isdir(p or "."):
            os.listdir(p or ".")


def list_dir(d):
    return os.listdir(d)
`)
	sh, traceDir := extractShims(t)
	base := append(os.Environ(), "PYTHONPATH="+lib)
	env := append(PythonEnv(traceDir, root, sh, base), "PYTHONDONTWRITEBYTECODE=1")
	checkDeps(t, runTraced(t, root, "python", env, traceDir, py, "main.py"), []string{"data6/", "main.py"}, []string{"./"})
}

// Replacing os.stat/lstat/access keeps the os.supports_* sets (and shutil's choice of
// the symlink-attack resistant rmtree) as they are without tracing.
func TestPythonShimSupportsSets(t *testing.T) {
	py := testPython(t)
	root := shimRepo(t, map[string]string{
		"main.py": `import os
import shutil

print(sorted(n for n in ("stat", "lstat", "access", "open", "execve", "posix_spawn")
             if getattr(os, n, None) in os.supports_dir_fd | os.supports_fd | os.supports_follow_symlinks | os.supports_effective_ids))
print(shutil._use_fd_functions, shutil.rmtree.avoids_symlink_attacks)
# A class attribute that must not bind (pathlib before 3.11).
import pathlib

print(pathlib.Path("main.py").stat().st_size > 0, pathlib.Path("main.py").exists(), pathlib.Path("nope").is_file())
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

// tracePytestRun runs pytest with tracing in root, whatever its exit code, and returns
// the attribution and the status of each unit as smates records it (a results file is
// final, otherwise the unit's status from the process trace).
func tracePytestRun(t *testing.T, root string, args ...string) (*Attribution, map[string]string) {
	t.Helper()
	py := pytestPython(t)
	sh, traceDir := extractShims(t)
	argv := append([]string{py, "-m", "pytest", "-q", "-p", "no:cacheprovider"}, args...)
	out, code := runPy(t, root, PythonEnv(traceDir, root, sh, os.Environ()), argv...)
	t.Logf("pytest %v exited %d:\n%s", args, code, out)
	procs, err := Collect(traceDir, root)
	if err != nil {
		t.Fatal(err)
	}
	a := Attribute(procs, unitOfPy, Options{})
	status := map[string]string{}
	for id, u := range a.Units {
		status[id] = u.Status
	}
	for f, r := range Results(traceDir, root) {
		status[f] = r.Status
	}
	return a, status
}

func needPyModule(t *testing.T, py, module string) {
	t.Helper()
	if err := exec.Command(py, "-c", "import "+module).Run(); err != nil {
		t.Skipf("%s has no %s", py, module)
	}
}

// The forkserver (the default start method on Linux from Python 3.14) starts during the
// first test using it and forks the workers of later tests: they belong to the test
// running when they start, not to the one that started the forkserver.
func TestPytestForkserverPool(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no fork")
	}
	root := shimRepo(t, map[string]string{
		"helper.py": `import multiprocessing
import os
import subprocess
import sys


def path(name):
    return os.path.join(os.path.dirname(os.path.abspath(__file__)), name)


def work(name):
    with open(path(name)) as f:
        f.read()
    # A child of the worker belongs to the same test.
    subprocess.run([sys.executable, "-c", "open(%r).read()" % path(name + ".child")], check=True)


def run(name):
    with multiprocessing.get_context("forkserver").Pool(1) as p:
        return p.apply(work, (name,))
`,
		"test_a.py": "import helper\n\n\ndef test_a():\n    helper.run('a.txt')\n",
		"test_b.py": "import helper\n\n\ndef test_b():\n    helper.run('b.txt')\n",
		"a.txt":     "", "b.txt": "", "a.txt.child": "", "b.txt.child": "",
	})
	a := tracePytest(t, root, "-W", "ignore::DeprecationWarning")
	checkUnit(t, a, "test_a.py", []string{"a.txt", "a.txt.child"}, []string{"b.txt", "b.txt.child"})
	checkUnit(t, a, "test_b.py", []string{"b.txt", "b.txt.child"}, []string{"a.txt", "a.txt.child"})
}

// A pytest session a test runs in a child process is part of that test, also when it
// runs test files of the suite.
func TestPytestSubprocessSession(t *testing.T) {
	root := shimRepo(t, map[string]string{
		"helper.py": "import os\n\n\ndef load(name):\n    with open(os.path.join(os.path.dirname(os.path.abspath(__file__)), name)) as f:\n        return f.read()\n",
		"data.txt":  "", "suite.txt": "",
		"test_outer.py": `import os
import subprocess
import sys
import tempfile

HERE = os.path.dirname(os.path.abspath(__file__))


def test_outer():
    d = tempfile.mkdtemp()
    with open(os.path.join(d, "test_inner.py"), "w") as f:
        f.write("import sys\nsys.path.insert(0, %r)\nimport helper\n\n\ndef test_inner():\n    helper.load('data.txt')\n" % HERE)
    r = subprocess.run([sys.executable, "-m", "pytest", "-q", "-p", "no:cacheprovider", d], capture_output=True, text=True)
    assert r.returncode == 0, r.stdout + r.stderr
`,
		"test_meta.py": `import os
import subprocess
import sys

HERE = os.path.dirname(os.path.abspath(__file__))


def test_meta():
    r = subprocess.run([sys.executable, "-m", "pytest", "-q", "-p", "no:cacheprovider", os.path.join(HERE, "sub", "test_sub.py")], capture_output=True, text=True)
    assert r.returncode == 1, r.stdout + r.stderr
`,
		"sub/test_sub.py": "import helper\n\n\ndef test_sub():\n    helper.load('suite.txt')\n    assert False\n",
	})
	a, status := tracePytestRun(t, root, "test_outer.py", "test_meta.py")
	checkUnit(t, a, "test_outer.py", []string{"data.txt", "helper.py"}, []string{"suite.txt"})
	checkUnit(t, a, "test_meta.py", []string{"suite.txt", "helper.py", "sub/test_sub.py"}, []string{"data.txt"})
	if status["test_outer.py"] != "pass" || status["test_meta.py"] != "pass" {
		t.Errorf("statuses %v", status)
	}
	// The inner session's failing test is not a result of the outer run.
	if s, ok := status["sub/test_sub.py"]; ok {
		t.Errorf("sub/test_sub.py: %q", s)
	}
}

// smates started from a test an outer smates traces (a project test running `smates
// run`) records a run of its own: the outer context it inherits does not make its
// pytest session a nested one.
func TestPytestInsideOuterRecording(t *testing.T) {
	py := pytestPython(t)
	root := shimRepo(t, map[string]string{
		"test_a.py": "import subprocess\nimport sys\n\n\ndef test_a():\n    subprocess.run([sys.executable, '-c', \"open('a.txt').read()\"], check=True)\n",
		"test_b.py": "def test_b():\n    assert False\n",
		"a.txt":     "",
	})
	sh, traceDir := extractShims(t)
	outer := []string{"SMATES_TRACE_CTX=4242-0:test:/outer/tests/test_runs_smates.py", "SMATES_TRACE_PARENT=4242-0"}
	env := append(outer, PythonEnv(traceDir, root, sh, append(os.Environ(), outer...))...)
	out, code := runPy(t, root, env, py, "-m", "pytest", "-q", "-p", "no:cacheprovider")
	if code != 1 {
		t.Fatalf("pytest exited %d:\n%s", code, out)
	}
	procs, err := Collect(traceDir, root)
	if err != nil {
		t.Fatal(err)
	}
	a := Attribute(procs, unitOfPy, Options{})
	checkUnit(t, a, "test_a.py", []string{"a.txt"}, nil)
	if a.Units["test_a.py"].Status != "pass" || a.Units["test_b.py"] == nil || a.Units["test_b.py"].Status != "fail" {
		t.Errorf("units %+v %+v", a.Units["test_a.py"], a.Units["test_b.py"])
	}
}

// A file not all of whose tests ran (-x, --maxfail, pytest.exit()) did not pass.
func TestPytestStoppedFile(t *testing.T) {
	root := shimRepo(t, map[string]string{
		"test_a.py": "def test_1():\n    pass\n\n\ndef test_2():\n    assert False\n\n\ndef test_3():\n    pass\n",
		"test_b.py": "def test_1():\n    pass\n\n\ndef test_2():\n    pass\n",
		"test_c.py": "import pytest\n\n\ndef test_1():\n    pytest.exit('stop')\n\n\ndef test_2():\n    assert False\n",
	})
	_, status := tracePytestRun(t, root, "-x", "test_b.py", "test_a.py")
	if status["test_a.py"] != "fail" || status["test_b.py"] != "pass" {
		t.Errorf("-x: statuses %v", status)
	}
	_, status = tracePytestRun(t, root, "test_c.py")
	if status["test_c.py"] != "unknown" {
		t.Errorf("pytest.exit(): statuses %v", status)
	}
}

// With pytest-xdist and -x, a file whose failing test never ran on its worker is not a
// pass; a complete run passes the files that passed.
func TestPytestXdistExitFirst(t *testing.T) {
	needPyModule(t, pytestPython(t), "xdist")
	var b strings.Builder
	for i := 0; i < 40; i++ {
		fmt.Fprintf(&b, "def test_%d():\n    import time\n    time.sleep(0.05)\n\n\n", i)
	}
	passing := b.String()
	root := shimRepo(t, map[string]string{
		"test_a.py": "def test_fail():\n    assert False\n",
		"test_b.py": passing + "def test_zz():\n    assert False, 'never ran'\n",
	})
	_, status := tracePytestRun(t, root, "-n", "2", "-x")
	if status["test_a.py"] != "fail" || status["test_b.py"] == "pass" {
		t.Errorf("-x: statuses %v", status)
	}
	root = shimRepo(t, map[string]string{"test_a.py": "def test_a():\n    pass\n", "test_b.py": passing})
	_, status = tracePytestRun(t, root, "-n", "2")
	if status["test_a.py"] != "pass" || status["test_b.py"] != "pass" {
		t.Errorf("complete run: statuses %v", status)
	}
}

// Test files outside the rootdir (-c cfg/pytest.ini, --rootdir) have node ids relative
// to the arguments, if anything: results are keyed by the files themselves.
func TestPytestRootdirOutside(t *testing.T) {
	root := shimRepo(t, map[string]string{
		"cfg/pytest.ini":  "[pytest]\n",
		"tests/test_a.py": "def test_ok():\n    pass\n\n\ndef test_bad():\n    assert False\n",
		"tests/test_b.py": "def test_ok():\n    pass\n",
		"other/test_c.py": "def test_ok():\n    pass\n\n\ndef test_bad():\n    assert False\n",
		"other/test_d.py": "import x_missing_module\n",
	})
	want := map[string]string{"tests/test_a.py": "fail", "tests/test_b.py": "pass", "other/test_c.py": "fail", "other/test_d.py": "fail"}
	for _, args := range [][]string{
		{"-c", "cfg/pytest.ini", "tests", "other/test_c.py", "other/test_d.py"},
		{"--rootdir=cfg", "tests", "other/test_c.py", "other/test_d.py"},
	} {
		_, status := tracePytestRun(t, root, append(args, "--continue-on-collection-errors")...)
		for f, s := range want {
			if status[f] != s {
				t.Errorf("%v: %s: %q, want %q (statuses %v)", args, f, status[f], s, status)
			}
		}
	}
}

// A command line too long to keep (a test provoking E2BIG) is cut short in the trace:
// the command may read anything below its working directory.
func TestPythonShimHugeExec(t *testing.T) {
	root := shimRepo(t, map[string]string{
		"test_a.py": "import subprocess\n\nimport pytest\n\n\ndef test_e2big():\n    with pytest.raises(OSError):\n        subprocess.run(['cat', 'x' * 17_000_000])\n",
	})
	a := tracePytest(t, root)
	checkUnit(t, a, "test_a.py", []string{"./**"}, nil)
}

// Replaced os functions behave like the builtins: they do not bind as methods (from
// Python 3.14 functools.partial does, and 3.13 warns about it), and they pickle (sent
// to pool workers) and copy.
func TestPythonShimOsFunctions(t *testing.T) {
	py := testPython(t)
	root := shimRepo(t, map[string]string{
		"main.py": `import copy
import multiprocessing
import os
import pickle
import warnings

warnings.simplefilter("error")


class FS:
    stat = os.stat
    lstat = os.lstat
    access = os.access
    open = os.open


def main():
    fs = FS()
    print(fs.stat(__file__).st_size > 0, fs.lstat(__file__).st_size > 0, fs.access(__file__, os.R_OK))
    os.close(fs.open(__file__, os.O_RDONLY))
    for name in ("stat", "lstat", "access", "open", "execve", "posix_spawn", "posix_spawnp", "spawnve", "_exit"):
        f = getattr(os, name, None)
        if f is not None:
            assert pickle.loads(pickle.dumps(f)) is f, name
            assert copy.deepcopy(f) is f, name
    with multiprocessing.get_context("spawn").Pool(1) as p:
        print([s.st_size > 0 for s in p.map(os.stat, [__file__])])


if __name__ == "__main__":
    main()
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
}

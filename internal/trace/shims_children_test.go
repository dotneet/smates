package trace

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// A Node, Bun or Python child traces itself only when its environment carries its
// runtime's shim: exec events say which runtimes the child's environment traces, and
// a child of an untraced runtime is an opaque command (its directory's tree and the
// files it names).
func TestShimsUntracedRuntimeChildren(t *testing.T) {
	py, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 not on PATH")
	}
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not on PATH")
	}
	root := shimRepo(t, map[string]string{
		"child.py":   "open('py.txt').read()\n",
		"child.cjs":  "require('fs').readFileSync('js.txt');\n",
		"py.txt":     "",
		"js.txt":     "",
		"parent.cjs": "require('child_process').execFileSync(process.env.PY, ['child.py']);\n",
		"parent.py":  "import os, subprocess\nsubprocess.run([os.environ['NODE'], 'child.cjs'], check=True)\nsubprocess.run([os.environ['NODE'], 'child.cjs'], env={'PATH': os.environ['PATH']}, check=True)\n",
	})
	// Node traced, Python not: the Python child is opaque.
	sh, traceDir := extractShims(t)
	env := append(NodeEnv(traceDir, root, sh, os.Environ()), "PY="+py, "PYTHONDONTWRITEBYTECODE=1", "PYTHONPATH=")
	procs := runCollect(t, root, traceDir, env, node, "parent.cjs")
	checkDeps(t, readsOf(procs, "node"), []string{"./**", "child.py"}, nil)
	if readsOf(procs, "python")["py.txt"] {
		t.Error("the Python child was traced without the Python shim")
	}
	// Both traced: the child traces itself.
	sh, traceDir = extractShims(t)
	env = append(Env(traceDir, root, sh, os.Environ()), "PY="+py, "PYTHONDONTWRITEBYTECODE=1")
	procs = runCollect(t, root, traceDir, env, node, "parent.cjs")
	checkDeps(t, readsOf(procs, "node"), nil, []string{"./**"})
	checkDeps(t, readsOf(procs, "python"), []string{"py.txt"}, nil)

	// Python traced, Node not: the Node children (one with an environment of its own)
	// are opaque.
	sh, traceDir = extractShims(t)
	env = append(PythonEnv(traceDir, root, sh, os.Environ()), "NODE="+node, "PYTHONDONTWRITEBYTECODE=1", "NODE_OPTIONS=", "BUN_OPTIONS=")
	procs = runCollect(t, root, traceDir, env, py, "parent.py")
	checkDeps(t, readsOf(procs, "python"), []string{"./**", "child.cjs"}, nil)
	// Both traced: they trace themselves, with an environment of their own too.
	sh, traceDir = extractShims(t)
	env = append(Env(traceDir, root, sh, os.Environ()), "NODE="+node, "PYTHONDONTWRITEBYTECODE=1")
	procs = runCollect(t, root, traceDir, env, py, "parent.py")
	checkDeps(t, readsOf(procs, "python"), nil, []string{"./**"})
	checkDeps(t, readsOf(procs, "node"), []string{"js.txt"}, nil)
	n := 0
	for _, p := range procs {
		if p.Lang == "node" {
			n++
		}
	}
	if n != 2 {
		t.Errorf("%d Node children traced, want 2", n)
	}
}

// Env traces every runtime: what NodeEnv and PythonEnv set, each variable once, but the
// pytest plugin (PytestEnv adds it).
func TestEnvTracesAllRuntimes(t *testing.T) {
	sh := &Shims{Dir: "/s", Node: "/s/node_modules/smates/node.cjs", Python: "/s/py"}
	names := []string{"SMATES_TRACE_DIR", "SMATES_REPO_ROOT", "NODE_OPTIONS", "BUN_OPTIONS", "NODE_V8_COVERAGE", "PYTHONPATH", "SMATES_TRACE_PARENT", "SMATES_TRACE_CTX"}
	for _, c := range []struct {
		name    string
		env     []string
		plugins int
	}{
		{"Env", Env("/t", "/r", sh, []string{"PATH=/bin"}), 0},
		{"PytestEnv", PytestEnv("/t", "/r", sh, []string{"PATH=/bin"}), 1},
	} {
		seen := map[string]int{}
		for _, kv := range c.env {
			name, _, _ := strings.Cut(kv, "=")
			seen[name]++
		}
		for _, name := range names {
			if seen[name] != 1 {
				t.Errorf("%s: %s set %d times in %q", c.name, name, seen[name], c.env)
			}
		}
		if seen["PYTEST_PLUGINS"] != c.plugins {
			t.Errorf("%s: PYTEST_PLUGINS set %d times, want %d", c.name, seen["PYTEST_PLUGINS"], c.plugins)
		}
	}
}

// A pytest session a JavaScript test starts is traced like any Python child: what it
// reads belongs to the JavaScript test (with the plugin it would take itself for a
// top-level session and file the reads under its own tests).
func TestNodeTestStartsPytest(t *testing.T) {
	py := pytestPython(t)
	root := shimRepo(t, nil)
	files := map[string]string{
		"inner/check_inner.py": "def test_inner():\n    assert open('data/inner.txt').read() == 'x'\n",
		"data/inner.txt":       "x",
		"a.test.cjs":           "",
	}
	procs := traceNode(t, root, files, `
const path = require('path');
globalThis.__smatesTrace.context('test:' + path.resolve('a.test.cjs'));
require('child_process').execFileSync(process.env.PY, ['-m', 'pytest', '-q', '-p', 'no:cacheprovider', 'inner/check_inner.py'], { stdio: 'inherit' });
`, "PY="+py, "PYTHONDONTWRITEBYTECODE=1")
	a := Attribute(procs, func(f string) string {
		if strings.HasSuffix(f, ".test.cjs") {
			return f
		}
		return ""
	}, Options{})
	checkUnit(t, a, "a.test.cjs", []string{"data/inner.txt"}, nil)
}

// A Python child given an environment of its own gets the Node shim's preload in
// BUN_OPTIONS too (Bun ignores NODE_OPTIONS --require).
func TestPythonShimChildBunOptions(t *testing.T) {
	py := testPython(t)
	root := shimRepo(t, map[string]string{
		"main.py": `import os, subprocess, sys
out = subprocess.run([sys.executable, "-c", "import os, sys; sys.stdout.write(os.environ.get('BUN_OPTIONS', ''))"],
                     env={"PATH": os.environ["PATH"]}, capture_output=True, text=True, check=True).stdout
if "--preload=" not in out or "node.cjs" not in out:
    sys.exit("BUN_OPTIONS: %r" % out)
open("done.txt").read()
`,
		"done.txt": "",
	})
	sh, traceDir := extractShims(t)
	env := append(Env(traceDir, root, sh, os.Environ()), "PYTHONDONTWRITEBYTECODE=1")
	checkDeps(t, runTraced(t, root, "python", env, traceDir, py, "main.py"), []string{"done.txt"}, nil)
}

// pyfakefs patches the os functions it finds in modules (and the os module): the shim
// keeps writing its trace to the real file system while a fake one is active.
func TestPythonShimPyfakefs(t *testing.T) {
	py := testPython(t)
	needPyModule(t, py, "pyfakefs")
	root := shimRepo(t, map[string]string{
		"main.py": `import os
from pyfakefs.fake_filesystem_unittest import Patcher

here = os.path.dirname(os.path.abspath(__file__))
with Patcher() as p:
    p.fs.add_real_file(os.path.join(here, "real.txt"))
    open(os.path.join(here, "real.txt")).read()
open(os.path.join(here, "after.txt")).read()
`,
		"real.txt": "", "after.txt": "",
	})
	checkDeps(t, runPyTraced(t, root, "main.py"), []string{"real.txt", "after.txt"}, nil)
}

// mock-fs swaps the fs binding's functions for those of its fake file system while it
// is active (_mockedBinding): the shim writes its trace with the real ones.
func TestNodeShimMockedFsBinding(t *testing.T) {
	root := shimRepo(t, nil)
	procs := traceNode(t, root, map[string]string{"mocked.txt": "", "after.txt": ""}, `
const fs = require('fs');
const path = require('path');
const binding = process.binding('fs');
// What mock-fs does when it is loaded: binding functions dispatch to _mockedBinding
// while it is set (mock() sets it, mock.restore() clears it).
for (const key of ['open', 'close', 'writeString', 'writeBuffer', 'fstat']) {
  const orig = binding[key];
  if (typeof orig !== 'function') continue;
  binding[key] = function () {
    if (this._mockedBinding) return this._mockedBinding[key].apply(this._mockedBinding, arguments);
    return orig.apply(this, arguments);
  }.bind(binding);
}
const fail = () => { const e = new Error('ENOENT, no such file or directory'); e.code = 'ENOENT'; throw e; };
binding._mockedBinding = { open: fail, close: fail, writeString: fail, writeBuffer: fail, fstat: fail };
globalThis.__smatesTrace.context('test:' + path.resolve('a.test.js'));
try { fs.readFileSync('mocked.txt'); } catch (_) {}
binding._mockedBinding = undefined;
fs.readFileSync('after.txt');
`)
	nodeVersion(t)
	checkDeps(t, readsOf(procs, "node"), []string{"mocked.txt", "after.txt"}, nil)
	found := false
	for _, p := range procs {
		for _, s := range p.CtxSpans {
			found = found || s.ID == "test:a.test.js"
		}
	}
	if !found {
		t.Error("the context switch while the binding was mocked was lost")
	}
}

// The shim's process.binding (for mock-fs) raises no deprecation warning under pending
// deprecations (fatal with --throw-deprecation).
func TestNodeShimPendingDeprecation(t *testing.T) {
	root := shimRepo(t, nil)
	traceNode(t, root, map[string]string{"a.txt": ""}, `
process.on('warning', (w) => {
  if (w.code === 'DEP0111') {
    console.error(w);
    process.exitCode = 3;
  }
});
require('fs').readFileSync('a.txt');
`, "NODE_PENDING_DEPRECATION=1")
}

// A pool worker that exits by itself may be killed before Node writes its coverage at
// exit (jest-worker restarting a worker over its idle memory limit, right after the
// worker's "exit"): the shim writes the coverage on "exit", so it exists before the
// process is done exiting.
func TestNodeShimPoolWorkerCoverageAtExit(t *testing.T) {
	root := shimRepo(t, nil)
	count := filepath.Join(t.TempDir(), "count")
	procs := traceNode(t, root, map[string]string{
		"lib.cjs": "module.exports = 1;\n",
		// The listener runs after the shim's (loaded first).
		"node_modules/jest-worker/build/workers/processChild.js": `require('../../../../lib.cjs');
process.on('exit', () => {
  const fs = require('fs');
  const n = fs.readdirSync(process.env.NODE_V8_COVERAGE).filter((f) => f.startsWith('coverage-' + process.pid + '-')).length;
  fs.writeFileSync(process.env.COUNT_FILE, String(n));
});
`,
	}, `
const c = require('child_process').fork('node_modules/jest-worker/build/workers/processChild.js');
c.on('exit', (code) => process.exit(code));
`, "COUNT_FILE="+count)
	nodeVersion(t)
	if n, err := os.ReadFile(count); err != nil || string(n) == "0" {
		t.Errorf("coverage files of the pool worker in its exit listener: %q %v, want some", n, err)
	}
	for _, p := range procs {
		if len(p.Argv) > 1 && strings.HasSuffix(p.Argv[1], "processChild.js") && !p.Code["lib.cjs"] {
			t.Errorf("pool worker: code %v, want lib.cjs", p.Code)
		}
	}
}

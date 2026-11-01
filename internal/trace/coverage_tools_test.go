package trace

import (
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// Coexistence with coverage tools: V8 coverage directories per invocation, a user's
// NODE_V8_COVERAGE, tools driving the V8 profiler through node:inspector, and Python
// coverage tools sharing sys.monitoring / sys.setprofile.

func TestSeparateV8(t *testing.T) {
	td := t.TempDir()
	env := []string{"A=1", "NODE_V8_COVERAGE=" + V8Dir(td)}
	if got, _ := SeparateV8(env, td, 0); !reflect.DeepEqual(got, env) {
		t.Errorf("invocation 0: %v", got)
	}
	got, err := SeparateV8(env, td, 2)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"A=1", "NODE_V8_COVERAGE=" + V8DirN(td, 2)}; !reflect.DeepEqual(got, want) {
		t.Errorf("invocation 2: %v, want %v", got, want)
	}
	if env[1] != "NODE_V8_COVERAGE="+V8Dir(td) {
		t.Errorf("input env modified: %v", env)
	}
	if fi, err := os.Stat(V8DirN(td, 2)); err != nil || !fi.IsDir() {
		t.Errorf("directory of invocation 2 not created: %v", err)
	}
	// Not smates' directory (a runner without tracing, or a user override): unchanged.
	other := []string{"NODE_V8_COVERAGE=/elsewhere"}
	if got, _ := SeparateV8(other, td, 1); !reflect.DeepEqual(got, other) {
		t.Errorf("foreign directory changed: %v", got)
	}
}

func TestCollectReadsEveryV8Dir(t *testing.T) {
	root := repoDir(t)
	td := t.TempDir()
	v8File(t, td, 10, 1, 0, v8JSON("file://"+filepath.Join(root, "a.js")))
	dir := V8DirN(td, 1)
	os.MkdirAll(dir, 0o755)
	if err := os.WriteFile(filepath.Join(dir, "coverage-11-1-0.json"), []byte(v8JSON("file://"+filepath.Join(root, "b.js"))), 0o644); err != nil {
		t.Fatal(err)
	}
	procs, err := Collect(td, root)
	if err != nil {
		t.Fatal(err)
	}
	code := map[int][]string{}
	for _, p := range procs {
		for c := range p.Code {
			code[p.Pid] = append(code[p.Pid], c)
		}
	}
	if !reflect.DeepEqual(code, map[int][]string{10: {"a.js"}, 11: {"b.js"}}) {
		t.Errorf("code per process %v", code)
	}
}

func TestUserV8Dir(t *testing.T) {
	t.Setenv("NODE_V8_COVERAGE", "")
	t.Setenv("SMATES_TRACE_DIR", "")
	if got := UserV8Dir(nil, "/suite"); got != "" {
		t.Errorf("unset: %q", got)
	}
	if got := UserV8Dir([]string{"NODE_V8_COVERAGE=cov"}, "/suite"); got != filepath.Join("/suite", "cov") {
		t.Errorf("relative: %q", got)
	}
	t.Setenv("NODE_V8_COVERAGE", "/abs/cov")
	if got := UserV8Dir(nil, "/suite"); got != "/abs/cov" {
		t.Errorf("from smates' environment: %q", got)
	}
	// An outer smates run tracing this one set it: not the user's.
	t.Setenv("SMATES_TRACE_DIR", "/outer/trace")
	t.Setenv("NODE_V8_COVERAGE", "/outer/trace/batch-0/v8")
	if got := UserV8Dir(nil, "/suite"); got != "" {
		t.Errorf("outer smates directory taken for the user's: %q", got)
	}
}

func TestCopyV8KeepsUserFiles(t *testing.T) {
	td := t.TempDir()
	v8File(t, td, 1, 1, 0, `{"result":[]}`)
	os.MkdirAll(V8DirN(td, 1), 0o755)
	os.WriteFile(filepath.Join(V8DirN(td, 1), "coverage-2-1-0.json"), []byte(`{"result":[]}`), 0o644)
	os.WriteFile(filepath.Join(V8Dir(td), "notes.txt"), []byte("x"), 0o644)
	dst := filepath.Join(t.TempDir(), "user", "cov")
	if err := os.MkdirAll(dst, 0o755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(dst, "coverage-1-1-0.json"), []byte("mine"), 0o644)
	os.WriteFile(filepath.Join(dst, "other.json"), []byte("mine"), 0o644)
	if err := CopyV8(td, dst); err != nil {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(dst)
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	if want := []string{"coverage-1-1-0.json", "coverage-2-1-0.json", "other.json"}; !reflect.DeepEqual(names, want) {
		t.Errorf("files %v, want %v", names, want)
	}
	if b, _ := os.ReadFile(filepath.Join(dst, "coverage-1-1-0.json")); string(b) != "mine" {
		t.Errorf("an existing file was overwritten: %q", b)
	}
}

func TestNodeShimInNodeModules(t *testing.T) {
	sh, err := ExtractShims(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	// Coverage reports leave out node_modules: the preloaded shim must not appear in them.
	if !strings.Contains(filepath.ToSlash(sh.Node), "/node_modules/") {
		t.Errorf("node shim at %s", sh.Node)
	}
	if _, err := os.Stat(sh.Node); err != nil {
		t.Error(err)
	}
}

// A tool taking and stopping V8's precise coverage itself (vitest's v8 provider, jest's
// v8 coverage) ends the collection behind NODE_V8_COVERAGE: the shim records the scripts
// of the coverage it takes.
func TestNodeShimInspectorCoverage(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not on PATH")
	}
	root := shimRepo(t, map[string]string{
		"lib.js": "exports.f = () => 1;\n",
		"test.mjs": `import { Session } from 'node:inspector/promises';
import { createRequire } from 'node:module';
const s = new Session();
s.connect();
await s.post('Profiler.enable');
await s.post('Profiler.startPreciseCoverage', { callCount: true, detailed: true });
createRequire(import.meta.url)('./lib.js').f();
const cov = await s.post('Profiler.takePreciseCoverage');
if (!cov.result.some((r) => r.url.endsWith('/lib.js'))) throw new Error('result changed');
await s.post('Profiler.stopPreciseCoverage');
await s.post('Profiler.disable');
s.disconnect();
`,
		"cb.cjs": `const { Session } = require('inspector');
const s = new Session();
s.connect();
s.post('Profiler.enable', () => {
  s.post('Profiler.startPreciseCoverage', { callCount: true, detailed: true }, () => {
    require('./lib.js').f();
    s.post('Profiler.takePreciseCoverage', (err, res) => {
      if (err || !res.result.length) process.exit(3);
      s.post('Profiler.stopPreciseCoverage', () => s.disconnect());
    });
  });
});
`,
	})
	for _, script := range []string{"test.mjs", "cb.cjs"} {
		t.Run(script, func(t *testing.T) {
			sh, traceDir := extractShims(t)
			if err := os.MkdirAll(V8Dir(traceDir), 0o755); err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command(node, script)
			cmd.Dir = root
			cmd.Env = append(os.Environ(), NodeEnv(traceDir, root, sh, []string{"NODE_OPTIONS="})...)
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("%v\n%s", err, out)
			}
			procs, err := Collect(traceDir, root)
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, p := range procs {
				found = found || p.Code["lib.js"]
			}
			if !found {
				t.Errorf("lib.js not recorded as executed")
			}
		})
	}
}

// pyMonitoringScript claims sys.monitoring tool ids and installs a profile function the
// way other tools would before smates starts, then starts smates' tracer (python -S: no
// sitecustomize) and checks that both keep working.
const pyMonitoringScript = `
import os, sys
sys.path.insert(0, os.environ["SHIM_DIR"])
taken = [int(x) for x in os.environ.get("TAKE_IDS", "").split(",") if x]
mon = getattr(sys, "monitoring", None)
if mon is not None:
    for i in taken:
        mon.use_tool_id(i, "other")
calls = []
def other(frame, event, arg):
    if event == "call":
        calls.append(frame.f_code.co_name)
sys.setprofile(other)
import smates_trace
t = smates_trace.install()
import lib
lib.helper()
sys.setprofile(None)
if "helper" not in calls:
    sys.exit("the earlier profile function was not called")
print("tool", t.tool)
`

func TestPythonShimToolIDs(t *testing.T) {
	py, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 not on PATH")
	}
	hasMon := exec.Command(py, "-c", "import sys; sys.exit(0 if hasattr(sys, 'monitoring') else 1)").Run() == nil
	cases := []struct {
		take string
		want string // tool id smates uses ("None": setprofile)
	}{
		{"", "3"},
		{"1,3", "4"},            // coverage.py moved up to 3: smates takes the next free one
		{"0,1,2,3,4,5", "None"}, // all taken: falls back to setprofile, chaining
	}
	for _, c := range cases {
		t.Run("take="+c.take, func(t *testing.T) {
			want := c.want
			if !hasMon {
				want = "None"
			}
			root := shimRepo(t, map[string]string{
				"t.py":   pyMonitoringScript,
				"lib.py": "def helper():\n    return 1\n",
			})
			sh, traceDir := extractShims(t)
			cmd := exec.Command(py, "-S", "t.py")
			cmd.Dir = root
			cmd.Env = append(os.Environ(), "SHIM_DIR="+sh.Python, "TAKE_IDS="+c.take, "SMATES_TRACE_DIR="+traceDir,
				"SMATES_REPO_ROOT="+root, "PYTHONDONTWRITEBYTECODE=1")
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("%v\n%s", err, out)
			}
			if !strings.Contains(string(out), "tool "+want+"\n") {
				t.Errorf("output %q, want tool %s", out, want)
			}
			procs, err := Collect(traceDir, root)
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, p := range procs {
				found = found || p.Code["lib.py"]
			}
			if !found {
				t.Errorf("lib.py not recorded as executed")
			}
		})
	}
}

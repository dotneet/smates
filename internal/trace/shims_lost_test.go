package trace

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// streamsOf returns the contents of the trace streams in traceDir.
func streamsOf(t *testing.T, traceDir string) []string {
	t.Helper()
	m, _ := filepath.Glob(filepath.Join(traceDir, "proc-*.jsonl"))
	var out []string
	for _, f := range m {
		data, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, string(data))
	}
	return out
}

// Events the Python shim cannot write are counted and reported by the next one written
// ({"k":"lost"}); a write cut short is ended before the next event. Every variant leaves
// the process marked as lost, and what was written afterwards is still read.
func TestPythonShimLostWrites(t *testing.T) {
	py := testPython(t)
	const prelude = `
import os, smates_trace
real = smates_trace._io
def use(write):
    smates_trace._io = (real[0], write, real[2], real[3])
def failing(fd, data):
    raise OSError(28, "No space left on device")
cut = []
def cut_short(fd, data):
    if not cut:
        cut.append(1)
        return real[1](fd, bytes(data[:7]))
    raise OSError(28, "No space left on device")
def short(fd, data):  # writes a few bytes at a time
    return real[1](fd, bytes(data[:5]))
open("a.txt").read()
`
	for _, tc := range []struct {
		name, script string
		lost         bool
	}{
		{name: "nothing lost", script: "open('c.txt').read()\n"},
		{name: "partial writes completed", script: "use(short)\nopen('b.txt').read()\nuse(real[1])\nopen('c.txt').read()\n"},
		{name: "failed writes", script: "use(failing)\nopen('b.txt').read()\nuse(real[1])\nopen('c.txt').read()\n", lost: true},
		{name: "write cut short", script: "use(cut_short)\nopen('b.txt').read()\nuse(real[1])\nopen('c.txt').read()\n", lost: true},
		{name: "failed open", script: "smates_trace._io = (lambda *a: (_ for _ in ()).throw(OSError(24, 'Too many open files')),) + real[1:]\nopen('b.txt').read()\nsmates_trace._io = real\nopen('c.txt').read()\n", lost: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := shimRepo(t, map[string]string{"a.txt": "", "b.txt": "", "c.txt": "", "s.py": prelude + tc.script})
			sh, traceDir := extractShims(t)
			if out, code := runPy(t, root, PythonEnv(traceDir, root, sh, os.Environ()), py, "s.py"); code != 0 {
				t.Fatalf("exit %d: %s", code, out)
			}
			procs, err := Collect(traceDir, root)
			if err != nil {
				t.Fatal(err)
			}
			if len(procs) != 1 {
				t.Fatalf("%d processes", len(procs))
			}
			p := procs[0]
			if p.Lost != tc.lost {
				t.Errorf("lost %v, want %v; streams %q", p.Lost, tc.lost, streamsOf(t, traceDir))
			}
			if !p.Reads["a.txt"] || !p.Reads["c.txt"] || !p.Exited {
				t.Errorf("reads %v, exited %v; streams %q", keys(p.Reads), p.Exited, streamsOf(t, traceDir))
			}
			if tc.lost && !strings.Contains(strings.Join(streamsOf(t, traceDir), ""), `{"k": "lost", "n": `) {
				t.Errorf("no lost event: %q", streamsOf(t, traceDir))
			}
		})
	}
}

// A Python process whose tracing fails half-way writes no trace at all (smates then
// analyzes the command that started it), not a start event that makes it look like a
// process that read nothing; its children are not told it is their traced ancestor,
// and the pytest plugin gets no tracer.
func TestPythonShimInstallFailure(t *testing.T) {
	py := testPython(t)
	for _, hook := range []string{"_wrap_probes", "_wrap_spawn", "_publish"} {
		t.Run(hook, func(t *testing.T) {
			root := shimRepo(t, map[string]string{"a.txt": "", "b.txt": ""})
			sh, traceDir := extractShims(t)
			writeFiles(t, root, map[string]string{"s.py": `
import os, subprocess, sys
sys.path.insert(0, os.environ["SHIMS"])
import smates_trace
def boom(*a, **k):
    raise RuntimeError("boom")
setattr(smates_trace._Tracer, "` + hook + `", boom)
assert smates_trace.install() is None
assert smates_trace.install() is None and smates_trace.tracer() is None
assert os.environ.get("SMATES_TRACE_PARENT") == "5-0", os.environ.get("SMATES_TRACE_PARENT")
open("a.txt").read()
os.listdir(".")
subprocess.run(["cat", "b.txt"], stdout=subprocess.DEVNULL)
if hasattr(os, "fork"):
    pid = os.fork()
    if pid == 0:
        open("b.txt").read()
        os._exit(0)
    os.waitpid(pid, 0)
`})
			if _, err := exec.LookPath("cat"); err != nil {
				t.Skip("cat not on PATH")
			}
			// Not through sitecustomize: the script makes install fail first.
			env := []string{"SMATES_TRACE_DIR=" + traceDir, "SMATES_REPO_ROOT=" + root, "SMATES_TRACE_PARENT=5-0", "SHIMS=" + sh.Python, "PYTHONPATH="}
			out, code := runPy(t, root, env, py, "s.py")
			if code != 0 {
				t.Fatalf("exit %d: %s", code, out)
			}
			if !strings.Contains(out, "smates: tracing disabled") {
				t.Errorf("no warning: %s", out)
			}
			if s := streamsOf(t, traceDir); len(s) != 0 {
				t.Errorf("streams written: %q", s)
			}
		})
	}
}

// An event the Node shim cannot write (the program closed its descriptor) is reported by
// the next one, written to the file opened again.
func TestNodeShimLostWrites(t *testing.T) {
	root := shimRepo(t, nil)
	procs := traceNode(t, root, map[string]string{"a.txt": "", "b.txt": "", "c.txt": ""}, `
const fs = require('fs');
const path = require('path');
fs.readFileSync('a.txt');
const dir = process.env.SMATES_TRACE_DIR;
const name = fs.readdirSync(dir).find((n) => n.startsWith('proc-' + process.pid + '-0-'));
const ino = fs.statSync(path.join(dir, name)).ino;
for (let fd = 3; fd < 256; fd++) {
  try {
    if (fs.fstatSync(fd).ino === ino) fs.closeSync(fd);
  } catch (_) {}
}
// Lost: written to the closed descriptor.
fs.readFileSync('b.txt');
fs.readFileSync('c.txt');
`)
	var p *Proc
	for _, q := range procs {
		if q.Lang == "node" {
			p = q
		}
	}
	if p == nil {
		t.Fatalf("procs %v", procs)
	}
	if !p.Lost {
		t.Errorf("not lost: reads %v", keys(p.Reads))
	}
	if !p.Reads["a.txt"] || !p.Reads["c.txt"] || !p.Exited {
		t.Errorf("reads %v, exited %v", keys(p.Reads), p.Exited)
	}
}

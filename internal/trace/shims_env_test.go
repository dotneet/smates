package trace

import (
	"os"
	"os/exec"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/dotneet/smates/internal/testutil"
)

// runShim runs a script of the repository root with tracing (argv[0] is the interpreter
// and its tracing flags) and returns the environment variables it was seen to read.
func runShim(t *testing.T, root, script, code string, argv ...string) []string {
	t.Helper()
	if _, err := exec.LookPath(argv[0]); err != nil {
		t.Skipf("%s not on PATH", argv[0])
	}
	sh, err := ExtractShims(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	testutil.Write(t, root, script, code)
	td := t.TempDir()
	for i, a := range argv {
		argv[i] = strings.NewReplacer("{node}", sh.Node).Replace(a)
	}
	cmd := exec.Command(argv[0], append(argv[1:], script)...)
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "SMATES_TRACE_DIR="+td, "SMATES_REPO_ROOT="+root, "PYTHONPATH="+sh.Python,
		"APP_REGION=eu", "APP_COPIED=x", "SET_LATER=outer", "NODE_OPTIONS=", "PYTHONDONTWRITEBYTECODE=1")
	if out, err := cmd.CombinedOutput(); err != nil || !strings.Contains(string(out), "done") {
		t.Fatalf("%v: %v\n%s", argv, err, out)
	}
	procs, err := Collect(td, root)
	if err != nil {
		t.Fatal(err)
	}
	var env []string
	for _, p := range procs {
		for k := range p.Reads {
			if name, ok := strings.CutPrefix(k, "$"); ok {
				env = append(env, name)
			}
		}
	}
	sort.Strings(env)
	return env
}

func TestPythonShimEnv(t *testing.T) {
	root := repoDir(t)
	got := runShim(t, root, "t.py", `
import os
os.environ.get("APP_REGION")
"APP_UNSET" in os.environ
try:
    os.environ["APP_MISSING"]
except KeyError:
    pass
try:
    os.getenv(None)  # the caller's TypeError, not one from the shim
except TypeError:
    pass
dict(os.environ)
os.environ.copy()
[v for _, v in os.environ.items()]
os.environ["SET_LATER"] = "inner"
os.environ["SET_LATER"]
print("done")
`, "python3")
	if want := []string{"APP_MISSING", "APP_REGION", "APP_UNSET"}; !reflect.DeepEqual(got, want) {
		t.Errorf("read %v, want %v", got, want)
	}
}

func TestNodeShimEnv(t *testing.T) {
	root := repoDir(t)
	// Console formatting can read FORCE_COLOR on some Node versions; use raw
	// output so the script observes only the environment reads under test.
	got := runShim(t, root, "t.js", `
void process.env.APP_REGION;
void ('APP_UNSET' in process.env);
const copy = { ...process.env };
JSON.stringify(process.env);
Object.keys(process.env);
process.env.SET_LATER = 'inner';
void process.env.SET_LATER;
process.stdout.write('done\n');
`, "node", "--require={node}")
	if want := []string{"APP_REGION", "APP_UNSET"}; !reflect.DeepEqual(got, want) {
		t.Errorf("read %v, want %v", got, want)
	}
}

// The environment proxy can be cloned like process.env itself (structuredClone,
// postMessage, workerData, v8.serialize), at the top level and one level down; a clone
// is a copy, not a read of every variable.
func TestNodeShimEnvClone(t *testing.T) {
	const script = `
const wt = require('worker_threads');
const v8 = require('v8');
if (wt.isMainThread) {
  if (structuredClone(process.env).APP_COPIED !== 'x') throw new Error('structuredClone');
  if (structuredClone({ env: process.env }).env.APP_COPIED !== 'x') throw new Error('nested');
  structuredClone([process.env]);
  // Getters run once, as without the shim (Node 18's structuredClone posts through a
  // MessagePort).
  let n = 0;
  const getter = { get g() { n++; return 1; } };
  structuredClone(getter);
  structuredClone({ get g() { n++; return 1; }, env: process.env });
  const ch = new wt.MessageChannel();
  ch.port1.postMessage(getter);
  ch.port1.close();
  if (n !== 3) throw new Error('getters ran ' + n + ' times, want 3');
  const { port1 } = new wt.MessageChannel();
  port1.postMessage({ env: process.env });
  port1.close();
  void process.env.APP_REGION;
  const w = new wt.Worker(__filename, { workerData: process.env });
  w.on('message', (m) => {
    if (m !== 'x') process.exit(2);
    // A replaced environment is traced too (process.env itself is no v8.serialize value).
    process.env = structuredClone(process.env);
    if (v8.deserialize(v8.serialize(process.env)).APP_COPIED !== 'x') throw new Error('v8');
    process.stdout.write('done\n');
  });
} else {
  wt.parentPort.postMessage(wt.workerData.APP_COPIED);
}
`
	for _, argv := range [][]string{{"node", "--require={node}"}, {"bun", "--preload={node}"}} {
		t.Run(argv[0], func(t *testing.T) {
			got := runShim(t, repoDir(t), "t.js", script, argv...)
			if want := []string{"APP_REGION"}; !reflect.DeepEqual(got, want) {
				t.Errorf("read %v, want %v", got, want)
			}
		})
	}
}

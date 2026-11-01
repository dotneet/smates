package trace

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/dotneet/smates/internal/state"
)

// The driver feeds the reporter the objects Vitest hands it (shapes observed with
// Vitest 1.6, 2.1, 3.2, 4.1 and 5.0 under --bail): r() builds a TestCase, mod() a
// TestModule, file()/leaf() the older File task tree.
const vitestReporterDriver = `
const { default: Reporter } = await import(process.env.REPORTER);
const root = process.env.ROOT;
const r = (name, mode, state) => ({ name, options: { mode }, result: () => ({ state }) });
const mod = (file, state, tests) => ({
  moduleId: root + '/' + file,
  state: () => state,
  errors: () => [],
  diagnostic: () => ({ duration: 5 }),
  children: { allTests: function* () { yield* tests; } },
});
const leaf = (name, mode, state) => ({ type: 'test', name, mode, result: state ? { state } : undefined });
const file = (name, state, tasks) => ({ filepath: root + '/' + name, mode: 'run', result: state ? { state } : undefined, tasks });

// Vitest 3+: an interrupted (--bail) run.
new Reporter().onTestRunEnd([
  // 4+: started, the rest cancelled (skipped, mode still run).
  mod('v5-cancelled.test.ts', 'passed', [r('a1', 'run', 'passed'), r('askip', 'skip', 'skipped'), r('a2', 'run', 'skipped')]),
  // 3.x: the tests not run stay pending.
  mod('v3-cancelled.test.ts', 'passed', [r('c1', 'run', 'passed'), r('c2', 'run', 'pending')]),
  // Never started.
  mod('never.test.ts', 'pending', []),
  mod('queued.test.ts', 'queued', []),
  mod('failed.test.ts', 'failed', [r('b1', 'run', 'failed'), r('b2', 'run', 'skipped')]),
  // Finished before the bail, with declared skips and todos.
  mod('done.test.ts', 'passed', [r('d1', 'run', 'passed'), r('dskip', 'skip', 'skipped'), r('dtodo', 'todo', 'skipped')]),
], [], 'interrupted');

// Vitest 3+: a complete run; a test skipping itself (ctx.skip) keeps its collected mode.
new Reporter().onTestRunEnd([
  mod('dynskip.test.ts', 'passed', [r('e1', 'run', 'skipped'), r('e2', 'run', 'passed')]),
  mod('allskip.test.ts', 'skipped', [r('f1', 'skip', 'skipped')]),
], [], 'passed');

// Vitest 1/2 (onFinished).
new Reporter().onFinished([
  file('old-cancelled.test.ts', 'pass', [leaf('c1', 'run', 'pass'), leaf('c2', 'run')]),
  file('old-never.test.ts', undefined, []),
  // Vitest 1 reports a file it never started as skipped, without tasks.
  file('old-never-v1.test.ts', 'skip', []),
  file('old-done.test.ts', 'pass', [leaf('d1', 'run', 'pass'), leaf('dskip', 'skip'), { type: 'suite', name: 's', mode: 'skip', tasks: [leaf('in', 'skip')] }, leaf('dyn', 'skip', 'skip')]),
  file('old-allskip.test.ts', 'skip', [leaf('f1', 'skip'), leaf('f2', 'todo')]),
  file('old-failed.test.ts', 'fail', [leaf('a1', 'run', 'fail'), leaf('a2', 'run')]),
]);
`

// A file Vitest did not run to the end (--bail, an interrupted run) is not a pass: the
// tests it skipped would otherwise be recorded as passing.
func TestVitestReporterUnfinishedFiles(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not on PATH")
	}
	shims := t.TempDir()
	if _, err := ExtractShims(shims); err != nil {
		t.Fatal(err)
	}
	root, _ := filepath.EvalSymlinks(t.TempDir())
	td := t.TempDir()
	driver := filepath.Join(t.TempDir(), "driver.mjs")
	if err := os.WriteFile(driver, []byte(vitestReporterDriver), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(node, driver)
	cmd.Env = append(os.Environ(), "REPORTER=file://"+filepath.ToSlash(filepath.Join(shims, "vitest-reporter.mjs")), "ROOT="+filepath.ToSlash(root), "SMATES_TRACE_DIR="+td)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	want := map[string]string{
		"v5-cancelled.test.ts": state.Unknown, "v3-cancelled.test.ts": state.Unknown, "never.test.ts": state.Unknown,
		"queued.test.ts": state.Unknown, "failed.test.ts": state.Fail, "done.test.ts": state.Pass,
		"dynskip.test.ts": state.Pass, "allskip.test.ts": state.Pass,
		"old-cancelled.test.ts": state.Unknown, "old-never.test.ts": state.Unknown, "old-never-v1.test.ts": state.Unknown,
		"old-done.test.ts": state.Pass, "old-allskip.test.ts": state.Pass, "old-failed.test.ts": state.Fail,
	}
	got := Results(td, root)
	for f, st := range want {
		if got[f].Status != st {
			t.Errorf("%s: status %q, want %q", f, got[f].Status, st)
		}
	}
	if len(got) != len(want) {
		t.Errorf("results %v", got)
	}
}

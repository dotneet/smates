package trace

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dotneet/smates/internal/state"
)

// The driver feeds the reporter the objects Vitest hands it (shapes observed with
// Vitest 1.6, 2.1, 3.2, 4.1 and 5.0 under --bail): r() builds a TestCase, mod() a
// TestModule, file()/leaf() the older File task tree.
const vitestReporterDriver = `
const { default: Reporter } = await import(process.env.REPORTER);
const root = process.env.ROOT;
const r = (name, mode, state, task) => ({ name, options: { mode }, result: () => ({ state }), task });
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

// Vitest 3+: a complete run; a test skipping itself (ctx.skip) keeps its collected mode,
// and the runner marks its task pending.
new Reporter().onTestRunEnd([
  mod('dynskip.test.ts', 'passed', [r('e1', 'run', 'skipped'), r('e2', 'run', 'passed')]),
  mod('dynskip-all.test.ts', 'skipped', [r('g1', 'run', 'skipped')]),
  mod('dynskip-pending.test.ts', 'passed', [r('h1', 'skip', 'skipped', { pending: true }), r('h2', 'run', 'passed')]),
  mod('dynskip-result-pending.test.ts', 'passed', [r('i1', 'skip', 'skipped', { result: { state: 'skip', pending: true } })]),
  mod('allskip.test.ts', 'skipped', [r('f1', 'skip', 'skipped')]),
  mod('declared.test.ts', 'passed', [r('j1', 'run', 'passed'), r('j2', 'skip', 'skipped', {}), r('j3', 'todo', 'skipped')]),
], [], 'passed');

// Vitest 1/2 (onFinished).
new Reporter().onFinished([
  file('old-cancelled.test.ts', 'pass', [leaf('c1', 'run', 'pass'), leaf('c2', 'run')]),
  file('old-never.test.ts', undefined, []),
  // Vitest 1 reports a file it never started as skipped, without tasks.
  file('old-never-v1.test.ts', 'skip', []),
  file('old-done.test.ts', 'pass', [leaf('d1', 'run', 'pass'), leaf('dskip', 'skip'), { type: 'suite', name: 's', mode: 'skip', tasks: [leaf('in', 'skip')] }, leaf('dyn', 'skip', 'skip')]),
  file('old-allskip.test.ts', 'skip', [leaf('f1', 'skip'), leaf('f2', 'todo')]),
  // A test that skipped itself is left pending.
  file('old-dynskip.test.ts', 'pass', [leaf('d1', 'run', 'pass'), Object.assign(leaf('dyn', 'skip', 'skip'), { pending: true })]),
  file('old-dynskip-result.test.ts', 'skip', [{ type: 'test', name: 'dyn', mode: 'skip', result: { state: 'skip', pending: true } }]),
  file('old-failed.test.ts', 'fail', [leaf('a1', 'run', 'fail'), leaf('a2', 'run')]),
]);

// Unhandled errors leave the module passed: the error's VITEST_TEST_PATH names the file
// that was running. Vitest 3+ (onTestRunEnd's second argument):
const ok = (file) => mod(file, 'passed', [r('t', 'run', 'passed')]);
const named = (file, msg) => Object.assign(new Error(msg || 'boom in ' + file), { VITEST_TEST_PATH: root + '/' + file });
new Reporter().onTestRunEnd([ok('uh-src.test.ts'), ok('uh-other.test.ts')], [named('uh-src.test.ts', 'unhandled boom')], 'failed');
new Reporter().onTestRunEnd([ok('uh-twice.test.ts')], [named('uh-twice.test.ts'), named('uh-twice.test.ts')], 'failed');
new Reporter().onTestRunEnd([ok('uh-rel.test.ts')], [{ message: 'relative', VITEST_TEST_PATH: './uh-rel.test.ts' }], 'failed');
new Reporter().onTestRunEnd([ok('uh-win.test.ts')], [{ message: 'backslashes', VITEST_TEST_PATH: (root + '/uh-win.test.ts').split('/').join('\\') }], 'failed');
// Errors naming no file of the run: no file passes; failures stand.
new Reporter().onTestRunEnd([ok('uh-anon.test.ts'), mod('uh-anon-fail.test.ts', 'failed', [r('x', 'run', 'failed')])], [new Error('no file')], 'failed');
new Reporter().onTestRunEnd([ok('uh-elsewhere.test.ts')], [named('not-in-this-run.test.ts')], 'failed');
new Reporter().onTestRunEnd([ok('uh-primitive.test.ts')], ['a thrown string'], 'failed');
new Reporter().onTestRunEnd([ok('uh-mixed-src.test.ts'), ok('uh-mixed-other.test.ts')], [named('uh-mixed-src.test.ts'), {}], 'failed');
new Reporter().onTestRunEnd([ok('uh-none.test.ts')], [], 'passed');
new Reporter().onTestRunEnd([ok('uh-undefined.test.ts')], undefined, 'passed');
// Vitest 1/2 (onFinished's second argument).
const okFile = (name) => file(name, 'pass', [leaf('t', 'run', 'pass')]);
new Reporter().onFinished([okFile('old-uh-src.test.ts'), okFile('old-uh-other.test.ts')], [named('old-uh-src.test.ts', 'old unhandled boom')]);
new Reporter().onFinished([okFile('old-uh-anon.test.ts'), file('old-uh-anon-fail.test.ts', 'fail', [leaf('a', 'run', 'fail')])], [new Error('no file')]);
new Reporter().onFinished([okFile('old-uh-none.test.ts')]);
`

// A file Vitest did not run to the end (--bail, an interrupted run) is not a pass: the
// tests it skipped would otherwise be recorded as passing. Nor is one a test of which
// skipped itself (ctx.skip()), all or some of its tests; declared skips (it.skip, .todo)
// are disabled tests and keep it passing. An unhandled error fails the file it names
// (VITEST_TEST_PATH), which Vitest leaves passed; one naming no file of the run keeps
// every file from passing.
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
		"dynskip.test.ts": state.Unknown, "dynskip-all.test.ts": state.Unknown, "dynskip-pending.test.ts": state.Unknown,
		"dynskip-result-pending.test.ts": state.Unknown, "allskip.test.ts": state.Pass, "declared.test.ts": state.Pass,
		"old-dynskip.test.ts": state.Unknown, "old-dynskip-result.test.ts": state.Unknown,
		"old-cancelled.test.ts": state.Unknown, "old-never.test.ts": state.Unknown, "old-never-v1.test.ts": state.Unknown,
		"old-done.test.ts": state.Pass, "old-allskip.test.ts": state.Pass, "old-failed.test.ts": state.Fail,
		// Unhandled errors.
		"uh-src.test.ts": state.Fail, "uh-other.test.ts": state.Pass, "uh-twice.test.ts": state.Fail,
		"uh-rel.test.ts": state.Fail, "uh-win.test.ts": state.Fail,
		"uh-anon.test.ts": state.Unknown, "uh-anon-fail.test.ts": state.Fail, "uh-elsewhere.test.ts": state.Unknown,
		"uh-primitive.test.ts": state.Unknown, "uh-mixed-src.test.ts": state.Fail, "uh-mixed-other.test.ts": state.Unknown,
		"uh-none.test.ts": state.Pass, "uh-undefined.test.ts": state.Pass,
		"old-uh-src.test.ts": state.Fail, "old-uh-other.test.ts": state.Pass,
		"old-uh-anon.test.ts": state.Unknown, "old-uh-anon-fail.test.ts": state.Fail, "old-uh-none.test.ts": state.Pass,
	}
	got := Results(td, root)
	for f, st := range want {
		if got[f].Status != st {
			t.Errorf("%s: status %q, want %q", f, got[f].Status, st)
		}
	}
	for f, msg := range map[string]string{"uh-src.test.ts": "unhandled boom", "old-uh-src.test.ts": "old unhandled boom"} {
		if !strings.Contains(got[f].Message, msg) {
			t.Errorf("%s: message %q, want the unhandled error", f, got[f].Message)
		}
	}
	if len(got) != len(want) {
		t.Errorf("results %v", got)
	}
}

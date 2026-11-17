package trace

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/dotneet/smates/internal/state"
)

// runReporterDriver runs a Node driver script against the extracted reporters and returns
// the results they wrote, by repo-relative file.
func runReporterDriver(t *testing.T, driver string) map[string]Result {
	t.Helper()
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
	script := filepath.Join(t.TempDir(), "driver.mjs")
	if err := os.WriteFile(script, []byte(driver), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(node, script)
	cmd.Env = append(os.Environ(),
		"VITEST_REPORTER=file://"+filepath.ToSlash(filepath.Join(shims, "vitest-reporter.mjs")),
		"JEST_REPORTER="+filepath.Join(shims, "jest-reporter.cjs"),
		"ROOT="+filepath.ToSlash(root), "SMATES_TRACE_DIR="+td)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	return Results(td, root)
}

// A Jest run that fails for a reason no file's result shows (global setup or teardown,
// obsolete snapshots) records no pass for the files it does not tell apart, also when
// another file failed (which keeps the run's exit code from saying so).
func TestJestReporterRunErrors(t *testing.T) {
	got := runReporterDriver(t, `
import { createRequire } from 'node:module';
const Reporter = createRequire(import.meta.url)(process.env.JEST_REPORTER);
const root = process.env.ROOT;
const res = (name, failing, snapshot) => ({
  testFilePath: root + '/' + name, numFailingTests: failing ? 1 : 0, numPassingTests: failing ? 0 : 1, numPendingTests: 0,
  perfStats: { start: 0, end: 1 }, testResults: [], snapshot: Object.assign({ unchecked: 0, unmatched: 0, fileDeleted: false }, snapshot),
});
const run = (results, agg) => {
  const r = new Reporter();
  for (const x of results) r.onTestResult({}, x);
  r.onRunComplete(new Set(), Object.assign({ testResults: results, snapshot: { failure: false, filesRemoved: 0 } }, agg));
};
run([res('clean-pass.test.js'), res('clean-fail.test.js', true)], {});
run([res('exec-pass.test.js'), res('exec-fail.test.js', true)], { runExecError: { message: 'globalTeardown failed' } });
run([res('snap-obsolete.test.js', false, { unchecked: 1 }), res('snap-other.test.js'), res('snap-fail.test.js', true)], { snapshot: { failure: true, filesRemoved: 0 } });
run([res('snap-removed.test.js'), res('snap-removed-fail.test.js', true)], { snapshot: { failure: true, filesRemoved: 1 } });
run([res('snap-none.test.js'), res('snap-none-fail.test.js', true)], { snapshot: { failure: true, filesRemoved: 0 } });
run([res('snap-unmatched.test.js', true, { unmatched: 1 }), res('snap-unmatched-other.test.js')], { snapshot: { failure: true, filesRemoved: 0 } });
`)
	want := map[string]string{
		"clean-pass.test.js": state.Pass, "clean-fail.test.js": state.Fail,
		"exec-pass.test.js": state.Unknown, "exec-fail.test.js": state.Fail,
		"snap-obsolete.test.js": state.Unknown, "snap-other.test.js": state.Pass, "snap-fail.test.js": state.Fail,
		"snap-removed.test.js": state.Unknown, "snap-removed-fail.test.js": state.Fail,
		"snap-none.test.js": state.Unknown, "snap-none-fail.test.js": state.Fail,
		"snap-unmatched.test.js": state.Fail, "snap-unmatched-other.test.js": state.Pass,
	}
	for f, st := range want {
		if got[f].Status != st {
			t.Errorf("%s: status %q, want %q", f, got[f].Status, st)
		}
	}
	if len(got) != len(want) {
		t.Errorf("results %v", got)
	}
}

// A result a reporter cannot write is reported by the next line it writes: smates then
// records no pass from that file of results.
func TestReportersLostWrites(t *testing.T) {
	got := runReporterDriver(t, `
import { createRequire } from 'node:module';
const { default: Vitest } = await import(process.env.VITEST_REPORTER);
const Jest = createRequire(import.meta.url)(process.env.JEST_REPORTER);
const root = process.env.ROOT;
const dir = process.env.SMATES_TRACE_DIR;
const mod = (file, state) => ({ moduleId: root + '/' + file, state: () => state, errors: () => [], diagnostic: () => ({}), children: { allTests: function* () {} } });
const jres = (name, failing) => ({ testFilePath: root + '/' + name, numFailingTests: failing ? 1 : 0, numPassingTests: 1, numPendingTests: 0, testResults: [] });

const v = new Vitest();
const j = new Jest();
process.env.SMATES_TRACE_DIR = dir + '/missing';
// Lost: the failures of v-lost and j-lost.
new Vitest().onTestRunEnd([mod('v-lost.test.ts', 'failed')], [], 'failed');
j.onTestResult({}, jres('j-lost.test.js', true));
process.env.SMATES_TRACE_DIR = dir;
v.onTestRunEnd([mod('v-pass.test.ts', 'passed')], [], 'failed');
j.onTestResult({}, jres('j-pass.test.js'));
j.onRunComplete(new Set(), { testResults: [], snapshot: {} });
`)
	for _, f := range []string{"v-pass.test.ts", "j-pass.test.js"} {
		if got[f].Status != state.Unknown {
			t.Errorf("%s: status %q, want unknown (%v)", f, got[f].Status, got)
		}
	}
	for _, f := range []string{"v-lost.test.ts", "j-lost.test.js"} {
		if _, ok := got[f]; ok {
			t.Errorf("%s reported: %v", f, got)
		}
	}
}

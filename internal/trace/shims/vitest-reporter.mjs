// smates reporter for Vitest: writes per-file results to $SMATES_TRACE_DIR.
// Supports the Vitest 3+ API (onTestRunEnd) and the older onFinished(files, errors).
import { appendFileSync, writeFileSync } from 'node:fs';
import { join } from 'node:path';

const out = () => join(process.env.SMATES_TRACE_DIR || '.', `results-vitest-${process.pid}.jsonl`);
// A result that cannot be written is counted, and the next line written says so
// ({"k":"lost"}): smates then records no pass from this file of results.
let lost = 0;
const append = (line) => {
  try {
    appendFileSync(out(), (lost ? JSON.stringify({ k: 'lost', n: lost }) + '\n' : '') + line);
    lost = 0;
  } catch {
    lost++;
  }
};
const write = (id, status, ms, msg) => {
  const ev = { k: 'result', id, status, ms: Math.round(ms || 0) };
  if (status === 'fail' && msg) ev.msg = msg;
  append(JSON.stringify(ev) + '\n');
};
const flushLost = () => {
  if (lost) append('');
};

// Failure messages are bounded: they only feed smates' failure hints.
const MAX_MSG = 4000;
const clip = (s, n) => (s.length > n ? s.slice(0, n) + '\n[…]' : s);
const errText = (e) => {
  if (!e) return '';
  let s = String(e.stack || e.message || e);
  if (e.message && !s.includes(e.message)) s = e.message + '\n' + s;
  if (e.diff) s += '\n' + e.diff;
  return clip(s, 1500);
};
const joinMsgs = (parts) => clip(parts.filter(Boolean).join('\n\n'), MAX_MSG);

// Vitest 3+: module-level errors and failed test cases of a TestModule.
const moduleMessage = (m, errors) => {
  try {
    const parts = (errors || []).map(errText);
    const tests = m.children && typeof m.children.allTests === 'function' ? m.children.allTests() : [];
    for (const t of tests) {
      const r = typeof t.result === 'function' ? t.result() : undefined;
      if (r && r.state === 'failed') parts.push((t.fullName || t.name) + ': ' + (r.errors || []).map(errText).join('\n'));
    }
    return joinMsgs(parts);
  } catch {
    return '';
  }
};

// Older API: walk the task tree of a File.
const fileMessage = (f) => {
  try {
    const parts = ((f.result && f.result.errors) || []).map(errText);
    const walk = (tasks, prefix) => {
      for (const t of tasks || []) {
        const name = prefix ? prefix + ' > ' + t.name : t.name;
        if (t.tasks) walk(t.tasks, name);
        else if (t.result && t.result.state === 'fail') parts.push(name + ': ' + (t.result.errors || []).map(errText).join('\n'));
      }
    };
    walk(f.tasks, '');
    return joinMsgs(parts);
  } catch {
    return '';
  }
};

// A file passes only if it ran to the end: with --bail (or an interrupted run) Vitest
// reports files it never started, and the tests a started file did not get to, without
// failing them. Such a file is "unknown" (it runs again next time), not "pass".
//
// A test that skips itself (ctx.skip(), at run time) is not proven to pass either: like
// a test an assumption aborted under Gradle, its file is "unknown". One declared skipped
// (it.skip, describe.skip, it.skipIf, .todo, or left out by .only or -t) is a disabled
// test and does not keep its file from passing. The runner marks a test that skipped
// itself pending (on the task or its result).
const selfSkipped = (task) => Boolean(task && (task.pending || (task.result && task.result.pending)));

// Vitest 3+ (TestModule): a module that never started is pending or queued. Tests that
// did not run are pending (3.x) or skipped (4+, where cancelled tests also get the skip
// mode); a test declared to run (options.mode, taken at collection) that was skipped
// skipped itself, or was cancelled.
const moduleStatus = (m, failed, interrupted) => {
  if (failed) return 'fail';
  const state = typeof m.state === 'function' ? m.state() : undefined;
  if (state !== 'passed' && state !== 'skipped') return 'unknown';
  try {
    const tests = m.children && typeof m.children.allTests === 'function' ? m.children.allTests() : [];
    for (const t of tests) {
      const r = typeof t.result === 'function' ? t.result() : undefined;
      const s = r && r.state;
      if (s === 'pending') return 'unknown';
      const mode = t.options && t.options.mode;
      const declared = mode === 'skip' || mode === 'todo';
      if (s === 'skipped' && (!declared || selfSkipped(t.task))) return 'unknown';
      if (interrupted && s !== 'passed' && !declared) return 'unknown';
    }
  } catch {
    return 'unknown';
  }
  return 'pass';
};

// Older API (File): a file that never started has no result or no collected tasks
// (Vitest 1 reports it as skipped), and a test that did not get to run has no result
// while its mode is still run (declared and dynamic skips change the mode to skip; a
// dynamic one leaves the test pending).
const fileStatus = (f, failed) => {
  if (failed) return 'fail';
  const state = f.result && f.result.state;
  if ((state !== 'pass' && state !== 'skip') || !Array.isArray(f.tasks) || f.tasks.length === 0) return 'unknown';
  let finished = true;
  const walk = (tasks) => {
    for (const t of tasks || []) {
      if (t.tasks) walk(t.tasks);
      else if (selfSkipped(t)) finished = false;
      else if ((t.mode === 'run' || t.mode === 'only') && !(t.result && (t.result.state === 'pass' || t.result.state === 'skip'))) finished = false;
    }
  };
  walk(f.tasks);
  return finished ? 'pass' : 'unknown';
};

// Unhandled errors (an uncaught exception or rejection while a file ran, a failing
// teardown) do not fail a module: Vitest leaves it passed and fails only the run. It tags
// each error with the test file that was running (VITEST_TEST_PATH, the file's absolute
// path; taken relative to the root too, in case a version gives it so), and that file
// fails. An error naming no file of the run could have come from any of them: then no
// file passes.
const normPath = (p) => String(p).replace(/\\/g, '/');
const sameFile = (id, p) => {
  if (typeof id !== 'string' || !id) return false;
  id = normPath(id);
  p = normPath(p).replace(/^\.\//, '');
  return id === p || (!p.startsWith('/') && !/^[A-Za-z]:\//.test(p) && id.endsWith('/' + p));
};
// unhandled sorts errors by the entries (modules or files) they name: paths(entry) lists
// an entry's file paths. It returns the errors of each entry and whether any named none.
const unhandled = (errors, entries, paths) => {
  const byEntry = new Map();
  let unattributed = false;
  for (const e of Array.isArray(errors) ? errors : []) {
    let p;
    try {
      p = e && typeof e === 'object' ? e.VITEST_TEST_PATH : undefined;
    } catch {}
    const hits = typeof p === 'string' && p ? entries.filter((x) => paths(x).some((id) => sameFile(id, p))) : [];
    if (hits.length === 0) unattributed = true;
    for (const x of hits) byEntry.set(x, (byEntry.get(x) || []).concat([e]));
  }
  return { byEntry, unattributed };
};
// settle is an entry's status given the run's unhandled errors (the entry's own ones
// already fail it): a pass stands only when every error named a file.
const settle = (status, unattributed) => (status === 'pass' && unattributed ? 'unknown' : status);

// Global setup (globalSetup files, project.provide) runs in the main process between
// onTestRunStart and the first test module: whatever it reads affects every test, so
// that window is marked as a "global:" context for the trace shim.
const mark = (id) => {
  try {
    globalThis.__smatesTrace && globalThis.__smatesTrace.context(id);
  } catch {}
};

export default class SmatesReporter {
  constructor() {
    this.done = false;
    this.inGlobal = false;
    // Tells smates the reporter was accepted, even if no test file runs.
    try {
      writeFileSync(join(process.env.SMATES_TRACE_DIR || '.', `reporter-loaded-${process.pid}`), '');
    } catch {}
  }
  onTestRunStart() {
    this.inGlobal = true;
    mark('global:');
  }
  onInit() {
    if (!this.inGlobal) {
      this.inGlobal = true;
      mark('global:');
    }
  }
  endGlobal() {
    if (this.inGlobal) {
      this.inGlobal = false;
      mark('');
    }
  }
  onTestModuleQueued() {
    this.endGlobal();
  }
  onTestModuleCollected() {
    this.endGlobal();
  }
  onCollected() {
    this.endGlobal();
  }
  onTestRunEnd(testModules, unhandledErrors, reason) {
    this.endGlobal();
    if (!Array.isArray(testModules)) return;
    this.done = true;
    const { byEntry, unattributed } = unhandled(unhandledErrors, testModules, (m) => [m.moduleId, m.task && m.task.filepath]);
    for (const m of testModules) {
      const state = typeof m.state === 'function' ? m.state() : undefined;
      const diag = typeof m.diagnostic === 'function' ? m.diagnostic() : undefined;
      const errors = (typeof m.errors === 'function' ? m.errors() : []) || [];
      const all = errors.concat(byEntry.get(m) || []);
      const failed = state === 'failed' || all.length > 0;
      const status = settle(moduleStatus(m, failed, reason === 'interrupted'), unattributed);
      write(m.moduleId, status, diag && diag.duration, failed ? moduleMessage(m, all) : '');
    }
    flushLost();
  }
  onFinished(files, errors) {
    if (this.done || !Array.isArray(files)) return;
    const { byEntry, unattributed } = unhandled(errors, files, (f) => [f.filepath]);
    for (const f of files) {
      const state = f.result && f.result.state;
      const own = byEntry.get(f) || [];
      const failed = state === 'fail' || (f.result && f.result.errors && f.result.errors.length > 0) || own.length > 0;
      const msg = failed ? joinMsgs([fileMessage(f)].concat(own.map(errText))) : '';
      write(f.filepath, settle(fileStatus(f, failed), unattributed), f.result && f.result.duration, msg);
    }
    flushLost();
  }
}

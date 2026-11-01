// smates reporter for Vitest: writes per-file results to $SMATES_TRACE_DIR.
// Supports the Vitest 3+ API (onTestRunEnd) and the older onFinished(files).
import { appendFileSync, writeFileSync } from 'node:fs';
import { join } from 'node:path';

const out = () => join(process.env.SMATES_TRACE_DIR || '.', `results-vitest-${process.pid}.jsonl`);
const write = (id, failed, ms, msg) => {
  const ev = { k: 'result', id, status: failed ? 'fail' : 'pass', ms: Math.round(ms || 0) };
  if (failed && msg) ev.msg = msg;
  try {
    appendFileSync(out(), JSON.stringify(ev) + '\n');
  } catch {}
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
  onTestRunEnd(testModules) {
    this.endGlobal();
    if (!Array.isArray(testModules)) return;
    this.done = true;
    for (const m of testModules) {
      const state = typeof m.state === 'function' ? m.state() : undefined;
      const diag = typeof m.diagnostic === 'function' ? m.diagnostic() : undefined;
      const errors = typeof m.errors === 'function' ? m.errors() : [];
      const failed = state === 'failed' || (errors && errors.length > 0);
      write(m.moduleId, failed, diag && diag.duration, failed ? moduleMessage(m, errors) : '');
    }
  }
  onFinished(files) {
    if (this.done || !Array.isArray(files)) return;
    for (const f of files) {
      const state = f.result && f.result.state;
      const failed = state === 'fail' || (f.result && f.result.errors && f.result.errors.length > 0);
      write(f.filepath, failed, f.result && f.result.duration, failed ? fileMessage(f) : '');
    }
  }
}

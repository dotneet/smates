// smates reporter for Vitest: writes per-file results to $SMATES_TRACE_DIR.
// Supports the Vitest 3+ API (onTestRunEnd) and the older onFinished(files).
import { appendFileSync } from 'node:fs';
import { join } from 'node:path';

const out = () => join(process.env.SMATES_TRACE_DIR || '.', `results-vitest-${process.pid}.jsonl`);
const write = (id, failed, ms) => {
  try {
    appendFileSync(out(), JSON.stringify({ k: 'result', id, status: failed ? 'fail' : 'pass', ms: Math.round(ms || 0) }) + '\n');
  } catch {}
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
      write(m.moduleId, state === 'failed' || (errors && errors.length > 0), diag && diag.duration);
    }
  }
  onFinished(files) {
    if (this.done || !Array.isArray(files)) return;
    for (const f of files) {
      const state = f.result && f.result.state;
      write(f.filepath, state === 'fail' || (f.result && f.result.errors && f.result.errors.length > 0), f.result && f.result.duration);
    }
  }
}

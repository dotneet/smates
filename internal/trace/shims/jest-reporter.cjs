'use strict';
// smates reporter for Jest: writes per-file results to $SMATES_TRACE_DIR.
const fs = require('fs');
const path = require('path');

// Failure messages are bounded: they only feed smates' failure hints.
const MAX_MSG = 4000;
const clip = (s, n) => (s.length > n ? s.slice(0, n) + '\n[…]' : s);

function failureMessage(result) {
  try {
    if (result.failureMessage) return clip(String(result.failureMessage), MAX_MSG);
    if (result.testExecError) {
      const e = result.testExecError;
      return clip(String(e.stack || e.message || e), MAX_MSG);
    }
    const parts = [];
    for (const t of result.testResults || []) {
      if (t.status === 'failed') parts.push(t.fullName + ': ' + clip((t.failureMessages || []).join('\n'), 1500));
    }
    return clip(parts.join('\n\n'), MAX_MSG);
  } catch (_) {
    return '';
  }
}

// A result that cannot be written is counted, and the next line written says so
// ({"k":"lost"}): smates then records no pass from this file of results.
let lost = 0;
function append(line) {
  try {
    const out = path.join(process.env.SMATES_TRACE_DIR || '.', `results-jest-${process.pid}.jsonl`);
    fs.appendFileSync(out, (lost ? JSON.stringify({ k: 'lost', n: lost }) + '\n' : '') + line);
    lost = 0;
  } catch (_) {
    lost++;
  }
}

class SmatesReporter {
  constructor() {
    // Tells smates the reporter was accepted, even if no test file runs.
    try {
      fs.writeFileSync(path.join(process.env.SMATES_TRACE_DIR || '.', `reporter-loaded-${process.pid}`), '');
    } catch (_) {}
  }
  onTestResult(test, result) {
    const failed = result.numFailingTests > 0 || Boolean(result.testExecError) || Boolean(result.failureMessage && result.numPassingTests === 0 && result.numPendingTests === 0);
    const ms = result.perfStats ? result.perfStats.end - result.perfStats.start : 0;
    const ev = { k: 'result', id: result.testFilePath, status: failed ? 'fail' : 'pass', ms };
    if (failed) ev.msg = failureMessage(result);
    append(JSON.stringify(ev) + '\n');
  }
  // A run can fail for reasons no file's result shows: an error in global setup or
  // teardown (runExecError), obsolete snapshots (snapshot.failure). Jest still reported
  // the files passed; when another file failed too, smates would record those passes. A
  // file with obsolete snapshots is unknown; an error of the run itself, or a snapshot
  // failure no file accounts for (obsolete snapshot files), makes every file unknown (a
  // failure written before stands: smates keeps the worse status).
  onRunComplete(_contexts, results) {
    try {
      if (!results) return;
      const files = (results.testResults || []).filter((r) => r && r.testFilePath);
      const unknown = (r) => append(JSON.stringify({ k: 'result', id: r.testFilePath, status: 'unknown', ms: 0 }) + '\n');
      const snap = results.snapshot;
      if (results.runExecError) {
        files.forEach(unknown);
      } else if (snap && snap.failure) {
        // Files with snapshots that did not match failed already.
        const own = files.filter((r) => r.snapshot && (r.snapshot.unchecked > 0 || r.snapshot.fileDeleted || r.snapshot.unmatched > 0));
        if (own.length > 0 && !(snap.filesRemoved > 0)) own.forEach(unknown);
        else files.forEach(unknown);
      }
    } catch (_) {
      lost++;
    }
    if (lost) append('');
  }
}
module.exports = SmatesReporter;

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

class SmatesReporter {
  onTestResult(test, result) {
    const failed = result.numFailingTests > 0 || Boolean(result.testExecError) || Boolean(result.failureMessage && result.numPassingTests === 0 && result.numPendingTests === 0);
    const ms = result.perfStats ? result.perfStats.end - result.perfStats.start : 0;
    const ev = { k: 'result', id: result.testFilePath, status: failed ? 'fail' : 'pass', ms };
    if (failed) ev.msg = failureMessage(result);
    try {
      fs.appendFileSync(path.join(process.env.SMATES_TRACE_DIR || '.', `results-jest-${process.pid}.jsonl`), JSON.stringify(ev) + '\n');
    } catch (_) {}
  }
}
module.exports = SmatesReporter;

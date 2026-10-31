'use strict';
// smates reporter for Jest: writes per-file results to $SMATES_TRACE_DIR.
const fs = require('fs');
const path = require('path');

class SmatesReporter {
  onTestResult(test, result) {
    const failed = result.numFailingTests > 0 || Boolean(result.testExecError) || Boolean(result.failureMessage && result.numPassingTests === 0 && result.numPendingTests === 0);
    const ms = result.perfStats ? result.perfStats.end - result.perfStats.start : 0;
    try {
      fs.appendFileSync(
        path.join(process.env.SMATES_TRACE_DIR || '.', `results-jest-${process.pid}.jsonl`),
        JSON.stringify({ k: 'result', id: result.testFilePath, status: failed ? 'fail' : 'pass', ms }) + '\n',
      );
    } catch (_) {}
  }
}
module.exports = SmatesReporter;

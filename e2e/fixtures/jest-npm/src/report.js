'use strict';
// Reports are produced by a standalone script (also used by the nightly job).
const { execFileSync } = require('child_process');
const path = require('path');

const SCRIPT = path.join(__dirname, '..', 'scripts', 'build-report.js');

function dailyReport(orders) {
  const out = execFileSync(process.execPath, [SCRIPT], { input: JSON.stringify(orders), encoding: 'utf8' });
  return JSON.parse(out);
}

module.exports = { dailyReport };

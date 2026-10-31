'use strict';
// SKU normalisation is shared with the warehouse tooling, so it lives in shell scripts.
const { execFileSync } = require('child_process');
const path = require('path');

const SCRIPTS = path.join(__dirname, '..', 'scripts');

function runTool(tool, input) {
  return execFileSync('sh', [path.join(SCRIPTS, `${tool}.sh`)], { input, encoding: 'utf8' }).trim();
}

function normalize(raw) {
  return runTool('normalize-sku', raw);
}

module.exports = { normalize };

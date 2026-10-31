import { test } from 'node:test';
import assert from 'node:assert/strict';
import { execSync } from 'node:child_process';

test('stats CLI through a shell pipeline', () => {
  // A shell sits between this process and the node child.
  const out = execSync('node scripts/stats.js | tr a-z A-Z', { encoding: 'utf8' });
  assert.equal(out.trim(), 'COUNT=4 MEDIAN=4 MAX=9');
});

import { test } from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import { add } from '../src/math.js';

// The outcome depends on a file outside the repository (see README.md).
test('coin', () => {
  const p = process.env.SMATES_E2E_COIN;
  const v = p && fs.existsSync(p) ? fs.readFileSync(p, 'utf8') : '';
  if (v === 'fail-once') fs.writeFileSync(p, 'pass');
  assert.equal(add(2, 2), 4);
  assert.ok(!v.startsWith('fail'), `coin says ${v}`);
});

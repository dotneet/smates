import { test } from 'node:test';
import assert from 'node:assert/strict';

test('plain', () => {
  // Copying the environment is not a read of every variable.
  const copy = { ...process.env };
  assert.equal(typeof copy, 'object');
});

import { test } from 'node:test';
import assert from 'node:assert/strict';
import { add } from '../src/math.js';

test('adds', () => {
  assert.equal(add(1, 2), 3);
});

import { test } from 'node:test';
import assert from 'node:assert/strict';
import { getConfig } from '../src/config.js';

test('defaults', () => {
  assert.deepEqual(getConfig(), { port: 8080, currency: 'USD', retries: 3 });
});

test('overrides', () => {
  assert.equal(getConfig({ port: 1 }).port, 1);
});

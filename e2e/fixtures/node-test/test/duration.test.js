import { test } from 'node:test';
import assert from 'node:assert/strict';
import { humanize, parseTimeout } from '../src/duration.js';

test('humanize', () => {
  assert.equal(humanize(60000), '1 minute');
});

test('parse', () => {
  assert.equal(parseTimeout('2s'), 2000);
  assert.throws(() => parseTimeout('soon'));
});

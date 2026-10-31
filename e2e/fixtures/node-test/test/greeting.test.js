import { test } from 'node:test';
import assert from 'node:assert/strict';
import { greet } from '../src/greeting.js';

test('greets in english', () => {
  assert.equal(greet('Ann'), 'Hello, Ann');
});

test('unsupported locales fall back to english', () => {
  assert.equal(greet('Ann', 'fr'), 'Hello, Ann');
});

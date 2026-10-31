import { test } from 'node:test';
import assert from 'node:assert/strict';
import { formatCents, sumCents } from '../src/money.js';

test('formats cents', () => {
  assert.equal(formatCents(1250), '$12.50');
  assert.equal(formatCents(-5), '-$0.05');
  assert.equal(formatCents(100, 'EUR'), 'EUR 1.00');
});

test('sums', () => {
  assert.equal(sumCents([1, 2, 3]), 6);
});

import { test } from 'node:test';
import assert from 'node:assert/strict';
import { makeCart } from '../src/cart.js';

test('default region tax', () => {
  const cart = makeCart().add('book', 1000, 2);
  assert.equal(cart.subtotal(), 2000);
  assert.equal(cart.tax(), 200);
  assert.equal(cart.total(), 2200);
});

test('tax-free region', () => {
  assert.equal(makeCart('or').add('pen', 300).total(), 300);
});

test('receipt', () => {
  assert.deepEqual(makeCart().add('pen', 300).receipt(), ['1 x pen $3.00', 'TOTAL $3.30']);
});

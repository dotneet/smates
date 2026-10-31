import { test } from 'node:test';
import assert from 'node:assert/strict';
import { loadOrder, cartFromOrder } from './helpers/orders.js';

test('fixture order totals', () => {
  const cart = cartFromOrder(loadOrder('big-order'));
  assert.equal(cart.subtotal(), 2500);
  assert.equal(cart.total(), 2750);
});

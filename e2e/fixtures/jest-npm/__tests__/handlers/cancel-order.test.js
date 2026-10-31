'use strict';
const { dispatch } = require('../../src/handlers');

test('cancel-order releases the reserved stock', () => {
  const released = [];
  const inventory = { release: (sku, qty) => released.push([sku, qty]) };
  const res = dispatch('cancel-order', { id: 'C-1', lines: [{ sku: 'PEN', qty: 3 }] }, { inventory });
  expect(res).toEqual({ status: 'cancelled', id: 'C-1' });
  expect(released).toEqual([['PEN', 3]]);
});

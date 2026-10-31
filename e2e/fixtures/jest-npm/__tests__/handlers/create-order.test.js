'use strict';
const { dispatch } = require('../../src/handlers');
const { Inventory } = require('../../src/inventory');

test('create-order reserves stock and returns a quote', () => {
  const inventory = new Inventory({ MUG: 5 });
  const res = dispatch('create-order', { id: 'B-1', lines: [{ sku: 'MUG', unitCents: 1250, qty: 2 }] }, { inventory });
  expect(res).toMatchObject({ status: 'created', id: 'B-1', subtotal: 2500, total: 3495 });
  expect(inventory.available('MUG')).toBe(3);
});

test('unknown commands are rejected', () => {
  expect(() => dispatch('../config', {})).toThrow('unknown command');
});

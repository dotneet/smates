'use strict';
const fs = require('fs');
const path = require('path');
const { Inventory } = require('../src/inventory');

const stock = () => JSON.parse(fs.readFileSync(path.join(__dirname, '__fixtures__', 'stock.json'), 'utf8'));

test('reserve and release adjust availability', () => {
  const inv = new Inventory(stock());
  expect(inv.reserve('MUG', 4)).toBe(6);
  expect(inv.release('MUG', 1)).toBe(7);
});

test('reserve refuses to oversell', () => {
  const inv = new Inventory(stock());
  expect(() => inv.reserve('POSTER', 3)).toThrow('insufficient stock for POSTER');
});

test('lowStock lists SKUs at or below the threshold', () => {
  expect(new Inventory(stock()).lowStock(3)).toEqual(['CAP', 'POSTER']);
});

'use strict';
const fs = require('fs');
const path = require('path');
const { quote, shipping } = require('../src/pricing');

const orders = JSON.parse(fs.readFileSync(path.join(__dirname, '__fixtures__', 'orders.json'), 'utf8'));

describe.each(orders)('order $id', (order) => {
  test('total matches the recorded quote', () => {
    expect(quote(order.lines).total).toBe(order.expectedTotal);
  });
});

test('shipping is free above the threshold', () => {
  expect(shipping(10000)).toBe(0);
  expect(shipping(100)).toBe(495);
});

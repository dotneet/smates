'use strict';
const { toCents, format, sum } = require('../src/money');

test('toCents rounds to the nearest cent', () => {
  expect(toCents('12.345')).toBe(1235);
  expect(toCents(0.1 + 0.2)).toBe(30);
});

test('format renders sign, units and cents', () => {
  expect(format(123456)).toBe('1234.56 EUR');
  expect(format(-5, 'USD')).toBe('-0.05 USD');
});

test('sum adds cents', () => {
  expect(sum([1, 2, 3])).toBe(6);
});

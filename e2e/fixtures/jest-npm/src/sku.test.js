'use strict';
const { normalize } = require('./sku');

test.each([
  ['abc 1', 'ABC-1'],
  ['  mug_large. ', 'MUG-LARGE'],
  ['pen.blue', 'PEN-BLUE'],
])('normalize(%j) = %j', (raw, expected) => {
  expect(normalize(raw)).toBe(expected);
});

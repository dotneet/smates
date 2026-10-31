'use strict';
const { dispatch } = require('../../src/handlers');

test('refund produces a negative amount', () => {
  expect(dispatch('refund', { id: 'R-1', amountCents: 1999, currency: 'EUR' })).toEqual({
    status: 'refunded',
    id: 'R-1',
    amount: -1999,
    display: '-19.99 EUR',
  });
});

import { expect, it } from 'vitest';
import { quote } from '../src/orders.ts';

it('quotes an order with tax', async () => {
  expect(await quote([{ cents: 5000, qty: 2 }], 'DE', 'EUR')).toEqual({
    subtotal: '100.00 €',
    tax: '19.00 €',
    total: '119.00 €',
  });
});

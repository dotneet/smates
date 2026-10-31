import { expect, it } from 'vitest';
import { cartSummary, lineTotal } from './cart';

it('applies volume discounts like the backend', () => {
  expect(lineTotal({ sku: 'ABC-001', quantity: 10, unitCents: 100 })).toBe(950);
  expect(lineTotal({ sku: 'ABC-001', quantity: 100, unitCents: 100 })).toBe(8500);
});

it('summarises a cart', () => {
  expect(cartSummary([{ sku: 'FRU-001', quantity: 10, unitCents: 120 }, { sku: 'FRU-002', quantity: 1, unitCents: 80 }])).toBe(
    '2 item(s), $12.20',
  );
});

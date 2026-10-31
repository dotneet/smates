import { expect, it } from 'vitest';
import { loadSchema, validate } from './schema';

it('speaks the same contract version as the backend', () => {
  const s = loadSchema();
  expect([s.title, s.version]).toEqual(['Order', 3]);
});

it('validates checkout orders', () => {
  const s = loadSchema();
  expect(validate(s, { id: 'o1', sku: 'ABC-123', quantity: 2, unit_cents: 499 })).toEqual([]);
  expect(validate(s, { id: 'o1', sku: 'abc', quantity: 0, unit_cents: 499 })).toHaveLength(2);
});

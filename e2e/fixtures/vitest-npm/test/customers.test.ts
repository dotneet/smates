import { expect, inject, test } from 'vitest';
import { taxRate } from '@lib/tax';

const customers = inject('customers');

test('seeded customers are provided by global setup', () => {
  expect(customers.map((c) => c.name)).toEqual(['ACME GmbH', 'Kaisha KK', 'Globex Inc']);
});

test('every seeded customer has a known tax region', () => {
  for (const c of customers) expect(taxRate(c.region)).toBeGreaterThanOrEqual(0);
});

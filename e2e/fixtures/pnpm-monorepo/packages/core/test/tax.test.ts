import { expect, it } from 'vitest';
import { taxFor } from '../src/tax.ts';

it('computes tax by region', async () => {
  expect(await taxFor('DE', 10000)).toBe(1900);
  expect(await taxFor('JP', 10000)).toBe(1000);
  await expect(taxFor('XX', 1)).rejects.toThrow(/no tax rate/);
});

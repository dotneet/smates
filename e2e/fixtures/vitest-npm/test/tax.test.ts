import { expect, test } from 'vitest';
import { defaultRegion, taxFor, taxRate } from '@lib/tax';

test('rates come from data/tax-rates.yaml', () => {
  expect(taxRate('DE')).toBe(0.19);
  expect(taxRate('DE', 'reduced')).toBe(0.07);
  expect(taxRate('JP')).toBe(0.1);
});

test('default region comes from the test environment', () => {
  expect(defaultRegion()).toBe('DE');
  expect(taxFor(1000)).toBe(190);
});

test('unknown regions throw', () => {
  expect(() => taxRate('ZZ')).toThrow(/unknown tax region/);
});

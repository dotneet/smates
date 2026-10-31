import { expect, test } from 'vitest';
import { slugify } from '@lib/slug';

test('slugify', () => {
  expect(slugify('  Crème Brûlée & Co. ')).toBe('creme-brulee-co');
  expect(slugify('ACME GmbH')).toBe('acme-gmbh');
});

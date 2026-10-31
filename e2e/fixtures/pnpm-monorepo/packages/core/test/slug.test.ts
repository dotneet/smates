import { expect, it } from 'vitest';
import { slugify } from '../src/slug.ts';

it('slugifies', () => {
  expect(slugify('  Héllo, Wörld!  ')).toBe('hello-world');
  expect(slugify('Price: $5')).toBe('price-5');
});

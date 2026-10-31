import { expect, it } from 'vitest';
import { apply, discover } from './plugins';

it('discovers exactly the registered plugins', () => {
  expect(discover().map((p) => p.name)).toEqual(['bold', 'shout']);
});

it('applies plugins in order', () => {
  expect(apply(discover(), 'sale')).toBe('**SALE**');
});

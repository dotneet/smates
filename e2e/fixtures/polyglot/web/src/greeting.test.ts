import { expect, it } from 'vitest';
import { greet, type Shopper } from './greeting';

it('greets the demo shopper loaded by the setup file', () => {
  const shopper = (globalThis as any).__SHOPPER__ as Shopper;
  expect(greet(shopper)).toBe('Welcome back, Ada! Prices in USD.');
});

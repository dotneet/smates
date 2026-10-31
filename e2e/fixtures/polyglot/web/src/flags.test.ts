import { expect, it } from 'vitest';
import { loadFlags } from './flags';

it('ships with the new checkout disabled', () => {
  expect(loadFlags()).toEqual({ newCheckout: false, giftCards: true });
});

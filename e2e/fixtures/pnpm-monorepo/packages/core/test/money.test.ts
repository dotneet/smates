import { describe, expect, it } from 'vitest';
import { formatMoney, knownCurrencies } from '../src/money.ts';

describe('formatMoney', () => {
  it('formats prefix and suffix currencies', () => {
    expect(formatMoney(12345, 'USD')).toBe('$123.45');
    expect(formatMoney(12345, 'EUR')).toBe('123.45 €');
    expect(formatMoney(12300, 'JPY')).toBe('¥123');
  });
  it('lists currencies', () => {
    expect(knownCurrencies()).toEqual(['EUR', 'JPY', 'USD']);
  });
});

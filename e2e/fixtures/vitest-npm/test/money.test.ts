import { describe, expect, it } from 'vitest';
import { currencyInfo, defaultCurrency, formatMoney } from '@lib/money';

describe('money', () => {
  it('uses the default currency from the test environment', () => {
    expect(defaultCurrency()).toBe('EUR');
    expect(formatMoney(123456)).toBe('1234.56 €');
  });
  it('formats prefix currencies', () => {
    expect(formatMoney(999, 'USD')).toBe('$9.99');
    expect(formatMoney(150000, 'JPY')).toBe('¥1500');
  });
  it('rejects unknown currencies', () => {
    expect(() => currencyInfo('XXX')).toThrow(/unknown currency/);
  });
});

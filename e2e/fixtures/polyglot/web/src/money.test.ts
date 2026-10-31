import { describe, expect, it } from 'vitest';
import { formatCents, percent } from './money';

describe('money', () => {
  it('formats cents', () => {
    expect(formatCents(0)).toBe('$0.00');
    expect(formatCents(1234)).toBe('$12.34');
    expect(formatCents(-250)).toBe('-$2.50');
  });
  it('rounds percentages half up', () => {
    expect(percent(1999, 10)).toBe(200);
  });
});

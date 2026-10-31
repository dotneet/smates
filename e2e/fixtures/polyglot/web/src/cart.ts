import { formatCents, percent, type Cents } from './money';

export interface Line {
  sku: string;
  quantity: number;
  unitCents: Cents;
}

export function discountPercent(qty: number): number {
  if (qty >= 100) return 15;
  if (qty >= 10) return 5;
  return 0;
}

export function lineTotal(l: Line): Cents {
  const gross = l.quantity * l.unitCents;
  return gross - percent(gross, discountPercent(l.quantity));
}

export function cartSummary(lines: Line[]): string {
  const total = lines.reduce((s, l) => s + lineTotal(l), 0);
  return `${lines.length} item(s), ${formatCents(total)}`;
}

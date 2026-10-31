import { formatMoney } from '@fx/core/money';
import { taxFor } from '@fx/core/tax';

export type Line = { cents: number; qty: number };

export async function quote(lines: Line[], region: string, currency: string) {
  const subtotal = lines.reduce((s, l) => s + l.cents * l.qty, 0);
  const tax = await taxFor(region, subtotal);
  return { subtotal: formatMoney(subtotal, currency), tax: formatMoney(tax, currency), total: formatMoney(subtotal + tax, currency) };
}

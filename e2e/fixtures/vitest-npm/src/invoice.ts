import { formatMoney } from '@lib/money';
import { taxFor } from './tax';

export type LineItem = { sku: string; qty: number; unitCents: number; reduced?: boolean };
export type Invoice = { id: string; customer: string; currency: string; region: string; items: LineItem[] };
export type Totals = { subtotal: number; tax: number; total: number };

export function totals(inv: Invoice): Totals {
  let subtotal = 0;
  let tax = 0;
  for (const it of inv.items) {
    const line = it.qty * it.unitCents;
    subtotal += line;
    tax += taxFor(line, inv.region, it.reduced ? 'reduced' : 'standard');
  }
  return { subtotal, tax, total: subtotal + tax };
}

export function renderInvoice(inv: Invoice): string {
  const t = totals(inv);
  const lines = inv.items.map((it) => `${it.sku} x${it.qty} ${formatMoney(it.qty * it.unitCents, inv.currency)}`);
  return [
    `Invoice ${inv.id} for ${inv.customer}`,
    ...lines,
    `Subtotal ${formatMoney(t.subtotal, inv.currency)}`,
    `Tax ${formatMoney(t.tax, inv.currency)}`,
    `Total ${formatMoney(t.total, inv.currency)}`,
  ].join('\n');
}

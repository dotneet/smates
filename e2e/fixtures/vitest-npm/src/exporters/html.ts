import { formatMoney } from '@lib/money';
import { totals, type Invoice } from '@lib/invoice';

const esc = (s: string) => s.replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;');

export default function html(inv: Invoice): string {
  const t = totals(inv);
  const rows = inv.items.map((it) => `<tr><td>${esc(it.sku)}</td><td>${it.qty}</td></tr>`).join('');
  return `<section class="invoice"><h1>Invoice ${esc(inv.id)}</h1><table>${rows}</table><p class="total">${formatMoney(t.total, inv.currency)}</p></section>`;
}

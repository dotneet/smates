import { formatMoney } from '@lib/money';
import { totals, type Invoice } from '@lib/invoice';

export default function markdown(inv: Invoice): string {
  const t = totals(inv);
  const rows = inv.items.map((it) => `| ${it.sku} | ${it.qty} | ${formatMoney(it.qty * it.unitCents, inv.currency)} |`);
  return [`# Invoice ${inv.id}`, '', '| SKU | Qty | Amount |', '|---|---:|---:|', ...rows, '', `**Total:** ${formatMoney(t.total, inv.currency)}`].join('\n');
}

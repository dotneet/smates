import { loadCatalog } from "./catalog";
import { loadDiscounts } from "./discounts";
import { formatCents } from "./money";
import { taxFor } from "./tax";
import type { Line } from "./types";

export interface Priced {
  lines: Line[];
  subtotal: number;
  discount: number;
  tax: number;
  total: number;
  applied: string[];
}

export async function priceCart(items: { sku: string; qty: number }[], region: string): Promise<Priced> {
  const catalog = loadCatalog();
  const discounts = await loadDiscounts();
  const lines: Line[] = items.map(({ sku, qty }) => {
    const p = catalog.get(sku);
    if (!p) throw new Error(`unknown sku ${sku}`);
    return { sku, name: p.name, category: p.category, qty, unitCents: p.priceCents };
  });
  let subtotal = 0;
  let discount = 0;
  let tax = 0;
  const applied = new Set<string>();
  for (const l of lines) {
    const gross = l.unitCents * l.qty;
    let off = 0;
    for (const d of discounts) {
      if (d.applies(l)) {
        off += d.amount(l);
        applied.add(d.id);
      }
    }
    subtotal += gross;
    discount += off;
    tax += taxFor(gross - off, region, l.category === "food");
  }
  return { lines, subtotal, discount, tax, total: subtotal - discount + tax, applied: [...applied].sort() };
}

export function summary(p: Priced, currency = "USD"): string {
  return `subtotal ${formatCents(p.subtotal, currency)}, discount ${formatCents(p.discount, currency)}, tax ${formatCents(p.tax, currency)}, total ${formatCents(p.total, currency)}`;
}

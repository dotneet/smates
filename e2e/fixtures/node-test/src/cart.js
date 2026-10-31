import { readFileSync } from 'node:fs';
import { formatCents, sumCents } from './money.js';
import { projectPath } from './paths.js';

let rates;
function taxRates() {
  // Data dependency read at runtime: data/tax-rates.json
  rates ??= JSON.parse(readFileSync(projectPath('data', 'tax-rates.json'), 'utf8'));
  return rates;
}

export function makeCart(region = 'default') {
  const items = [];
  return {
    add(name, cents, qty = 1) {
      items.push({ name, cents, qty });
      return this;
    },
    subtotal() {
      return sumCents(items.map((i) => i.cents * i.qty));
    },
    tax() {
      const rate = taxRates()[region] ?? taxRates().default;
      return Math.round(this.subtotal() * rate);
    },
    total() {
      return this.subtotal() + this.tax();
    },
    receipt() {
      return items.map((i) => `${i.qty} x ${i.name} ${formatCents(i.cents)}`).concat(`TOTAL ${formatCents(this.total())}`);
    },
  };
}

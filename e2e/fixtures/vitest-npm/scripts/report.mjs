#!/usr/bin/env node
// Prints a one-line summary of an invoice JSON file: node scripts/report.mjs <invoice.json>
import { readFileSync } from 'node:fs';

const root = new URL('..', import.meta.url);
const cfg = JSON.parse(readFileSync(new URL('config/report.json', root), 'utf8'));
const file = process.argv[2];
if (!file) {
  console.error('usage: report.mjs <invoice.json>');
  process.exit(2);
}
const inv = JSON.parse(readFileSync(file, 'utf8'));
const subtotal = inv.items.reduce((s, it) => s + it.qty * it.unitCents, 0);
const who = cfg.showCustomer ? ` ${inv.customer}` : '';
console.log(`${cfg.title}: ${inv.id}${who} subtotal=${(subtotal / 100).toFixed(cfg.currencyDecimals)}`);

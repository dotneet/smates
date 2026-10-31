import { readFileSync } from 'node:fs';

export type Currency = { symbol: string; decimals: number; position: 'prefix' | 'suffix' };

// Loaded next to this module (works from src/ and from a bundled dist/).
const currencies: Record<string, Currency> = JSON.parse(
  readFileSync(new URL('./currencies.json', import.meta.url), 'utf8'),
);

export function defaultCurrency(): string {
  return process.env.APP_CURRENCY ?? 'USD';
}

export function currencyInfo(code: string): Currency {
  const c = currencies[code];
  if (!c) throw new Error(`unknown currency ${code}`);
  return c;
}

export function formatMoney(cents: number, currency: string = defaultCurrency()): string {
  const c = currencyInfo(currency);
  const amount = (cents / 100).toFixed(c.decimals);
  return c.position === 'prefix' ? `${c.symbol}${amount}` : `${amount} ${c.symbol}`;
}

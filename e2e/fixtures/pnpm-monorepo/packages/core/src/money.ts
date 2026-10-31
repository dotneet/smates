import { readFileSync } from 'node:fs';

type Currency = { symbol: string; decimals: number; position: 'prefix' | 'suffix' };

let table: Record<string, Currency> | undefined;

// The currency table is data, read lazily at runtime (not imported), so importing this
// module alone does not touch it.
function currencies(): Record<string, Currency> {
  table ??= JSON.parse(readFileSync(new URL('../data/currencies.json', import.meta.url), 'utf8'));
  return table!;
}

export function formatMoney(cents: number, code: string): string {
  const c = currencies()[code];
  if (!c) throw new Error(`unknown currency ${code}`);
  const amount = (cents / 100).toFixed(c.decimals);
  return c.position === 'prefix' ? `${c.symbol}${amount}` : `${amount} ${c.symbol}`;
}

export function knownCurrencies(): string[] {
  return Object.keys(currencies()).sort();
}

import { readFile } from 'node:fs/promises';

let rates: Promise<Record<string, number>> | undefined;

function load(): Promise<Record<string, number>> {
  rates ??= readFile(new URL('../data/tax-rates.json', import.meta.url), 'utf8').then((s) => JSON.parse(s));
  return rates;
}

export async function taxFor(region: string, cents: number): Promise<number> {
  const r = (await load())[region];
  if (r === undefined) throw new Error(`no tax rate for ${region}`);
  return Math.round(cents * r);
}

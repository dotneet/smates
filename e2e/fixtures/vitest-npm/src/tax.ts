import { readFileSync } from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import { parse } from 'yaml';

type Rates = { standard: number; reduced: number };

const dataDir = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..', 'data');
let regions: Record<string, Rates> | undefined;

function load(): Record<string, Rates> {
  regions ??= parse(readFileSync(path.join(dataDir, 'tax-rates.yaml'), 'utf8')).regions as Record<string, Rates>;
  return regions;
}

export function defaultRegion(): string {
  return process.env.TAX_REGION ?? 'US';
}

export function taxRate(region: string = defaultRegion(), kind: keyof Rates = 'standard'): number {
  const r = load()[region];
  if (!r) throw new Error(`unknown tax region ${region}`);
  return r[kind];
}

export function taxFor(cents: number, region?: string, kind: keyof Rates = 'standard'): number {
  return Math.round(cents * taxRate(region, kind));
}

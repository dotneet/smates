// Tax rates live in a JSON data file read with the default `fs` import.
import fs from "node:fs";
import { join } from "node:path";
import { percentOf } from "./money";

type Rates = Record<string, { standard: number; reduced?: number }>;

const RATES_FILE = join(import.meta.dir, "..", "data", "tax-rates.json");
let cache: Rates | undefined;

function rates(): Rates {
  return (cache ??= JSON.parse(fs.readFileSync(RATES_FILE, "utf8")) as Rates);
}

export function regions(): string[] {
  return Object.keys(rates()).sort();
}

export function taxRate(region: string, reduced = false): number {
  const r = rates()[region];
  if (!r) throw new Error(`unknown tax region ${region}`);
  return reduced && r.reduced !== undefined ? r.reduced : r.standard;
}

export function taxFor(cents: number, region: string, reduced = false): number {
  return percentOf(cents, taxRate(region, reduced));
}

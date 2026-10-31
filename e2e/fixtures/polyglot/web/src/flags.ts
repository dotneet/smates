// Feature flags: config/flags.json, overlaid by config/flags.<mode>.json when present.
import { existsSync, readFileSync } from 'node:fs';

export type Flags = Record<string, boolean>;

export function loadFlags(mode: string = process.env.APP_MODE ?? 'test'): Flags {
  const base: Flags = JSON.parse(readFileSync(new URL('../config/flags.json', import.meta.url), 'utf8'));
  const overlay = new URL(`../config/flags.${mode}.json`, import.meta.url);
  if (existsSync(overlay)) Object.assign(base, JSON.parse(readFileSync(overlay, 'utf8')));
  return base;
}

// The order contract shared with the Go backend and the Python tools.
import { readFileSync } from 'node:fs';

export interface OrderSchema {
  title: string;
  version: number;
  required: string[];
  properties: Record<string, { type: string; pattern?: string; minimum?: number }>;
}

export function loadSchema(): OrderSchema {
  return JSON.parse(readFileSync(new URL('../../shared/schema.json', import.meta.url), 'utf8'));
}

export function validate(schema: OrderSchema, order: Record<string, unknown>): string[] {
  const errors: string[] = [];
  for (const k of schema.required) if (!(k in order)) errors.push(`missing ${k}`);
  for (const [k, v] of Object.entries(order)) {
    const p = schema.properties[k];
    if (!p) {
      errors.push(`unknown ${k}`);
      continue;
    }
    if (p.type === 'string' && typeof v !== 'string') errors.push(`${k}: want string`);
    if (p.type === 'integer' && !Number.isInteger(v)) errors.push(`${k}: want integer`);
    if (p.pattern && typeof v === 'string' && !new RegExp(p.pattern).test(v)) errors.push(`${k}: bad format`);
    if (p.minimum !== undefined && typeof v === 'number' && v < p.minimum) errors.push(`${k}: too small`);
  }
  return errors;
}

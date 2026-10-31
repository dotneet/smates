// Text transforms are discovered from JSON descriptors dropped into src/plugins/.
import { readdirSync, readFileSync } from 'node:fs';

export interface Plugin {
  name: string;
  op: 'upper' | 'wrap';
  arg?: string;
}

const dir = new URL('./plugins/', import.meta.url);

export function discover(): Plugin[] {
  return readdirSync(dir)
    .filter((f) => f.endsWith('.json'))
    .sort()
    .map((f) => JSON.parse(readFileSync(new URL(f, dir), 'utf8')) as Plugin);
}

export function apply(plugins: Plugin[], text: string): string {
  return plugins.reduce((s, p) => (p.op === 'upper' ? s.toUpperCase() : `${p.arg}${s}${p.arg}`), text);
}

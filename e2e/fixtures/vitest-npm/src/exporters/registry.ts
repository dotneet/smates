import path from 'node:path';
import { fileURLToPath, pathToFileURL } from 'node:url';
import type { Invoice } from '@lib/invoice';

export type Exporter = (inv: Invoice) => string;

const exportersDir = path.dirname(fileURLToPath(import.meta.url));

// Exporters are resolved at runtime from a computed file path, so third parties can drop
// new formats into this directory.
export async function loadExporter(kind: string): Promise<Exporter> {
  const file = path.join(exportersDir, `${kind.toLowerCase()}.ts`);
  const mod = await import(/* @vite-ignore */ pathToFileURL(file).href);
  return mod.default as Exporter;
}

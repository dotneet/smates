// Locale bundles are JSON modules loaded by computed dynamic import; the available
// locales are discovered with Bun.Glob.
import { join } from "node:path";

const LOCALES = join(import.meta.dir, "..", "locales");

export function availableLocales(): string[] {
  return [...new Bun.Glob("*.json").scanSync(LOCALES)].map((f) => f.replace(/\.json$/, "")).sort();
}

export async function messages(locale: string): Promise<Record<string, string>> {
  const mod = await import(join(LOCALES, `${locale}.json`));
  return mod.default as Record<string, string>;
}

export async function t(locale: string, key: string, vars: Record<string, string | number> = {}): Promise<string> {
  const msg = (await messages(locale))[key];
  if (msg === undefined) throw new Error(`missing message ${locale}:${key}`);
  return msg.replace(/\{(\w+)\}/g, (_, k) => String(vars[k] ?? `{${k}}`));
}

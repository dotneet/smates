import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';

const localesDir = join(dirname(fileURLToPath(import.meta.url)), 'locales');

// Locale modules are loaded by computed path: no static import names them.
export async function messages(lang: string): Promise<Record<string, string>> {
  const file = join(localesDir, lang + '.ts');
  const mod = await import(/* @vite-ignore */ file);
  return mod.default;
}

export async function t(lang: string, key: string): Promise<string> {
  return (await messages(lang))[key] ?? key;
}

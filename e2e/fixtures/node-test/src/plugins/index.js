import { readdirSync } from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

const dir = path.dirname(fileURLToPath(import.meta.url));

// Every *.js file next to this one (except index.js) is a plugin.
export function listPlugins() {
  return readdirSync(dir)
    .filter((f) => f.endsWith('.js') && f !== 'index.js')
    .map((f) => f.slice(0, -3))
    .sort();
}

// Plugins are loaded with a computed specifier: invisible to static import analysis.
export async function loadPlugin(name) {
  const mod = await import(`./${name}.js`);
  return mod.default;
}

export async function applyAll(input) {
  let out = input;
  for (const name of listPlugins()) {
    out = (await loadPlugin(name))(out);
  }
  return out;
}

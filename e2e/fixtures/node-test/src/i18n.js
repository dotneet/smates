import { existsSync, readFileSync, readdirSync } from 'node:fs';
import { projectPath } from './paths.js';

const dir = projectPath('locales');

export function availableLocales() {
  return readdirSync(dir)
    .filter((f) => f.endsWith('.json'))
    .map((f) => f.slice(0, -5))
    .sort();
}

// Unknown locales fall back to English.
export function messages(lang) {
  const file = projectPath('locales', `${lang}.json`);
  const chosen = existsSync(file) ? file : projectPath('locales', 'en.json');
  return JSON.parse(readFileSync(chosen, 'utf8'));
}

export function t(lang, key, vars = {}) {
  const msg = messages(lang)[key] ?? key;
  return msg.replace(/\{(\w+)\}/g, (_, k) => String(vars[k] ?? ''));
}

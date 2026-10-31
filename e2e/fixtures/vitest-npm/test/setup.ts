// setupFiles: runs in every test worker before each test file.
import { readFileSync } from 'node:fs';
import path from 'node:path';

const defaults: Record<string, string> = JSON.parse(
  readFileSync(path.resolve(process.cwd(), 'test', 'env-defaults.json'), 'utf8'),
);
for (const [k, v] of Object.entries(defaults)) {
  process.env[k] ??= v;
}

import path from 'node:path';
import { fileURLToPath } from 'node:url';

// Project root, computed at runtime (static analysis cannot follow joins on it).
export const ROOT = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..');

export function projectPath(...parts) {
  return path.join(ROOT, ...parts);
}

import { execFileSync } from 'node:child_process';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';

const root = join(dirname(fileURLToPath(import.meta.url)), '..');

// Runs one of the package's shell scripts (scripts/<name>.sh) and returns its output.
export function runScript(name: string): string {
  return execFileSync('sh', [join(root, 'scripts', `${name}.sh`)], { encoding: 'utf8' });
}

// Runs one of the package's node scripts (scripts/<name>.mjs) and returns its output.
export function runNode(name: string): string {
  return execFileSync(process.execPath, [join(root, 'scripts', `${name}.mjs`)], { encoding: 'utf8' });
}

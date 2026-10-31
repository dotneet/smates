import { spawnSync } from 'node:child_process';
import { fileURLToPath } from 'node:url';
import { expect, it } from 'vitest';

it('the build step emits an installable manifest', () => {
  const script = fileURLToPath(new URL('../scripts/manifest.mjs', import.meta.url));
  const r = spawnSync(process.execPath, [script], { encoding: 'utf8' });
  expect(r.status, r.stderr).toBe(0);
  const m = JSON.parse(r.stdout);
  expect(m).toMatchObject({ name: 'Polyglot Store', display: 'standalone', start_url: '/' });
});

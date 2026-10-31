import { createRequire } from 'node:module';

const require = createRequire(import.meta.url);

// Reports the version of the workspace dependency (resolved through node_modules/@fx/core,
// a pnpm symlink to packages/core).
export default function health() {
  const core = require('@fx/core/package.json') as { version: string };
  return { ok: true, core: core.version };
}

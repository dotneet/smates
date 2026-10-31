import { createRequire } from 'node:module';

const require = createRequire(import.meta.url);
const configFile = process.env.APP_CONFIG ?? ['..', 'config', 'defaults.json'].join('/');

// JSON loaded through CommonJS require from an ESM module.
const defaults = require(configFile);

export function getConfig(overrides = {}) {
  return { ...defaults, ...overrides };
}

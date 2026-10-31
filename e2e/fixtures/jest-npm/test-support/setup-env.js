'use strict';
// Loaded by jest (setupFiles) before every test file.
process.env.APP_ENV = 'test';
process.env.TZ = 'UTC';
global.FIXED_NOW = Date.UTC(2026, 0, 15, 12, 0, 0);

'use strict';
const config = require('../src/config');

test('config is loaded from config/app.json and frozen', () => {
  expect(config.currency).toBe('EUR');
  expect(Object.isFrozen(config)).toBe(true);
});

test('setup file pinned the environment', () => {
  expect(process.env.APP_ENV).toBe('test');
  expect(new Date(global.FIXED_NOW).toISOString()).toBe('2026-01-15T12:00:00.000Z');
});

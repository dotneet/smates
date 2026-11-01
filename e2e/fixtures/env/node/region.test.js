import { test } from 'node:test';
import assert from 'node:assert/strict';
import { region } from './lib/region.js';

test('region', () => {
  assert.notEqual(region(), 'invalid');
});

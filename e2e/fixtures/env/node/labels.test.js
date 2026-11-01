import { test } from 'node:test';
import assert from 'node:assert/strict';

test('labels', () => {
  assert.equal(typeof (process.env.BUILD_LABEL ?? ''), 'string');
  assert.equal(typeof (process.env.GITHUB_RUN_ID ?? ''), 'string');
});

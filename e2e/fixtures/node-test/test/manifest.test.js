import { test } from 'node:test';
import assert from 'node:assert/strict';
import { execFileSync } from 'node:child_process';

test('release manifest', () => {
  const out = execFileSync('sh', ['scripts/build-manifest.sh'], { encoding: 'utf8' });
  assert.deepEqual(out.trim().split('\n'), ['MANIFEST v1', 'name=node-test-fixture']);
});

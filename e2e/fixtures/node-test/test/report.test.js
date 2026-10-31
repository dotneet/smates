import { test } from 'node:test';
import assert from 'node:assert/strict';
import { execFileSync } from 'node:child_process';
import { fileURLToPath } from 'node:url';

const script = fileURLToPath(new URL('../scripts/report.js', import.meta.url));

test('report CLI prints a price table', () => {
  const out = execFileSync(process.execPath, [script], { encoding: 'utf8' });
  assert.equal(out, 'SKU          PRICE\nA-100       $12.50\nB-200        $0.99\n');
});

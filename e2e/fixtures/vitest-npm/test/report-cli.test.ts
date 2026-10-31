import { spawnSync } from 'node:child_process';
import { expect, test } from 'vitest';

test('report CLI prints a summary line', () => {
  const r = spawnSync(process.execPath, ['scripts/report.mjs', 'test/fixtures/invoices/a.json'], { encoding: 'utf8' });
  expect(r.stderr).toBe('');
  expect(r.status).toBe(0);
  expect(r.stdout.trim()).toBe('Monthly report: INV-A ACME GmbH subtotal=30.00');
});

test('report CLI needs an argument', () => {
  const r = spawnSync(process.execPath, ['scripts/report.mjs'], { encoding: 'utf8' });
  expect(r.status).toBe(2);
});

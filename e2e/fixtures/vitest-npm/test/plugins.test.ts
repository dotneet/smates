import { expect, test } from 'vitest';
import { loadPlugin } from '@lib/plugins/index';

const rows = [
  { sku: 'WIDGET', qty: 2, amountCents: 2000 },
  { sku: 'say "hi"', qty: 1, amountCents: 50 },
];

test('csv plugin', async () => {
  const csv = await loadPlugin('csv');
  expect(csv.contentType).toBe('text/csv');
  expect(csv.serialize(rows)).toBe('sku,qty,amountCents\nWIDGET,2,$20.00\n"say ""hi""",1,$0.50');
});

test('json plugin', async () => {
  const json = await loadPlugin('json');
  expect(JSON.parse(json.serialize(rows))).toEqual(rows);
});

test('unknown plugins fail to load', async () => {
  await expect(loadPlugin('xml')).rejects.toThrow();
});

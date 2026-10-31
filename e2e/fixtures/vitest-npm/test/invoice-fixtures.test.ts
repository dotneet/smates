// Golden files: every invoice in fixtures/invoices must total to its expectedTotalCents.
import { readdirSync, readFileSync } from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import { describe, expect, test } from 'vitest';
import { totals, type Invoice } from '@lib/invoice';

const dir = path.join(path.dirname(fileURLToPath(import.meta.url)), 'fixtures', 'invoices');
const files = readdirSync(dir).filter((f) => f.endsWith('.json')).sort();

describe('invoice fixtures', () => {
  test('there are fixtures', () => {
    expect(files.length).toBeGreaterThan(0);
  });
  for (const f of files) {
    test(f, () => {
      const inv = JSON.parse(readFileSync(path.join(dir, f), 'utf8')) as Invoice & { expectedTotalCents: number };
      expect(totals(inv).total).toBe(inv.expectedTotalCents);
    });
  }
});

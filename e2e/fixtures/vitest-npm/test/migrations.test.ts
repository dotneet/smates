// Migration files must be numbered consecutively: NNN_snake_case.sql
import { readdirSync } from 'node:fs';
import { expect, test } from 'vitest';

test('migrations are numbered consecutively', () => {
  const files = readdirSync('db/migrations').sort();
  expect(files.length).toBeGreaterThan(0);
  files.forEach((f, i) => {
    expect(f).toMatch(/^\d{3}_[a-z_]+\.sql$/);
    expect(Number(f.slice(0, 3))).toBe(i + 1);
  });
});

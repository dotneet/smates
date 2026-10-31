// Validates the seed data itself (shape, unique ids and e-mails).
import { readFileSync } from 'node:fs';
import { expect, test } from 'vitest';

const seed = JSON.parse(readFileSync('test/seed/customers.json', 'utf8')) as Array<Record<string, unknown>>;

test('seed customers have unique ids and e-mails', () => {
  expect(new Set(seed.map((c) => c.id)).size).toBe(seed.length);
  expect(new Set(seed.map((c) => c.email)).size).toBe(seed.length);
});

test('seed customers have the required fields', () => {
  for (const c of seed) {
    expect(Object.keys(c).sort()).toEqual(['email', 'id', 'name', 'region']);
    expect(String(c.email)).toMatch(/^[^@\s]+@[^@\s]+$/);
  }
});

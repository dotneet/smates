import { execFileSync } from 'node:child_process';
import { expect, test } from 'vitest';

test('build-schema.sh concatenates the migrations in order', () => {
  const out = execFileSync('sh', ['scripts/build-schema.sh'], { encoding: 'utf8' });
  const lines = out.split('\n');
  expect(lines[0]).toBe('-- generated schema (do not edit)');
  expect(lines[1]).toBe('PRAGMA foreign_keys = ON;');
  expect(out).toMatch(/CREATE TABLE customers \(/);
  expect(out).toMatch(/ALTER TABLE customers ADD COLUMN email TEXT;/);
  expect(out).toMatch(/CREATE TABLE invoices \(/);
  expect(out.indexOf('CREATE TABLE customers')).toBeLessThan(out.indexOf('ALTER TABLE customers'));
});

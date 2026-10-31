import { expect, it } from 'vitest';
import { runScript } from './helpers.ts';

it('content passes the copy lint', () => {
  expect(runScript('lint-copy')).toContain('copy ok');
});

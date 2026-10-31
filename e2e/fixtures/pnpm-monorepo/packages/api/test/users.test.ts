import { expect, it } from 'vitest';
import { userHandle } from '../src/users.ts';

it('builds handles', () => {
  expect(userHandle('Ada Lovelace', 7)).toBe('ada-lovelace-7');
});

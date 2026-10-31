import { expect, it } from 'vitest';
import { navItems, type Session } from '../src/nav.ts';

it('shows admin to editors (session comes from test/setup.ts)', () => {
  const session = (globalThis as any).__session as Session;
  expect(navItems(session)).toEqual(['home', 'pricing', 'admin']);
});

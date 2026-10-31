import { expect, it } from 'vitest';
import { loadMessages, t } from './i18n';

it('loads the Japanese bundle on demand', async () => {
  const ja = await loadMessages('ja');
  expect(t(ja, 'checkout')).toBe('レジに進む');
  expect(t(ja, 'items', { count: '3' })).toBe('3点');
});

it('falls back to the key', async () => {
  const en = await loadMessages('en');
  expect(t(en, 'missing.key')).toBe('missing.key');
});

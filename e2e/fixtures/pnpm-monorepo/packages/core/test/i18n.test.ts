import { expect, it } from 'vitest';
import { t } from '../src/i18n.ts';

it('translates', async () => {
  expect(await t('en', 'greeting')).toBe('Hello');
  expect(await t('ja', 'checkout')).toBe('お会計');
  expect(await t('ja', 'missing')).toBe('missing');
});

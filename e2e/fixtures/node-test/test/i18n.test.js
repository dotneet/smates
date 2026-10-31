import { test } from 'node:test';
import assert from 'node:assert/strict';
import { availableLocales, t } from '../src/i18n.js';

test('lists locales from the locales directory', () => {
  assert.deepEqual(availableLocales(), ['en', 'ja']);
});

test('japanese messages', () => {
  assert.equal(t('ja', 'bye'), 'さようなら');
});

import { expect, it } from 'vitest';
import { runNode } from './helpers.ts';

it('builds the sitemap from content pages', () => {
  expect(JSON.parse(runNode('build-sitemap'))).toEqual([
    { path: '/about', title: 'About us' },
    { path: '/pricing', title: 'Pricing' },
  ]);
});

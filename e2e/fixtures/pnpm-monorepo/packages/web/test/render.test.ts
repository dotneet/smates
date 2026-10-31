import { expect, it } from 'vitest';
import { escapeHtml, link } from '../src/render.ts';

it('escapes html', () => {
  expect(escapeHtml(`<b>"Tom" & 'Jerry'</b>`)).toBe('&lt;b&gt;&quot;Tom&quot; &amp; &#39;Jerry&#39;&lt;/b&gt;');
  expect(link('/a?x=1&y=2', 'A & B')).toBe('<a href="/a?x=1&amp;y=2">A &amp; B</a>');
});

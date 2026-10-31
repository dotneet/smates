import { expect, test } from 'vitest';
import { loadExporter } from '@lib/exporters/registry';
import { sampleInvoice } from '@test/helpers/sample-invoice';

test('markdown exporter', async () => {
  const md = await loadExporter('markdown');
  const out = md(sampleInvoice());
  expect(out).toContain('# Invoice INV-1');
  expect(out).toContain('| WIDGET | 2 | 20.00 € |');
  expect(out).toContain('**Total:** 45.20 €');
});

test('html exporter', async () => {
  const html = await loadExporter('HTML');
  expect(html(sampleInvoice({ id: '<1>' }))).toContain('<h1>Invoice &lt;1&gt;</h1>');
  expect(html(sampleInvoice())).toContain('<p class="total">45.20 €</p>');
});

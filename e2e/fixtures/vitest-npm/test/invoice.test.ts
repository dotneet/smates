import { expect, test } from 'vitest';
import { renderInvoice, totals } from '@lib/invoice';
import { sampleInvoice } from '@test/helpers/sample-invoice';

test('totals apply standard and reduced rates', () => {
  expect(totals(sampleInvoice())).toEqual({ subtotal: 4000, tax: 520, total: 4520 });
});

test('renders an invoice', () => {
  const text = renderInvoice(sampleInvoice());
  expect(text.split('\n')[0]).toBe('Invoice INV-1 for ACME GmbH');
  expect(text).toContain('Total 45.20 €');
});

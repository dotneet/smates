import type { Invoice } from '@lib/invoice';

export function sampleInvoice(overrides: Partial<Invoice> = {}): Invoice {
  return {
    id: 'INV-1',
    customer: 'ACME GmbH',
    currency: 'EUR',
    region: 'DE',
    items: [
      { sku: 'WIDGET', qty: 2, unitCents: 1000 },
      { sku: 'BOOK', qty: 1, unitCents: 2000, reduced: true },
    ],
    ...overrides,
  };
}

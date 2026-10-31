import { formatCents } from './money.js';

// Only used by scripts/report.js (a CLI run as a child process).
export function formatReport(rows) {
  const lines = rows.map((r) => `${r.sku.padEnd(8)}${formatCents(r.cents).padStart(10)}`);
  return ['SKU          PRICE', ...lines].join('\n');
}

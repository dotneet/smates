import { formatMoney } from '@lib/money';
import type { Plugin, Row } from '../index';

const cell = (v: string | number) => (typeof v === 'string' && /[",\n]/.test(v) ? `"${v.replace(/"/g, '""')}"` : String(v));

const csv: Plugin = {
  name: 'csv',
  contentType: 'text/csv',
  serialize(rows: Row[]) {
    if (rows.length === 0) return '';
    const cols = Object.keys(rows[0]);
    const body = rows.map((r) => cols.map((c) => (c.endsWith('Cents') ? formatMoney(Number(r[c]), 'USD') : cell(r[c]))).join(','));
    return [cols.join(','), ...body].join('\n');
  },
};
export default csv;

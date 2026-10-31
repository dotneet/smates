import { formatMoney } from '@fx/core/money';

const products = [
  { id: 'p1', name: 'Mug', cents: 1250 },
  { id: 'p2', name: 'Poster', cents: 2000 },
];

export default function list(q: Record<string, string>) {
  const currency = q.currency ?? 'USD';
  return products.map((p) => ({ id: p.id, name: p.name, price: formatMoney(p.cents, currency) }));
}

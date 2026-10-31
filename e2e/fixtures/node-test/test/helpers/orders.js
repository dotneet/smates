import { readFileSync } from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import { makeCart } from '../../src/cart.js';

const fixtures = path.join(path.dirname(fileURLToPath(import.meta.url)), '..', 'fixtures');

export function loadOrder(name) {
  return JSON.parse(readFileSync(path.join(fixtures, `${name}.json`), 'utf8'));
}

export function cartFromOrder(order) {
  const cart = makeCart(order.region);
  for (const line of order.lines) cart.add(line.name, line.cents, line.qty);
  return cart;
}

'use strict';
const { quote } = require('../pricing');

function handle(payload, { inventory }) {
  for (const line of payload.lines) {
    inventory.reserve(line.sku, line.qty);
  }
  return { status: 'created', id: payload.id, ...quote(payload.lines) };
}

module.exports = { handle };

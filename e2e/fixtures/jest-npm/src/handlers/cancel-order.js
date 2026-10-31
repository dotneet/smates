'use strict';

function handle(payload, { inventory }) {
  for (const line of payload.lines) {
    inventory.release(line.sku, line.qty);
  }
  return { status: 'cancelled', id: payload.id };
}

module.exports = { handle };

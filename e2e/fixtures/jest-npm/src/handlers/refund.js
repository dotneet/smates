'use strict';
const { format } = require('../money');

function handle(payload) {
  const cents = -Math.abs(payload.amountCents);
  return { status: 'refunded', id: payload.id, amount: cents, display: format(cents, payload.currency) };
}

module.exports = { handle };

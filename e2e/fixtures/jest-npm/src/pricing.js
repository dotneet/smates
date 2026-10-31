'use strict';
const config = require('./config');
const { sum } = require('./money');

function subtotal(lines) {
  return sum(lines.map((l) => l.unitCents * l.qty));
}

function shipping(sub) {
  return sub >= config.freeShippingThreshold ? 0 : config.shippingFee;
}

function quote(lines) {
  const sub = subtotal(lines);
  const tax = Math.round(sub * config.taxRate);
  const ship = shipping(sub);
  return { subtotal: sub, tax, shipping: ship, total: sub + tax + ship, currency: config.currency };
}

module.exports = { subtotal, shipping, quote };

'use strict';
const mailer = require('./mailer');
const templates = require('./templates');

async function orderConfirmed(order, email) {
  const all = templates.loadAll();
  const body = templates.render(all['order-confirmed'], { id: order.id, total: order.totalDisplay });
  return mailer.send(email, `Order ${order.id} confirmed`, body);
}

module.exports = { orderConfirmed };

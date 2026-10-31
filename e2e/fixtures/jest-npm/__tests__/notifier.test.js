'use strict';
jest.mock('../src/mailer');

const mailer = require('../src/mailer');
const { orderConfirmed } = require('../src/notifier');

test('order confirmation is rendered and sent through the mailer', async () => {
  await orderConfirmed({ id: 'N-1', totalDisplay: '34.95 EUR' }, 'ann@example.com');
  expect(mailer.send).toHaveBeenCalledTimes(1);
  expect(mailer.outbox[0].subject).toBe('Order N-1 confirmed');
  expect(mailer.outbox[0].body).toContain('Total: 34.95 EUR.');
});

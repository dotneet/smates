'use strict';
// Manual jest mock for src/mailer.js: records messages instead of sending them.
const outbox = [];

module.exports = {
  outbox,
  send: jest.fn(async (to, subject, body) => {
    outbox.push({ to, subject, body });
    return { queued: true, id: outbox.length };
  }),
};

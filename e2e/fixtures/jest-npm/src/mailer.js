'use strict';
// Real SMTP transport (never used in tests: see __mocks__/mailer.js).
const net = require('net');

function send(to, subject, body) {
  return new Promise((resolve, reject) => {
    const socket = net.connect(25, process.env.SMTP_HOST || 'localhost');
    socket.on('error', reject);
    socket.end(`To: ${to}\r\nSubject: ${subject}\r\n\r\n${body}\r\n`, () => resolve({ queued: true }));
  });
}

module.exports = { send };

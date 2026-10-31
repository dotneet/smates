#!/usr/bin/env node
'use strict';
// Reads orders (JSON) on stdin, prints a summary (JSON) on stdout.
const { sum, format } = require('../src/money');

let input = '';
process.stdin.setEncoding('utf8');
process.stdin.on('data', (c) => (input += c));
process.stdin.on('end', () => {
  const orders = JSON.parse(input);
  const total = sum(orders.map((o) => o.totalCents));
  process.stdout.write(JSON.stringify({ count: orders.length, total, display: format(total) }));
});

'use strict';

function toCents(amount) {
  return Math.round(Number(amount) * 100);
}

function format(cents, currency = 'EUR') {
  const sign = cents < 0 ? '-' : '';
  const abs = Math.abs(cents);
  const units = Math.floor(abs / 100);
  const rest = String(abs % 100).padStart(2, '0');
  return `${sign}${units}.${rest} ${currency}`;
}

function sum(values) {
  return values.reduce((acc, v) => acc + v, 0);
}

module.exports = { toCents, format, sum };

// Leaf module: money formatting.
export function formatCents(cents, currency = 'USD') {
  const sign = cents < 0 ? '-' : '';
  const abs = Math.abs(Math.round(cents));
  const whole = Math.floor(abs / 100);
  const frac = String(abs % 100).padStart(2, '0');
  return `${sign}${currency === 'USD' ? '$' : currency + ' '}${whole}.${frac}`;
}

export function sumCents(values) {
  return values.reduce((a, b) => a + b, 0);
}

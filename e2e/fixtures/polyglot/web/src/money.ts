export type Cents = number;

/** Formats integer cents as "$12.34". */
export function formatCents(c: Cents): string {
  const sign = c < 0 ? '-' : '';
  const abs = Math.abs(c);
  return `${sign}$${Math.floor(abs / 100)}.${String(abs % 100).padStart(2, '0')}`;
}

/** pct percent of c, rounded half up. */
export function percent(c: Cents, pct: number): Cents {
  return Math.floor((c * pct + 50) / 100);
}

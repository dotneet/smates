const SYMBOLS: Record<string, string> = { USD: "$", EUR: "€", GBP: "£" };

/** Formats integer cents, e.g. 123456 -> "$1,234.56". */
export function formatCents(cents: number, currency = "USD"): string {
  const sign = cents < 0 ? "-" : "";
  const abs = Math.abs(Math.round(cents));
  const whole = Math.floor(abs / 100)
    .toString()
    .replace(/\B(?=(\d{3})+(?!\d))/g, ",");
  const frac = (abs % 100).toString().padStart(2, "0");
  return `${sign}${SYMBOLS[currency] ?? currency + " "}${whole}.${frac}`;
}

/** pct percent of cents, rounded half away from zero. */
export function percentOf(cents: number, pct: number): number {
  return Math.round((cents * pct) / 100);
}

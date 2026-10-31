export interface Shopper {
  name: string;
  locale: string;
  currency: string;
}

export function greet(s: Shopper): string {
  return `Welcome back, ${s.name}! Prices in ${s.currency}.`;
}

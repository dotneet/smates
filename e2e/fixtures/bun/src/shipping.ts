// Shipping zones are read with Bun's native file API (Bun.file), not node:fs.
import { join } from "node:path";

interface Zone {
  countries: string[];
  baseCents: number;
  perKgCents: number;
}

const ZONES_FILE = join(import.meta.dir, "..", "data", "shipping-zones.json");

export async function shippingCost(country: string, grams: number): Promise<number> {
  const zones: Record<string, Zone> = await Bun.file(ZONES_FILE).json();
  const zone = Object.values(zones).find((z) => z.countries.includes(country));
  if (!zone) throw new Error(`no shipping to ${country}`);
  return zone.baseCents + Math.ceil(grams / 1000) * zone.perKgCents;
}

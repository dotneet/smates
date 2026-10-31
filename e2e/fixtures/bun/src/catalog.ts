// The product catalog is a CSV file read with a named `readFileSync` import.
import { readFileSync } from "node:fs";
import { join } from "node:path";
import { z } from "zod";

const Product = z.object({
  sku: z.string().regex(/^[A-Z]{2}-\d{3}$/),
  name: z.string().min(1),
  category: z.enum(["food", "book", "general"]),
  priceCents: z.coerce.number().int().nonnegative(),
});
export type Product = z.infer<typeof Product>;

const DATA_DIR = join(import.meta.dir, "..", "data");

export function parseCatalog(text: string): Map<string, Product> {
  const [header, ...rows] = text.trim().split(/\r?\n/);
  const cols = header.split(",");
  const out = new Map<string, Product>();
  for (const row of rows) {
    const cells = row.split(",");
    const rec = Object.fromEntries(cols.map((c, i) => [c, cells[i]]));
    const p = Product.parse(rec);
    out.set(p.sku, p);
  }
  return out;
}

export function loadCatalog(file = "catalog.csv"): Map<string, Product> {
  return parseCatalog(readFileSync(join(DATA_DIR, file), "utf8"));
}

// Discount rules are plugins: every module in this directory is discovered with a
// directory listing and loaded with a computed dynamic import.
import { readdirSync } from "node:fs";
import { join } from "node:path";
import type { Discount } from "../types";

export async function loadDiscounts(): Promise<Discount[]> {
  const files = readdirSync(import.meta.dir)
    .filter((f) => f.endsWith(".ts") && f !== "index.ts")
    .sort();
  const mods = await Promise.all(files.map((f) => import(join(import.meta.dir, f))));
  return mods.map((m) => m.default as Discount);
}

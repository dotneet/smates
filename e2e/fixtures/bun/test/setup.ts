// Preloaded by bunfig.toml before every test file: loads the customer fixtures once.
import { readFileSync } from "node:fs";
import { join } from "node:path";

export interface Customer {
  id: string;
  name: string;
  points: number;
  country: string;
}

const customers: Customer[] = JSON.parse(readFileSync(join(import.meta.dir, "fixtures", "customers.json"), "utf8"));
(globalThis as { __customers?: Customer[] }).__customers = customers;

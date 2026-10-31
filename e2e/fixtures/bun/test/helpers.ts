import type { Customer } from "./setup";

export function fixtureCustomer(id: string): Customer {
  const all = (globalThis as { __customers?: Customer[] }).__customers;
  if (!all) throw new Error("customer fixtures not loaded (test/setup.ts preload missing)");
  const c = all.find((x) => x.id === id);
  if (!c) throw new Error(`no fixture customer ${id}`);
  return c;
}

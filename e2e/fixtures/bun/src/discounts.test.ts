import { expect, test } from "bun:test";
import { loadDiscounts } from "./discounts";
import type { Line } from "./types";

const line = (over: Partial<Line>): Line => ({ sku: "GN-001", name: "x", category: "general", qty: 1, unitCents: 1000, ...over });

test("discovers every discount plugin", async () => {
  expect((await loadDiscounts()).map((d) => d.id)).toEqual(["books", "bulk"]);
});

test("plugin rules", async () => {
  const byId = Object.fromEntries((await loadDiscounts()).map((d) => [d.id, d]));
  expect(byId.books.applies(line({ category: "book" }))).toBe(true);
  expect(byId.books.amount(line({ category: "book", qty: 2 }))).toBe(200);
  expect(byId.bulk.applies(line({ qty: 9 }))).toBe(false);
  expect(byId.bulk.amount(line({ qty: 10 }))).toBe(500);
});

import { expect, test } from "bun:test";
import { shippingCost } from "./shipping";

test("zone pricing", async () => {
  expect(await shippingCost("US", 500)).toBe(499 + 120);
  expect(await shippingCost("FR", 2500)).toBe(1299 + 3 * 450);
  expect(await shippingCost("JP", 1000)).toBe(1499 + 520);
});

test("unsupported country", async () => {
  await expect(shippingCost("BR", 100)).rejects.toThrow("no shipping to BR");
});

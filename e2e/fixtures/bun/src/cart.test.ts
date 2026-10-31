import { expect, test } from "bun:test";
import { priceCart, summary } from "./cart";

test("prices a mixed cart in Germany", async () => {
  const p = await priceCart(
    [
      { sku: "BK-001", qty: 1 },
      { sku: "FD-001", qty: 10 },
      { sku: "GN-002", qty: 2 },
    ],
    "DE",
  );
  expect(p.subtotal).toBe(4200 + 18900 + 2500);
  expect(p.applied).toEqual(["books", "bulk"]);
  expect(p.discount).toBe(420 + 945);
  expect(p.tax).toBe(718 + 1257 + 475);
  expect(summary(p, "EUR")).toBe("subtotal €256.00, discount €13.65, tax €24.50, total €266.85");
});

test("unknown sku", async () => {
  await expect(priceCart([{ sku: "ZZ-999", qty: 1 }], "JP")).rejects.toThrow("unknown sku");
});

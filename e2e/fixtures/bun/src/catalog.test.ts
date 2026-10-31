import { expect, test } from "bun:test";
import { loadCatalog, parseCatalog } from "./catalog";

test("loads the catalog", () => {
  const c = loadCatalog();
  expect(c.size).toBe(6);
  expect(c.get("BK-001")?.priceCents).toBe(4200);
  expect(c.get("FD-002")?.category).toBe("food");
});

test("rejects malformed rows", () => {
  expect(() => parseCatalog("sku,name,category,priceCents\nbad,X,book,1")).toThrow();
});

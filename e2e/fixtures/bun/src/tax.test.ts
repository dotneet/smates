import { expect, test } from "bun:test";
import { regions, taxFor, taxRate } from "./tax";

test("known regions", () => {
  expect(regions()).toEqual(["DE", "JP", "US-CA", "US-OR"]);
});

test("standard and reduced rates", () => {
  expect(taxRate("DE")).toBe(19);
  expect(taxRate("DE", true)).toBe(7);
  expect(taxRate("US-CA", true)).toBe(7.25);
  expect(taxFor(10000, "JP", true)).toBe(800);
  expect(() => taxRate("XX")).toThrow("unknown tax region");
});

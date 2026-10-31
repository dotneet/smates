import { describe, expect, test } from "bun:test";
import { formatCents, percentOf } from "./money";

describe("formatCents", () => {
  test("formats with grouping", () => {
    expect(formatCents(123456)).toBe("$1,234.56");
    expect(formatCents(5)).toBe("$0.05");
    expect(formatCents(-1999, "EUR")).toBe("-€19.99");
  });
  test("unknown currency falls back to the code", () => {
    expect(formatCents(100, "CHF")).toBe("CHF 1.00");
  });
});

test("percentOf rounds", () => {
  expect(percentOf(999, 10)).toBe(100);
  expect(percentOf(1000, 7.25)).toBe(73);
});

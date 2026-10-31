import { expect, test } from "bun:test";
import { fixtureCustomer } from "../test/helpers";
import { tierDiscountPct, tierFor } from "./loyalty";

test("tiers by points", () => {
  expect(tierFor(0)).toBe("bronze");
  expect(tierFor(1000)).toBe("silver");
  expect(tierFor(5000)).toBe("gold");
});

test("fixture customers land in the expected tiers", () => {
  expect(tierFor(fixtureCustomer("c-ada").points)).toBe("gold");
  expect(tierFor(fixtureCustomer("c-bob").points)).toBe("silver");
  expect(tierDiscountPct(tierFor(fixtureCustomer("c-cyd").points))).toBe(0);
});

import { expect, test } from "bun:test";
import { validateDataDir } from "./validate-data";

test("every data file is well-formed", () => {
  const { checked, problems } = validateDataDir();
  expect(checked).toEqual(["catalog.csv", "shipping-zones.json", "tax-rates.json"]);
  expect(problems).toEqual([]);
});

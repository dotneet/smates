import { expect, test } from "bun:test";
import { availableLocales, t } from "./i18n";

test("ships English and Japanese", () => {
  expect(availableLocales()).toEqual(["en-US", "ja-JP"]);
});

test("interpolates messages", async () => {
  expect(await t("en-US", "cart.total", { amount: "$5.00" })).toBe("Total: $5.00");
  expect(await t("ja-JP", "cart.items", { count: 3 })).toBe("3 点");
  await expect(t("en-US", "nope")).rejects.toThrow("missing message");
});

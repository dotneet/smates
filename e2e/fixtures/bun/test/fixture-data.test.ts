import { expect, test } from "bun:test";
import fs from "node:fs";
import { join } from "node:path";

// Guards the shared fixtures: every file parses and every record has a unique id.
const DIR = join(import.meta.dir, "fixtures");

test("fixtures are valid", () => {
  const files = fs.readdirSync(DIR).filter((f) => f.endsWith(".json"));
  expect(files.length).toBeGreaterThan(0);
  for (const f of files) {
    const rows = JSON.parse(fs.readFileSync(join(DIR, f), "utf8")) as { id: string }[];
    expect(Array.isArray(rows)).toBe(true);
    expect(new Set(rows.map((r) => r.id)).size).toBe(rows.length);
  }
});

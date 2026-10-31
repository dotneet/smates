// Data integrity checks over every file in data/ (used by CI and by the admin tool).
import fs from "node:fs";
import { join } from "node:path";

const DATA_DIR = join(import.meta.dir, "..", "data");

export interface Problem {
  file: string;
  message: string;
}

export function validateDataDir(dir = DATA_DIR): { checked: string[]; problems: Problem[] } {
  const checked: string[] = [];
  const problems: Problem[] = [];
  for (const file of fs.readdirSync(dir).sort()) {
    const text = fs.readFileSync(join(dir, file), "utf8");
    checked.push(file);
    if (file.endsWith(".json")) {
      try {
        const v = JSON.parse(text);
        if (typeof v !== "object" || v === null || Object.keys(v).length === 0) {
          problems.push({ file, message: "empty document" });
        }
      } catch (e) {
        problems.push({ file, message: `invalid JSON: ${(e as Error).message}` });
      }
    } else if (file.endsWith(".csv")) {
      const [header, ...rows] = text.trim().split(/\r?\n/);
      const n = header.split(",").length;
      rows.forEach((r, i) => {
        if (r.split(",").length !== n) problems.push({ file, message: `row ${i + 2}: expected ${n} cells` });
      });
    }
  }
  return { checked, problems };
}

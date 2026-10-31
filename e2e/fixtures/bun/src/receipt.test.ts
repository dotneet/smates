import { expect, test } from "bun:test";
import { join } from "node:path";

// The receipt renderer is a CLI script; exercise it end to end in a child process.
const SCRIPT = join(import.meta.dir, "..", "scripts", "render-receipt.ts");

function render(order: unknown): string {
  const proc = Bun.spawnSync([process.execPath, SCRIPT], { stdin: Buffer.from(JSON.stringify(order)) });
  if (proc.exitCode !== 0) throw new Error(`receipt script failed: ${proc.stderr.toString()}`);
  return proc.stdout.toString();
}

test("renders lines, total and footer", () => {
  const out = render({
    id: "A-17",
    lines: [
      { name: "Coffee", qty: 2, unitCents: 1890 },
      { name: "Cable", qty: 1, unitCents: 1250 },
    ],
  });
  const lines = out.trimEnd().split("\n");
  expect(lines[0]).toStartWith("RECEIPT A-17");
  expect(lines[2]).toBe("2 x Coffee" + "$37.80".padStart(30));
  expect(lines.at(-2)).toBe("TOTAL" + "$50.30".padStart(35));
  expect(lines.at(-1)).toBe("Thank you for shopping with Bun Shop!");
});

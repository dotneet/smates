// Renders a plain-text receipt from an order on stdin. Run as a separate process
// (`bun run receipt < order.json`, and by the receipt test).
import { readFileSync } from "node:fs";
import { join } from "node:path";
import { formatCents } from "../src/money";

interface Order {
  id: string;
  currency?: string;
  lines: { name: string; qty: number; unitCents: number }[];
}

const order: Order = JSON.parse(await Bun.stdin.text());
const footer = readFileSync(join(import.meta.dir, "receipt-footer.txt"), "utf8").trim();
const width = 40;
const out: string[] = [`RECEIPT ${order.id}`.padEnd(width, " "), "-".repeat(width)];
let total = 0;
for (const l of order.lines) {
  const amount = l.qty * l.unitCents;
  total += amount;
  const left = `${l.qty} x ${l.name}`;
  const right = formatCents(amount, order.currency);
  out.push(left + right.padStart(width - left.length, " "));
}
out.push("-".repeat(width));
out.push("TOTAL" + formatCents(total, order.currency).padStart(width - 5, " "));
out.push(footer);
console.log(out.join("\n"));

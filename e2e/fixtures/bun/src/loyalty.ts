export type Tier = "bronze" | "silver" | "gold";

const THRESHOLDS: [Tier, number][] = [
  ["gold", 5000],
  ["silver", 1000],
  ["bronze", 0],
];

export function tierFor(points: number): Tier {
  for (const [tier, min] of THRESHOLDS) if (points >= min) return tier;
  return "bronze";
}

/** Extra discount percentage granted by a loyalty tier. */
export function tierDiscountPct(tier: Tier): number {
  return { bronze: 0, silver: 2, gold: 5 }[tier];
}

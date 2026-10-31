// Only used by scripts/stats.js (run through a shell pipeline).
export function median(values) {
  const s = [...values].sort((a, b) => a - b);
  const mid = Math.floor(s.length / 2);
  return s.length % 2 ? s[mid] : (s[mid - 1] + s[mid]) / 2;
}

export function describe(values) {
  return `count=${values.length} median=${median(values)} max=${Math.max(...values)}`;
}

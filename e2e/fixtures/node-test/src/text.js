// Leaf module used by plugins only.
export function words(s) {
  return s.split(/\s+/).filter(Boolean);
}

export function capitalize(w) {
  return w.charAt(0).toUpperCase() + w.slice(1);
}

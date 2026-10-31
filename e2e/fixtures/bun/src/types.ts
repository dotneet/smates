// Shared shapes (type-only module: erased at runtime).
export type Category = "food" | "book" | "general";

export interface Line {
  sku: string;
  name: string;
  category: Category;
  qty: number;
  unitCents: number;
}

export interface Discount {
  id: string;
  label: string;
  applies(line: Line): boolean;
  amount(line: Line): number;
}

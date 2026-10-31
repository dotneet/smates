import { percentOf } from "../money";
import type { Discount } from "../types";

const books: Discount = {
  id: "books",
  label: "10% off books",
  applies: (l) => l.category === "book",
  amount: (l) => percentOf(l.unitCents * l.qty, 10),
};
export default books;

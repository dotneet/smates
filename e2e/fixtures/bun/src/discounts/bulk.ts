import { percentOf } from "../money";
import type { Discount } from "../types";

const MIN_QTY = 10;

const bulk: Discount = {
  id: "bulk",
  label: `5% off ${MIN_QTY}+ units`,
  applies: (l) => l.qty >= MIN_QTY,
  amount: (l) => percentOf(l.unitCents * l.qty, 5),
};
export default bulk;

// Command tool prints a price quote for one order line.
package main

import (
	"flag"
	"fmt"

	"example.com/shop/pkg/calc"
	"example.com/shop/pkg/limits"
)

func main() {
	sku := flag.String("sku", "A1", "product SKU")
	qty := flag.Int("qty", 1, "quantity")
	unit := flag.Int("unit", 100, "unit price in cents")
	discount := flag.Int("discount", 0, "discount percent")
	flag.Parse()
	sub := calc.Subtotal([]calc.Line{{SKU: *sku, Qty: *qty, UnitCents: *unit}})
	after := calc.ApplyDiscount(sub, limits.ClampDiscount(*discount))
	total := after + calc.Tax(after, 100)
	fmt.Printf("quote %s x%d: total=%d\n", *sku, *qty, total)
}

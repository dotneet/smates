// Package calc does money arithmetic in integer cents.
package calc

// Line is one order line.
type Line struct {
	SKU       string
	Qty       int
	UnitCents int
}

// Subtotal returns the sum of qty*unit price over all lines.
func Subtotal(lines []Line) int {
	total := 0
	for _, l := range lines {
		total += l.Qty * l.UnitCents
	}
	return total
}

// Tax returns the tax for an amount, rounded half up, at a rate in permille.
func Tax(cents, permille int) int {
	return (cents*permille + 500) / 1000
}

// Package pricing computes order line totals with volume discounts.
package pricing

import "example.com/polyglot/backend/internal/money"

// Line is one order line.
type Line struct {
	SKU       string
	Quantity  int64
	UnitCents money.Cents
}

// DiscountPercent is the volume discount for a quantity.
func DiscountPercent(qty int64) int64 {
	switch {
	case qty >= 100:
		return 15
	case qty >= 10:
		return 5
	}
	return 0
}

// Total returns the discounted total of a line.
func Total(l Line) money.Cents {
	gross := money.Cents(l.Quantity) * l.UnitCents
	return gross - money.Percent(gross, DiscountPercent(l.Quantity))
}

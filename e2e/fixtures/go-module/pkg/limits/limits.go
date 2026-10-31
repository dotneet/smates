// Package limits holds business limits. They mirror config/settings.yaml.
package limits

const (
	MaxItems       = 50
	MaxDiscountPct = 30
	TaxPermille    = 100
)

// ClampDiscount bounds a requested discount to the allowed maximum.
func ClampDiscount(pct int) int {
	if pct < 0 {
		return 0
	}
	if pct > MaxDiscountPct {
		return MaxDiscountPct
	}
	return pct
}

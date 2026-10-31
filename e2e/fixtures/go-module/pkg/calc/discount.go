package calc

// ApplyDiscount removes pct percent from cents, rounding the discount down.
func ApplyDiscount(cents, pct int) int {
	if pct <= 0 {
		return cents
	}
	if pct > 100 {
		pct = 100
	}
	return cents - cents*pct/100
}

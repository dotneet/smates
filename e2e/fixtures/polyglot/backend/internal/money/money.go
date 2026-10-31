// Package money formats and rounds amounts held in integer cents.
package money

import "fmt"

// Cents is an amount in the smallest currency unit.
type Cents int64

// Format renders c as "$12.34".
func Format(c Cents) string {
	sign := ""
	if c < 0 {
		sign, c = "-", -c
	}
	return fmt.Sprintf("%s$%d.%02d", sign, c/100, c%100)
}

// Percent returns pct percent of c, rounded half up.
func Percent(c Cents, pct int64) Cents {
	return Cents((int64(c)*pct + 50) / 100)
}

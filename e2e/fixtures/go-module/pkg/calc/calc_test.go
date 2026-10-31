package calc

import "testing"

func TestSubtotal(t *testing.T) {
	got := Subtotal([]Line{{"A1", 3, 250}, {"B2", 1, 1000}})
	if got != 1750 {
		t.Fatalf("Subtotal = %d, want 1750", got)
	}
}

func TestTax(t *testing.T) {
	if got := Tax(1750, 100); got != 175 {
		t.Fatalf("Tax = %d, want 175", got)
	}
	if got := Tax(5, 100); got != 1 { // 0.5 rounds half up
		t.Fatalf("Tax(5) = %d, want 1", got)
	}
}

func TestApplyDiscount(t *testing.T) {
	cases := []struct{ in, pct, want int }{{1000, 0, 1000}, {1000, 10, 900}, {999, 10, 900}, {500, 150, 0}}
	for _, c := range cases {
		if got := ApplyDiscount(c.in, c.pct); got != c.want {
			t.Errorf("ApplyDiscount(%d, %d) = %d, want %d", c.in, c.pct, got, c.want)
		}
	}
}

package api

import (
	"testing"

	"example.com/shop/pkg/calc"
)

func TestPrice(t *testing.T) {
	q, err := Price(Order{Lines: []calc.Line{{SKU: "A1", Qty: 4, UnitCents: 250}}, DiscountPct: 10})
	if err != nil {
		t.Fatal(err)
	}
	if q != (Quote{Subtotal: 1000, Discount: 100, Tax: 90, Total: 990}) {
		t.Fatalf("quote = %+v", q)
	}
}

func TestPriceEmpty(t *testing.T) {
	if _, err := Price(Order{}); err == nil {
		t.Fatal("expected error")
	}
}

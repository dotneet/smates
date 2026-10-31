package api_test

import (
	"strings"
	"testing"

	"example.com/shop/pkg/api"
	"example.com/shop/pkg/calc"
)

func TestCheckoutInvoice(t *testing.T) {
	out, err := api.Checkout(api.Order{Customer: "Ada", Lines: []calc.Line{{SKU: "A1", Qty: 4, UnitCents: 250}}, DiscountPct: 10})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(out, "INVOICE for Ada") || !strings.Contains(out, "total    9.90 USD") {
		t.Fatalf("invoice:\n%s", out)
	}
}

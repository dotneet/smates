package render

import (
	_ "embed"
	"strings"
	"testing"
)

//go:embed testdata/invoice.golden
var invoiceGolden string

func TestInvoice(t *testing.T) {
	got, err := Invoice(InvoiceData{Customer: "Ada", Subtotal: 1000, Discount: 100, Tax: 90, Total: 990, Currency: "USD"})
	if err != nil {
		t.Fatal(err)
	}
	if want := strings.TrimSpace(invoiceGolden); got != want {
		t.Fatalf("Invoice =\n%s\nwant\n%s", got, want)
	}
}

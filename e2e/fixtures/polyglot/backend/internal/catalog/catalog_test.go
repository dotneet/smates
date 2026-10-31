package catalog

import "testing"

func TestLoadDir(t *testing.T) {
	ps, err := LoadDir("testdata/products")
	if err != nil {
		t.Fatal(err)
	}
	var skus []string
	for _, p := range ps {
		skus = append(skus, p.SKU)
	}
	if len(ps) != 2 || skus[0] != "FRU-001" || skus[1] != "FRU-002" {
		t.Fatalf("products = %v", skus)
	}
}

func TestQuote(t *testing.T) {
	ps, err := LoadDir("testdata/products")
	if err != nil {
		t.Fatal(err)
	}
	// 10 apples at 120 and 10 pears at 80, 5% volume discount.
	if got := Quote(ps, 10); got != 1900 {
		t.Fatalf("Quote = %d, want 1900", got)
	}
}

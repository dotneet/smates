// Package catalog loads product definitions (one JSON file per product) from a directory.
package catalog

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"example.com/polyglot/backend/internal/money"
	"example.com/polyglot/backend/internal/pricing"
)

// Product is one catalog entry.
type Product struct {
	SKU       string      `json:"sku"`
	Name      string      `json:"name"`
	UnitCents money.Cents `json:"unit_cents"`
}

// LoadDir reads every *.json file in dir, sorted by SKU.
func LoadDir(dir string) ([]Product, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var out []Product
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			return nil, err
		}
		var p Product
		if err := json.Unmarshal(data, &p); err != nil {
			return nil, fmt.Errorf("%s: %w", e.Name(), err)
		}
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].SKU < out[j].SKU })
	return out, nil
}

// Quote prices qty units of every product.
func Quote(ps []Product, qty int64) money.Cents {
	var sum money.Cents
	for _, p := range ps {
		sum += pricing.Total(pricing.Line{SKU: p.SKU, Quantity: qty, UnitCents: p.UnitCents})
	}
	return sum
}

package api

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"example.com/shop/pkg/calc"
)

// The command line tool must quote the same total as the API.
func TestToolMatchesAPI(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "tool")
	if out, err := exec.Command("go", "build", "-o", bin, "../../cmd/tool").CombinedOutput(); err != nil {
		t.Fatalf("go build cmd/tool: %v\n%s", err, out)
	}
	out, err := exec.Command(bin, "-sku", "A1", "-qty", "4", "-unit", "250", "-discount", "10").CombinedOutput()
	if err != nil {
		t.Fatalf("cmd/tool: %v\n%s", err, out)
	}
	q, err := Price(Order{Lines: []calc.Line{{SKU: "A1", Qty: 4, UnitCents: 250}}, DiscountPct: 10})
	if err != nil {
		t.Fatal(err)
	}
	want := "quote A1 x4: total=990"
	if got := strings.TrimSpace(string(out)); got != want || q.Total != 990 {
		t.Fatalf("tool printed %q, want %q (api total %d)", got, want, q.Total)
	}
}

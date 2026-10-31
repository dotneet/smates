package limits

import (
	"os"
	"strconv"
	"strings"
	"testing"
)

// readSettings parses the flat "key: value" config shared with deployment, with the
// optional developer override settings.local.yaml applied on top.
func readSettings(t *testing.T) map[string]string {
	t.Helper()
	out := map[string]string{}
	parseSettings(t, "../../config/settings.yaml", out, true)
	parseSettings(t, "../../config/settings.local.yaml", out, false)
	return out
}

func parseSettings(t *testing.T, path string, out map[string]string, required bool) {
	t.Helper()
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) && !required {
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	for i, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			t.Fatalf("%s:%d: malformed line %q", path, i+1, line)
		}
		out[strings.TrimSpace(k)] = strings.TrimSpace(v)
	}
}

func TestConstantsMatchSettings(t *testing.T) {
	s := readSettings(t)
	for key, want := range map[string]int{"max_items": MaxItems, "max_discount_pct": MaxDiscountPct, "tax_permille": TaxPermille} {
		got, err := strconv.Atoi(s[key])
		if err != nil {
			t.Fatalf("%s: %v", key, err)
		}
		if got != want {
			t.Errorf("settings %s = %d, code has %d", key, got, want)
		}
	}
}

func TestClampDiscount(t *testing.T) {
	if ClampDiscount(80) != MaxDiscountPct || ClampDiscount(-1) != 0 || ClampDiscount(5) != 5 {
		t.Fatal("ClampDiscount")
	}
}

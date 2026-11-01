package summary

import (
	"strings"
	"testing"

	"github.com/dotneet/smates/internal/selector"
)

func TestServiceVia(t *testing.T) {
	e2e := sel("e2e/login.spec.ts", selector.ReasonChanged, map[string]string{"backend/api/user.go": selector.ViaService}, 5000)
	e2e.Service = map[string]string{"backend/api/user.go": "depends_on suite go"}
	res := &selector.Result{
		Mode:    "state",
		Suites:  []*selector.Suite{suite("e2e", "vitest", e2e, skip("e2e/other.spec.ts", 100))},
		Changed: []string{"backend/api/user.go"},
	}
	out := Markdown(&Input{Result: res})
	if !strings.Contains(out, "server code its tests exercise (`depends_on suite go`)") {
		t.Errorf("service dependency not explained:\n%s", out)
	}
	if got := viaLabel(selector.ViaService, "x.go", ""); got != "server code its tests exercise" {
		t.Errorf("without a label: %q", got)
	}
}

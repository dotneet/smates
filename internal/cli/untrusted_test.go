package cli

import (
	"strings"
	"testing"

	"github.com/dotneet/smates/internal/runner"
	"github.com/dotneet/smates/internal/selector"
	"github.com/dotneet/smates/internal/state"
)

// TestUntrustedWarning: the browser passes trust_browser_passes does not record are
// named with the reason, a few of them; others get no line.
func TestUntrustedWarning(t *testing.T) {
	pass := func(static bool) *runner.Observation {
		return &runner.Observation{Status: state.Pass, StaticOnly: static}
	}
	cases := []struct {
		name      string
		obs       map[string]*runner.Observation
		untrusted map[string]string
		want      []string // substrings; nil = no warning
	}{
		{"trusted passes", map[string]*runner.Observation{"a.stories.tsx": pass(true)}, nil, nil},
		{"traced passes", map[string]*runner.Observation{"a.test.ts": pass(false)}, map[string]string{"a.test.ts": "x"}, nil},
		{"one untrusted", map[string]*runner.Observation{"a.stories.tsx": pass(true)}, map[string]string{"a.stories.tsx": `imports "~mocks/a", which smates cannot resolve`},
			[]string{"1 browser test(s) passed but are not trusted", `a.stories.tsx (imports "~mocks/a", which smates cannot resolve)`}},
		{"bounded", map[string]*runner.Observation{"a": pass(true), "b": pass(true), "c": pass(true), "d": pass(true)},
			map[string]string{"a": "r", "b": "r", "c": "r", "d": "r"}, []string{"4 browser test(s)", "a (r); b (r); c (r); and 1 more"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := untrustedWarning(&selector.Suite{Name: "ui", Untrusted: c.untrusted}, c.obs)
			if (got == "") != (c.want == nil) {
				t.Fatalf("warning %q, want %v", got, c.want)
			}
			for _, w := range c.want {
				if !strings.Contains(got, w) {
					t.Errorf("warning %q lacks %q", got, w)
				}
			}
		})
	}
}

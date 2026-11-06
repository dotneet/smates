package runner

import (
	"slices"
	"testing"
)

// bunfigPreloads reads every form bun takes a test preload in; a value it cannot read
// is reported (the selection then cannot tell what the preloads reach).
func TestBunfigPreloads(t *testing.T) {
	for _, c := range []struct {
		name, toml string
		want       []string
		ok         bool
	}{
		{"test table, array", "[test]\npreload = [\"./a.ts\", './b.ts']\n", []string{"./a.ts", "./b.ts"}, true},
		{"test table, string", "[test]\npreload = \"./a.ts\" # setup\n", []string{"./a.ts"}, true},
		{"top level", "preload = [\"./a.ts\"]\n[test]\nroot = \"src\"\n", []string{"./a.ts"}, true},
		{"dotted key", "test.preload = [\"./a.ts\"]\n", []string{"./a.ts"}, true},
		{"inline table", "test = { preload = [\"./a.ts\"], root = \"src\" }\n", []string{"./a.ts"}, true},
		{"byte order mark", "\xef\xbb\xbf[test]\npreload = [\"./a.ts\"]\n", []string{"./a.ts"}, true},
		{"empty array", "[test]\npreload = [] # none\n", nil, true},
		{"another table's preload", "[run]\npreload = [\"./r.ts\"]\n", nil, true},
		{"no preload", "[test]\ncoverage = true\n", nil, true},
		{"not a string", "[test]\npreload = 3\n", nil, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, ok := bunfigPreloads(c.toml)
			if !slices.Equal(got, c.want) || ok != c.ok {
				t.Errorf("bunfigPreloads = %q %v, want %q %v", got, ok, c.want, c.ok)
			}
		})
	}
}

package runner

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dotneet/smates/internal/config"
	"github.com/dotneet/smates/internal/gitx"
)

// The recording bunfig puts the shim first, keeps project preloads (absolute) and
// disables coverage thresholds, which per-file coverage runs would otherwise fail.
func TestBunConfigWithShim(t *testing.T) {
	for _, tc := range []struct{ name, in string }{
		{"none", ""},
		{"preload and threshold", "[test]\npreload = [\"./setup.ts\"]\ncoverageThreshold = { lines = 0.9, functions = 0.9 }\n"},
		{"no test section", "smol = true\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if tc.in != "" {
				if err := os.WriteFile(filepath.Join(dir, "bunfig.toml"), []byte(tc.in), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			rc := &RunContext{Suite: &Suite{Suite: config.Suite{Root: "."}}, TraceDir: t.TempDir()}
			rc.Repo = &gitx.Repo{Root: dir}
			path, cleanup, err := bunConfigWithShim(rc, "/shim/node.cjs")
			if err != nil {
				t.Fatal(err)
			}
			defer cleanup()
			data, _ := os.ReadFile(path)
			out := string(data)
			if !strings.Contains(out, `preload = ["/shim/node.cjs"`) || !strings.Contains(out, "coverageThreshold = 0") || strings.Contains(out, "0.9") {
				t.Errorf("bunfig:\n%s", out)
			}
			if strings.Contains(tc.in, "setup.ts") && !strings.Contains(out, filepath.Join(dir, "setup.ts")) {
				t.Errorf("project preload lost:\n%s", out)
			}
		})
	}
}

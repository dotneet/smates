//go:build !windows

package runner

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/dotneet/smates/internal/config"
	"github.com/dotneet/smates/internal/gitx"
)

// When the runner rejects smates' injected reporter, the run is retried without it: only
// the retry's V8 coverage reaches the user's NODE_V8_COVERAGE, once.
func TestNodeFallbackExportsCoverageOnce(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	// Writes one coverage file per run; fails (before any test) with the injected flag.
	script := `echo '{"result":[]}' > "$NODE_V8_COVERAGE/coverage-$$-1-0.json"
for a in "$@"; do [ "$a" = "--reporter=smates" ] && exit 1; done
exit 0
`
	if err := os.WriteFile(filepath.Join(root, "run.sh"), []byte(script), 0o644); err != nil {
		t.Fatal(err)
	}
	rc := &RunContext{
		Repo: &gitx.Repo{Root: root},
		Suite: &Suite{Suite: config.Suite{Name: "s", Runner: "vitest", Root: ".", Command: "sh run.sh",
			Env: map[string]string{"NODE_V8_COVERAGE": "usercov"}}},
		Record:   true,
		TraceDir: filepath.Join(t.TempDir(), "trace"),
		ShimDir:  t.TempDir(),
		Stdout:   io.Discard,
		Stderr:   io.Discard,
		UnitOf:   func(string) string { return "" },
	}
	if err := os.MkdirAll(rc.TraceDir, 0o755); err != nil {
		t.Fatal(err)
	}
	res, err := runNodeWithFallback(context.Background(), rc, []string{"--reporter=smates"}, nil, traceOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("retry exit code %d", res.ExitCode)
	}
	files, _ := filepath.Glob(filepath.Join(root, "usercov", "coverage-*.json"))
	if len(files) != 1 {
		t.Errorf("user's NODE_V8_COVERAGE has %d coverage files, want 1 (from the retry): %v", len(files), files)
	}
}

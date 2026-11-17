//go:build !windows

package runner

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/dotneet/smates/internal/config"
	"github.com/dotneet/smates/internal/gitx"
	"github.com/dotneet/smates/internal/state"
)

// Isolated runs (isolate: true, untraced) inject the reporter too, and fall back like a
// batch run: when every unit's process failed before running a test file with smates'
// arguments, the units run again without them, so smates never reports failures it
// caused. A run that failed on its own is not repeated.
func TestIsolatedNodeFallback(t *testing.T) {
	// Fails (before any test) with the injected flag; b.test.js fails when FAIL_B is set,
	// every file when FAIL_ALL is.
	script := `for a in "$@"; do [ "$a" = "--reporter=smates" ] && exit 1; last=$a; done
[ -n "$FAIL_ALL" ] && exit 1
case $last in *b.test*) [ -n "$FAIL_B" ] && exit 1;; esac
exit 0
`
	units := []string{"a.test.js", "b.test.js"}
	cases := []struct {
		name     string
		env      map[string]string
		injected []string
		reporter bool // the reporter loaded in the first attempt
		retried  bool
		exit     int
	}{
		{"reporter rejected", nil, []string{"--reporter=smates"}, false, true, 0},
		{"reporter rejected, a test fails without it", map[string]string{"FAIL_B": "1"}, []string{"--reporter=smates"}, false, true, 1},
		// Must keep: a unit passed with the injected arguments: they were not rejected.
		{"a unit passed", map[string]string{"FAIL_B": "1"}, []string{"--other"}, false, false, 1},
		// Must keep: the reporter loaded, so every failure is the tests' own.
		{"reporter loaded", map[string]string{"FAIL_ALL": "1"}, []string{"--other"}, true, false, 1},
		{"nothing injected", map[string]string{"FAIL_ALL": "1"}, nil, false, false, 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			repo := tempRepo(t, units...)
			if err := os.WriteFile(filepath.Join(repo.Root, "run.sh"), []byte(script), 0o644); err != nil {
				t.Fatal(err)
			}
			var stderr strings.Builder
			rc := &RunContext{
				Repo:     repo,
				Suite:    &Suite{Suite: config.Suite{Name: "s", Runner: "vitest", Root: ".", Command: "sh run.sh", Env: c.env}},
				Units:    units,
				Record:   true,
				TraceDir: filepath.Join(t.TempDir(), "trace"),
				ShimDir:  t.TempDir(),
				Stdout:   io.Discard,
				Stderr:   &stderr,
				UnitOf: func(f string) string {
					if slices.Contains(units, f) {
						return f
					}
					return ""
				},
			}
			if err := os.MkdirAll(rc.TraceDir, 0o755); err != nil {
				t.Fatal(err)
			}
			if c.reporter {
				os.WriteFile(filepath.Join(rc.TraceDir, "reporter-loaded-1"), nil, 0o644)
			}
			res, err := runIsolatedNodeWithFallback(context.Background(), rc, c.injected, func(u string, injected []string) []string {
				return append(append([]string{}, injected...), u)
			}, traceOptions{unitsFromArgv: true})
			if err != nil {
				t.Fatal(err)
			}
			if res.ExitCode != c.exit {
				t.Errorf("exit %d, want %d", res.ExitCode, c.exit)
			}
			if retried := strings.Contains(stderr.String(), "retrying without it"); retried != c.retried {
				t.Errorf("retried %v, want %v", retried, c.retried)
			}
			if c.retried && c.exit == 0 {
				for u, o := range res.Observations {
					if o.Status == state.Fail {
						t.Errorf("%s: a failure smates caused: %+v", u, o)
					}
				}
			}
		})
	}
}

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

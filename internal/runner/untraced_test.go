package runner

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/dotneet/smates/internal/config"
	"github.com/dotneet/smates/internal/state"
	"github.com/dotneet/smates/internal/trace"
)

// TestTraceEnvUntraced: an untraced run keeps the shims' environment but not smates'
// NODE_V8_COVERAGE, so the user's own value (if any) applies; a traced run gets it.
func TestTraceEnvUntraced(t *testing.T) {
	repo := tempRepo(t, "test/perf.test.ts")
	td := t.TempDir()
	for _, c := range []struct {
		name     string
		untraced bool
		userV8   string
	}{
		{"traced", false, ""},
		{"untraced", true, ""},
		{"untraced, the user collects coverage", true, "/tmp/user-v8"},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := &Suite{Suite: config.Suite{Root: "."}}
			if c.userV8 != "" {
				s.Env = map[string]string{"NODE_V8_COVERAGE": c.userV8}
			}
			rc := &RunContext{Repo: repo, Suite: s, TraceDir: td, ShimDir: t.TempDir(), Record: true, Untraced: c.untraced}
			env := traceEnv(rc)
			own := slices.Contains(env, "NODE_V8_COVERAGE="+trace.V8Dir(td))
			if own == c.untraced {
				t.Errorf("smates' NODE_V8_COVERAGE set: %v, want %v (%q)", own, !c.untraced, env)
			}
			if slices.ContainsFunc(env, func(kv string) bool { return strings.HasPrefix(kv, "NODE_V8_COVERAGE=") && !own }) {
				t.Errorf("an untraced run overrides the user's NODE_V8_COVERAGE: %q", env)
			}
			for _, key := range []string{"SMATES_TRACE_DIR=", "NODE_OPTIONS="} {
				if !slices.ContainsFunc(env, func(kv string) bool { return strings.HasPrefix(kv, key) }) {
					t.Errorf("%s missing: the shims must still trace (%q)", key, env)
				}
			}
		})
	}
}

// TestObserveUntracedRunResults: an untraced unit runs in a runner of its own whose
// processes the shim traces (untracedOptions), so a pass reported without any trace
// lost it (a wrapper dropping the environment, a killed process): must keep, it stays
// unknown and runs again rather than being recorded from its static closure alone. A
// failure stays a failure.
func TestObserveUntracedRunResults(t *testing.T) {
	for _, c := range []struct {
		name   string
		status string
		want   string
		warn   string
	}{
		{name: "must keep: a pass without a trace", status: "pass", want: state.Unknown, warn: "none left a trace"},
		{name: "a failure without a trace", status: "fail", want: state.Fail},
	} {
		t.Run(c.name, func(t *testing.T) {
			file := "test/perf.test.ts"
			repo := tempRepo(t, file)
			td := t.TempDir()
			os.MkdirAll(trace.V8Dir(td), 0o755)
			line := mustJSON(map[string]any{"k": "result", "id": filepath.Join(repo.Root, filepath.FromSlash(file)), "status": c.status, "ms": 3})
			os.WriteFile(filepath.Join(td, "results-vitest-1.jsonl"), []byte(line+"\n"), 0o644)
			s := &Suite{Suite: config.Suite{Root: ".", Tests: []string{"**/*.test.ts"}}}
			rc := &RunContext{Repo: repo, Suite: s, TraceDir: td, Record: true, Untraced: true, UnitOf: func(f string) string {
				if s.IsTestFile(f) {
					return f
				}
				return ""
			}}
			obs, warns, err := observe(rc, 0, untracedOptions(rc, traceOptions{}))
			if err != nil {
				t.Fatal(err)
			}
			o := obs[file]
			if o == nil || o.Status != c.want || o.StaticOnly {
				t.Fatalf("observation %+v, want status %s", o, c.want)
			}
			if c.warn == "" && len(warns) > 0 || c.warn != "" && (len(warns) != 1 || !strings.Contains(warns[0], c.warn)) {
				t.Errorf("warnings %q, want one containing %q", warns, c.warn)
			}
		})
	}
}

// TestTracesUntraced: the runners whose code coverage can be left out while the shims
// still trace, and only those, record untraced units.
func TestTracesUntraced(t *testing.T) {
	for _, name := range Names() {
		want := slices.Contains([]string{"vitest", "jest", "node", "bun"}, name)
		if got := TracesUntraced(name); got != want {
			t.Errorf("%s: %v, want %v", name, got, want)
		}
	}
}

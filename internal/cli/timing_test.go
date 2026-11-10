package cli

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/dotneet/smates/internal/config"
	"github.com/dotneet/smates/internal/runner"
)

// TestMeasuresTime: a test reading a clock and asserting on the order of values is a
// timing test, whichever clock and assertion style; a clock or an ordering assertion
// alone, or fake timers, is not.
func TestMeasuresTime(t *testing.T) {
	for _, c := range []struct {
		name, src string
		want      bool
	}{
		{"performance.now, vitest matcher", `const t0 = performance.now(); run(); expect(performance.now() - t0).toBeLessThan(50);`, true},
		{"ratio of two measurements", "function time(n) { const t0 = performance.now(); run(n); return performance.now() - t0; }\nexpect(time(400) / time(200)).toBeLessThan(6);", true},
		{"process.hrtime.bigint", `const t0 = process.hrtime.bigint(); run(); expect(Number(process.hrtime.bigint() - t0)).toBeLessThanOrEqual(1e8);`, true},
		{"process.hrtime pair", `const [s] = process.hrtime(start); expect(s).toBeGreaterThanOrEqual(0);`, true},
		{"Date.now difference", `const t0 = Date.now(); run(); const ms = Date.now() - t0; expect(ms).toBeGreaterThan(5);`, true},
		{"expect(a < b).toBe(true)", `const t = performance.now() - t0; expect(t < 100).toBe(true);`, true},
		{"node:assert ok", `const t = performance.now() - t0; assert.ok(t < 100);`, true},
		{"node:assert strict", `const t = performance.now() - t0; assert.strict.ok(t > 0);`, true},
		{"plain assert", `const t = performance.now() - t0; assert(t < 100, "too slow");`, true},
		{"a clock without an ordering assertion", `const t0 = performance.now(); expect(run()).toBe(4);`, false},
		{"an ordering assertion without a clock", `expect(items.length).toBeGreaterThan(2);`, false},
		{"Date.now as a timestamp", `const created = Date.now(); expect(created).toBeGreaterThan(0);`, false},
		{"fake timers (vitest)", `vi.useFakeTimers(); const t0 = performance.now(); expect(performance.now() - t0).toBeLessThan(10);`, false},
		{"fake timers (jest)", `jest.useFakeTimers(); const t0 = Date.now() - 1; expect(t0).toBeGreaterThan(0);`, false},
		{"mocked timers (node:test)", `mock.timers.enable({ apis: ["Date"] }); const ms = Date.now() - t0; assert.ok(ms < 10);`, false},
	} {
		if got := measuresTime([]byte(c.src)); got != c.want {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
		}
	}
}

// TestTimingTests: only test files of Node-based suites are reported, relative to the
// suite root, unless the suite's untraced already lists them.
func TestTimingTests(t *testing.T) {
	dir := t.TempDir()
	timed := `const t0 = performance.now(); expect(performance.now() - t0).toBeLessThan(50);`
	files := map[string]string{
		"web/test/perf.test.ts":  timed,
		"web/test/plain.test.ts": `expect(1).toBe(1);`,
		"web/test/clock.test.js": timed,
		"py/tests/test_perf.py":  "t0 = time.perf_counter()\nassert time.perf_counter() - t0 < 1\n",
	}
	var list []string
	for p, src := range files {
		abs := filepath.Join(dir, filepath.FromSlash(p))
		os.MkdirAll(filepath.Dir(abs), 0o755)
		os.WriteFile(abs, []byte(src), 0o644)
		list = append(list, p)
	}
	slices.Sort(list)
	mk := func(c config.Suite) *runner.Suite { return &runner.Suite{Suite: c} }
	for _, c := range []struct {
		name string
		s    *runner.Suite
		want []string
	}{
		{"vitest", mk(config.Suite{Name: "web", Runner: "vitest", Root: "web", Tests: []string{"test/*.test.*"}}), []string{"test/clock.test.js", "test/perf.test.ts"}},
		{"jest", mk(config.Suite{Name: "web", Runner: "jest", Root: "web", Tests: []string{"test/*.test.*"}}), []string{"test/clock.test.js", "test/perf.test.ts"}},
		{"already untraced", mk(config.Suite{Name: "web", Runner: "vitest", Root: "web", Tests: []string{"test/*.test.*"}, Untraced: []string{"test/perf.*"}}), []string{"test/clock.test.js"}},
		{"pytest (not detected)", mk(config.Suite{Name: "py", Runner: "pytest", Root: "py", Tests: []string{"tests/test_*.py"}}), nil},
	} {
		if got := timingTests(dir, c.s, list); !slices.Equal(got, c.want) {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
		}
	}
}

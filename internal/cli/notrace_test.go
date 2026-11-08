package cli

import (
	"strings"
	"testing"

	"github.com/dotneet/smates/internal/runner"
	"github.com/dotneet/smates/internal/state"
)

// TestNoTraceWarning checks when a recorded batch is reported as having left no
// trace: it passed, it had units and nothing was observed for any of them.
func TestNoTraceWarning(t *testing.T) {
	obs := func(ids ...string) map[string]*runner.Observation {
		m := map[string]*runner.Observation{}
		for _, id := range ids {
			m[id] = &runner.Observation{Files: map[string]bool{}, Status: state.Pass}
		}
		return m
	}
	for _, c := range []struct {
		name  string
		units []string
		r     *runner.RunResult
		warn  bool
	}{
		{"passed, nothing observed", []string{"a", "b"}, &runner.RunResult{}, true},
		{"passed, an empty map", []string{"a"}, &runner.RunResult{Observations: obs()}, true},
		{"passed, one unit observed", []string{"a", "b"}, &runner.RunResult{Observations: obs("a")}, false},
		{"passed, every unit observed", []string{"a"}, &runner.RunResult{Observations: obs("a")}, false},
		{"failed, nothing observed", []string{"a"}, &runner.RunResult{ExitCode: 1}, false},
		{"not started", []string{"a"}, &runner.RunResult{ExitCode: 127}, false},
		{"no units", nil, &runner.RunResult{}, false},
	} {
		w := noTraceWarning(c.units, c.r, "vitest", false)
		if (w != "") != c.warn {
			t.Errorf("%s: %q, want a warning: %v", c.name, w, c.warn)
		}
		if c.warn && (!strings.Contains(w, "left no trace") || !strings.Contains(w, "run again next time")) {
			t.Errorf("%s: %q", c.name, w)
		}
	}
}

// TestNoTraceWarningWording: the warning says what happens to the units next (run
// again as new, or decided against the base revision) and, for a custom suite, why it
// cannot be traced instead of blaming a wrapper.
func TestNoTraceWarningWording(t *testing.T) {
	for _, c := range []struct {
		name           string
		runner         string
		diffUnrecorded bool
		want, not      []string
	}{
		{"ci without a base", "go", false, []string{"run again next time", "wrapper"}, []string{"base revision"}},
		{"against the base", "go", true, []string{"decided against the base revision by static analysis", "wrapper"}, []string{"run again next time"}},
		{"custom suite", "custom", false, []string{"Node and Python processes", "junit"}, []string{"wrapper"}},
		{"custom suite against the base", "custom", true, []string{"base revision", "junit"}, []string{"wrapper", "run again"}},
	} {
		w := noTraceWarning([]string{"a"}, &runner.RunResult{}, c.runner, c.diffUnrecorded)
		for _, s := range c.want {
			if !strings.Contains(w, s) {
				t.Errorf("%s: %q lacks %q", c.name, w, s)
			}
		}
		for _, s := range c.not {
			if strings.Contains(w, s) {
				t.Errorf("%s: %q has %q", c.name, w, s)
			}
		}
	}
}

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
		w := noTraceWarning(c.units, c.r)
		if (w != "") != c.warn {
			t.Errorf("%s: %q, want a warning: %v", c.name, w, c.warn)
		}
		if c.warn && (!strings.Contains(w, "left no trace") || !strings.Contains(w, "run again next time")) {
			t.Errorf("%s: %q", c.name, w)
		}
	}
}

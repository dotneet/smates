package cli

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/dotneet/smates/internal/config"
	"github.com/dotneet/smates/internal/runner"
	"github.com/dotneet/smates/internal/selector"
	"github.com/dotneet/smates/internal/state"
)

// TestPlanUntraced: while recording, the units a suite's untraced lists run in batches
// of their own, however the run is cut (--fail-fast, --max-time); a full run then
// passes explicit units. Without recording, or without untraced units, nothing splits.
func TestPlanUntraced(t *testing.T) {
	ids := []string{"test/a.test.ts", "test/perf.test.ts", "test/b.test.ts"}
	newSuite := func(untraced []string) (*selector.Result, *state.State) {
		sr := &selector.Suite{Name: "web", All: true, Suite: &runner.Suite{Suite: config.Suite{Name: "web", Untraced: untraced}}, Files: map[string][]string{}}
		entries := map[string]*state.Entry{}
		for _, id := range ids {
			sr.Tests = append(sr.Tests, &selector.Test{ID: id, Selected: true, Reason: selector.ReasonAll})
			sr.Files[id] = []string{id}
			entries[id] = &state.Entry{Status: state.Pass, DurationMs: 10}
		}
		res := &selector.Result{Mode: selector.ModeAll, Suites: []*selector.Suite{sr}}
		st := &state.State{Suites: map[string]*state.Suite{"web": {Tests: entries}}}
		prioritize(res, st, nil)
		return res, st
	}
	describe := func(plan []*batch) string {
		var out []string
		for _, b := range plan {
			out = append(out, fmt.Sprintf("%v untraced=%v full=%v", b.units, b.untraced, b.full))
		}
		return strings.Join(out, "; ")
	}
	for _, c := range []struct {
		name     string
		untraced []string
		record   bool
		flags    runFlags
		want     string
	}{
		{name: "no untraced", record: true,
			want: "[test/a.test.ts test/b.test.ts test/perf.test.ts] untraced=false full=true"},
		{name: "untraced while recording", untraced: []string{"test/perf.*"}, record: true,
			want: "[test/a.test.ts test/b.test.ts] untraced=false full=false; [test/perf.test.ts] untraced=true full=false"},
		{name: "untraced without recording", untraced: []string{"test/perf.*"},
			want: "[test/a.test.ts test/b.test.ts test/perf.test.ts] untraced=false full=true"},
		{name: "every unit untraced", untraced: []string{"test/**"}, record: true,
			want: "[test/a.test.ts test/b.test.ts test/perf.test.ts] untraced=true full=true"},
		{name: "fail-fast", untraced: []string{"test/perf.*"}, record: true, flags: runFlags{failFast: true},
			want: "[test/a.test.ts test/b.test.ts] untraced=false full=false; [test/perf.test.ts] untraced=true full=false"},
		{name: "max-time", untraced: []string{"test/perf.*"}, record: true, flags: runFlags{maxTime: time.Hour},
			want: "[test/a.test.ts test/b.test.ts] untraced=false full=false; [test/perf.test.ts] untraced=true full=false"},
	} {
		t.Run(c.name, func(t *testing.T) {
			res, st := newSuite(c.untraced)
			flags := c.flags
			cyc := &cycle{p: &project{state: st}, f: &flags, sel: &selection{res: res}, runAll: true, record: c.record}
			if got := describe(cyc.plan()); got != c.want {
				t.Errorf("plan:\n got %s\nwant %s", got, c.want)
			}
		})
	}
}

// TestUntrustedWarningUntraced: untraced passes that cannot be trusted are named apart
// from browser passes, each with its reason.
func TestUntrustedWarningUntraced(t *testing.T) {
	sr := &selector.Suite{
		Name:  "web",
		Suite: &runner.Suite{Suite: config.Suite{Name: "web", Untraced: []string{"test/perf.test.ts"}}},
		Files: map[string][]string{"test/perf.test.ts": {"test/perf.test.ts"}, "src/a.stories.tsx": {"src/a.stories.tsx"}},
		Untrusted: map[string]string{
			"test/perf.test.ts": `src/grid.ts imports "node:child_process", which may run code no trace sees without coverage`,
			"src/a.stories.tsx": "static analysis is off",
		},
	}
	pass := &runner.Observation{Status: state.Pass, StaticOnly: true}
	got := untrustedWarning(sr, map[string]*runner.Observation{"test/perf.test.ts": pass})
	for _, w := range []string{"1 untraced test(s) passed but smates cannot tell all they depend on", "test/perf.test.ts (src/grid.ts imports"} {
		if !strings.Contains(got, w) {
			t.Errorf("warning %q lacks %q", got, w)
		}
	}
	if strings.Contains(got, "browser") {
		t.Errorf("warning %q names browser tests", got)
	}
	both := untrustedWarning(sr, map[string]*runner.Observation{"test/perf.test.ts": pass, "src/a.stories.tsx": pass})
	if !strings.Contains(both, "1 browser test(s)") || !strings.Contains(both, "1 untraced test(s)") {
		t.Errorf("warning %q", both)
	}
}

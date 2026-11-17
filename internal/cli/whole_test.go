package cli

import (
	"context"
	"fmt"
	"io"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/dotneet/smates/internal/config"
	"github.com/dotneet/smates/internal/gitx"
	"github.com/dotneet/smates/internal/runner"
	"github.com/dotneet/smates/internal/selector"
	"github.com/dotneet/smates/internal/state"
)

// wholeUnits are the units of the suites of TestWholeSuiteNeverSplit, in their order.
var wholeUnits = []string{"src/a.stories.tsx", "src/b.stories.tsx", "src/c.test.ts", "src/perf.stories.tsx"}

// wholeFixture is a selection of one suite with the units wholeUnits, all selected (as
// Result.WholeSuites leaves it), with mixed priorities and recorded durations (so that
// --fail-fast and --max-time split it). whole gives the suite a project whose test
// files smates cannot list (runner.Matcher.Whole). selectAll false leaves only the
// first unit selected (a shadow run then runs the rest too).
func wholeFixture(whole, selectAll bool, untraced []string, reqs []config.Requirement) (*selector.Result, *state.State) {
	su := &runner.Suite{Suite: config.Suite{Name: "web", Root: ".", Untraced: untraced, Requires: reqs}}
	if whole {
		su.Discovered = []runner.Matcher{{Base: ".", Tests: []string{"**/*.stories.*"}, Browser: true, Whole: true}}
	}
	sr := &selector.Suite{Name: "web", All: selectAll, Suite: su, Files: map[string][]string{}}
	entries := map[string]*state.Entry{}
	for i, id := range wholeUnits {
		reason := selector.ReasonAll
		if i == 1 {
			reason = selector.ReasonNew
		}
		t := &selector.Test{ID: id, Selected: selectAll || i == 0, Reason: reason, Duration: int64(20_000 + 1000*i)}
		sr.Tests = append(sr.Tests, t)
		sr.Files[id] = []string{id}
		if i != 1 {
			status := state.Pass
			if i == 0 {
				status = state.Fail // runs first, alone under --fail-fast
			}
			entries[id] = &state.Entry{Status: status, DurationMs: t.Duration}
		}
	}
	if !selectAll {
		sr.Tests[0].Reason = selector.ReasonChanged
	}
	res := &selector.Result{Mode: selector.ModeState, Suites: []*selector.Suite{sr}}
	st := &state.State{Suites: map[string]*state.Suite{"web": {Tests: entries}}}
	prioritize(res, st, nil)
	return res, st
}

// TestWholeSuiteNeverSplit: a suite that must run whole (a project whose test files
// smates cannot list) runs in one unfiltered batch however the run is cut: --shard
// gives all of it to one shard, --fail-fast and --max-time do not split it, untraced
// units do not take a batch of their own, and unmet requirements defer all of it. A
// suite without such a project is cut as usual.
func TestWholeSuiteNeverSplit(t *testing.T) {
	type run struct {
		shard    int
		units    []string
		full     bool
		untraced bool
	}
	unmetReq := []config.Requirement{{File: "missing-file-for-the-test", Tests: []string{"src/c.*"}}}
	for _, tc := range []struct {
		name       string
		flags      runFlags
		untraced   []string
		reqs       []config.Requirement
		record     bool
		partialSel bool // only the first unit selected (with --shadow)
		// normal is the plan of the suite without an unlistable project ("shard:
		// units full" per batch); whole runs every unit in one full batch (on one shard).
		normal string
		// wholeUnmet: requirements leave the whole suite out.
		wholeUnmet  bool
		normalUnmet []string
	}{
		{name: "plain", normal: "1:[src/a.stories.tsx src/c.test.ts src/perf.stories.tsx src/b.stories.tsx] true"},
		{name: "fail-fast", flags: runFlags{failFast: true},
			normal: "1:[src/a.stories.tsx] false; 1:[src/c.test.ts src/perf.stories.tsx src/b.stories.tsx] false"},
		{name: "max-time", flags: runFlags{maxTime: time.Minute},
			normal: "1:[src/a.stories.tsx] false; 1:[src/c.test.ts] false; 1:[src/perf.stories.tsx] false; 1:[src/b.stories.tsx] false"},
		{name: "untraced", untraced: []string{"src/perf.*"}, record: true,
			normal: "1:[src/a.stories.tsx src/c.test.ts src/b.stories.tsx] false; 1:[src/perf.stories.tsx] false"},
		{name: "requires unmet", reqs: unmetReq, wholeUnmet: true, normalUnmet: []string{"src/c.test.ts"},
			normal: "1:[src/a.stories.tsx src/perf.stories.tsx src/b.stories.tsx] false"},
		{name: "shard", flags: runFlags{shardN: 3}, normal: "sharded"},
		{name: "shadow shard", flags: runFlags{shardN: 3, shadow: true}, partialSel: true, normal: "sharded"},
		{name: "everything at once", flags: runFlags{shardN: 2, failFast: true, maxTime: time.Minute}, untraced: []string{"src/perf.*"}, record: true, normal: "sharded"},
	} {
		for _, whole := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s/whole=%t", tc.name, whole), func(t *testing.T) {
				var runs []run
				var unmet []string
				shards := max(tc.flags.shardN, 1)
				for i := 1; i <= shards; i++ {
					res, st := wholeFixture(whole, !tc.partialSel, tc.untraced, tc.reqs)
					flags := tc.flags
					if flags.shardN > 1 {
						flags.shardI = i
					}
					applyShard(res, flags.shardI, flags.shardN, flags.shadow)
					c := &cycle{p: &project{state: st, repo: &gitx.Repo{Root: t.TempDir()}, log: io.Discard}, f: &flags, sel: &selection{res: res},
						record: tc.record, checked: map[string]error{}}
					c.runAll = flags.shadow && flags.shardN <= 1
					batches := c.plan()
					c.units.plan(batches)
					for _, b := range batches {
						if c.deferUnmet(context.Background(), b) {
							continue
						}
						runs = append(runs, run{shard: i, units: b.units, full: b.full, untraced: b.untraced})
					}
					unmet = append(unmet, c.units.in("web", unitUnmet)...)
				}
				var got []string
				for _, r := range runs {
					got = append(got, fmt.Sprintf("%d:%v %t", r.shard, r.units, r.full))
				}
				switch {
				case whole && tc.wholeUnmet:
					slices.Sort(unmet)
					if len(runs) != 0 || !slices.Equal(unmet, wholeUnits) {
						t.Errorf("must defer the whole suite: ran %v, unmet %v", got, unmet)
					}
				case whole:
					if len(runs) != 1 || !runs[0].full {
						t.Fatalf("must run in one unfiltered batch on one shard, got %v", got)
					}
					units := slices.Sorted(slices.Values(runs[0].units))
					if !slices.Equal(units, wholeUnits) {
						t.Errorf("the batch runs %v, want every unit %v", units, wholeUnits)
					}
					// Must keep: units that must run untraced keep running untraced, so
					// the whole batch does.
					if want := len(tc.untraced) > 0; runs[0].untraced != want {
						t.Errorf("the batch runs untraced=%t, want %t", runs[0].untraced, want)
					}
				case tc.normal == "sharded":
					// Cut across the shards, each unit once, none run unfiltered.
					var all []string
					for _, r := range runs {
						if r.full {
							t.Errorf("shard %d runs the suite unfiltered: %v", r.shard, got)
						}
						all = append(all, r.units...)
					}
					slices.Sort(all)
					if !slices.Equal(all, wholeUnits) {
						t.Errorf("the shards run %v, want each of %v once", all, wholeUnits)
					}
				default:
					if s := strings.Join(got, "; "); s != tc.normal {
						t.Errorf("plan:\n got %s\nwant %s", s, tc.normal)
					}
					if !slices.Equal(unmet, tc.normalUnmet) {
						t.Errorf("unmet %v, want %v", unmet, tc.normalUnmet)
					}
				}
			})
		}
	}
}

// TestApplyShardWholeSuite: every shard agrees on the one shard that owns a suite that
// must run whole; the others neither select nor own any of its tests. A suite of the
// same shape whose files can be listed is still split by test.
func TestApplyShardWholeSuite(t *testing.T) {
	for _, n := range []int{2, 3, 5} {
		owners := map[int]bool{}
		for i := 1; i <= n; i++ {
			res, _ := wholeFixture(true, true, nil, nil)
			applyShard(res, i, n, false)
			sr := res.Suites[0]
			for _, tt := range sr.Tests {
				owners[tt.Shard] = true
			}
			switch sel := sr.Selected(); {
			case i == wholeShard("web", n) && (len(sel) != len(wholeUnits) || !sr.All):
				t.Errorf("n=%d: the owning shard %d selects %v (all %t)", n, i, sel, sr.All)
			case i != wholeShard("web", n) && (len(sel) != 0 || sr.All):
				t.Errorf("n=%d: shard %d selects %v (all %t) of a suite another shard runs whole", n, i, sel, sr.All)
			}
		}
		if len(owners) != 1 {
			t.Errorf("n=%d: owners %v, want one", n, owners)
		}
	}
}

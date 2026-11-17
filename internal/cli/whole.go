package cli

import (
	"hash/fnv"

	"github.com/dotneet/smates/internal/runner"
	"github.com/dotneet/smates/internal/selector"
)

// ---- suites that must run whole ----
//
// A suite with a project whose test files smates cannot list (runner.Matcher.Whole: a
// Storybook project whose stories it cannot read) has guessed units: the runner may run
// tests no unit names. Selection then selects the whole suite (Result.WholeSuites), and
// only the runner run unfiltered (runner.RunContext.All) is sure to run them all. Such
// a suite is never split: not into batches (--fail-fast, --max-time, untraced units),
// not across shards (one shard runs all of it, wholeShard), and requirements defer all
// of it or none (deferUnmet). Everything that decides how a suite runs goes through
// mustRunWhole and cycle.suiteRun.

// wholeProject reports whether s has a project whose test files smates could not list
// (runner.Matcher.Whole).
func wholeProject(s *runner.Suite) bool {
	if s == nil {
		return false
	}
	for i := range s.Discovered {
		if s.Discovered[i].Whole {
			return true
		}
	}
	return false
}

// mustRunWhole reports whether a selection runs every test of sr, a suite with a
// project whose test files smates could not list: it must then run unfiltered, all at
// once. everything says that every test runs whatever was selected (a shadow run).
func mustRunWhole(sr *selector.Suite, everything bool) bool {
	return len(sr.Tests) > 0 && wholeProject(sr.Suite) && (everything || sr.All)
}

// wholeShard is the shard (1-based, of n) that runs a suite that must run whole: a
// stable hash of the suite's name, so that every shard agrees on it without
// coordination, as for the units of other suites (applyShard).
func wholeShard(suite string, n int) int {
	h := fnv.New32a()
	h.Write([]byte("whole\x00" + suite))
	return int(h.Sum32()%uint32(n)) + 1
}

// suiteRun is how a cycle runs one suite (cycle.suiteRun).
type suiteRun struct {
	// units are the units the cycle runs.
	units []string
	// full: units are every test of the suite, so a batch holding all of them may let
	// the runner discover the tests itself (runner.RunContext.All).
	full bool
	// whole: the suite must run unfiltered (mustRunWhole): its units run in one batch
	// that is never split, and unmet requirements defer all of them.
	whole bool
}

// suiteRun decides how the cycle runs sr: which units, and whether they may or must
// run unfiltered. A full run (runAll) runs every test; a sharded shadow run every unit
// its shard owns, selected or not, so the shards of one run together run every test
// once; any other run the selected units. applyShard has already given a suite that
// must run whole to a single shard, all of it.
func (c *cycle) suiteRun(sr *selector.Suite) suiteRun {
	var ids []string
	if c.runAll || c.shadowShard() {
		ids = make([]string, 0, len(sr.Tests))
		for _, t := range sr.Tests {
			if c.runAll || t.Shard == c.f.shardI {
				ids = append(ids, t.ID)
			}
		}
	} else {
		ids = sr.Selected()
	}
	whole := len(ids) > 0 && len(ids) == len(sr.Tests) && wholeProject(sr.Suite)
	full := len(ids) > 0 && (c.runAll || whole || sr.All && c.f.shardN <= 1)
	return suiteRun{units: ids, full: full, whole: whole}
}

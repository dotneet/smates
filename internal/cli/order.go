package cli

import (
	"cmp"
	"slices"
	"time"

	"github.com/dotneet/smates/internal/depkey"
	"github.com/dotneet/smates/internal/selector"
	"github.com/dotneet/smates/internal/state"
)

// Priorities (selector.Test.Priority): the order in which `smates run` starts the tests
// it runs, the likeliest to fail first.
const (
	prioFailed   = 1 // did not pass last time
	prioDirect   = 2 // the test file or a file it imports directly changed
	prioIndirect = 3 // a transitive import, traced data or configuration changed; a reference or the agent
	prioOther    = 4 // new tests, full runs, reruns
)

// prioritize sets the priority of every selected test of res. direct lists the direct
// imports of a file (nil when there is no static graph).
func prioritize(res *selector.Result, st *state.State, direct func(string) []string) {
	for _, sr := range res.Suites {
		var tests map[string]*state.Entry
		if su := st.Suites[sr.Name]; su != nil {
			tests = su.Tests
		}
		for _, t := range sr.Tests {
			if t.Selected {
				t.Priority = priorityOf(sr, t, tests[t.ID], direct)
			}
		}
	}
}

// direct is the static graph's direct imports (nil without a graph).
func (p *project) direct() func(string) []string {
	if p.graph == nil {
		return nil
	}
	return p.graph.Direct
}

func priorityOf(sr *selector.Suite, t *selector.Test, e *state.Entry, direct func(string) []string) int {
	if t.Reason == selector.ReasonFailed || (e != nil && e.Status == state.Fail) {
		return prioFailed
	}
	switch t.Reason {
	case selector.ReasonChanged, selector.ReasonDiff:
		var imports map[string]bool
		for _, c := range t.Changed {
			switch t.Via[c] {
			case selector.ViaTest:
				return prioDirect
			case selector.ViaImport:
				if imports == nil {
					imports = map[string]bool{}
					for _, f := range unitFiles(sr, t.ID) {
						if direct != nil {
							for _, d := range direct(f) {
								imports[d] = true
							}
						}
					}
				}
				if f, ok := depkey.File(c); ok && imports[f] {
					return prioDirect
				}
			}
		}
		return prioIndirect
	case selector.ReasonReference, selector.ReasonAgent, selector.ReasonCaller, selector.ReasonOrphan:
		return prioIndirect
	}
	return prioOther
}

// rank is the priority a run uses for a test (unselected tests run by --shadow last).
func rank(t *selector.Test) int {
	if t.Priority == 0 {
		return prioOther
	}
	return t.Priority
}

// batch is one runner invocation: units of one suite, in the order they should start.
type batch struct {
	si    int // index of the suite in the selection
	sr    *selector.Suite
	units []string
	full  bool // the units are the whole suite (the runner may discover them itself)
	prio  int  // the best priority among the units
	// part and parts number the batches of a suite (1-based).
	part, parts int
	// est is how long the batch takes by recorded durations (--max-time); unknown when
	// its suite has none recorded.
	est     time.Duration
	unknown bool
}

// timeSlices is the number of batches a --max-time budget is cut into: a batch takes
// about a quarter of it, so the run stops close to the budget without starting a
// runner process for every unit.
const timeSlices = 4

// unitEstimates returns how long a unit of sr takes: its recorded duration, else the
// mean of the suite's recorded durations; ok is false when the suite has none.
func unitEstimates(sr *selector.Suite, recorded map[string]*state.Entry) func(string) (time.Duration, bool) {
	dur := map[string]time.Duration{}
	var sum time.Duration
	for _, t := range sr.Tests {
		if _, ok := recorded[t.ID]; ok {
			d := time.Duration(t.Duration) * time.Millisecond
			dur[t.ID] = d
			sum += d
		}
	}
	return func(u string) (time.Duration, bool) {
		if d, ok := dur[u]; ok {
			return d, true
		}
		if len(dur) == 0 {
			return 0, false
		}
		return sum / time.Duration(len(dur)), true
	}
}

// splitByTime cuts b into consecutive batches whose estimated durations add up to at
// most target each (at least one unit per batch), in b's order. Units of unknown
// duration end up in one batch of unknown duration.
func splitByTime(b *batch, est func(string) (time.Duration, bool), target time.Duration, byID map[string]*selector.Test) []*batch {
	var out []*batch
	for _, u := range b.units {
		d, ok := est(u)
		n := len(out)
		if n == 0 || out[n-1].unknown != !ok || ok && out[n-1].est+d > target {
			out = append(out, &batch{si: b.si, sr: b.sr, prio: rank(byID[u]), unknown: !ok})
		}
		last := out[len(out)-1]
		last.units = append(last.units, u)
		last.est += d
	}
	return out
}

// plan orders what the cycle runs. Units start by priority, then by recorded duration
// (shortest first, unrecorded last), and suites by their best priority. With
// --fail-fast a suite is split into one batch per priority, and batches run by
// priority across suites, so the likeliest failures finish before the rest starts.
// With --max-time the batches are cut further by recorded durations (splitByTime), so
// that the run can stop between them.
func (c *cycle) plan() []*batch {
	var out []*batch
	for si, sr := range c.sel.res.Suites {
		units, full := c.units(sr)
		if len(units) == 0 {
			continue
		}
		byID := make(map[string]*selector.Test, len(sr.Tests))
		for _, t := range sr.Tests {
			byID[t.ID] = t
		}
		var recorded map[string]*state.Entry
		if su := c.p.state.Suites[sr.Name]; su != nil {
			recorded = su.Tests
		}
		units = slices.Clone(units)
		slices.SortStableFunc(units, func(a, b string) int {
			ta, tb := byID[a], byID[b]
			_, ra := recorded[a]
			_, rb := recorded[b]
			return cmp.Or(
				cmp.Compare(rank(ta), rank(tb)),
				compareBool(!ra, !rb), // recorded durations first
				cmp.Compare(ta.Duration, tb.Duration),
				cmp.Compare(a, b),
			)
		})
		parts := []*batch{{si: si, sr: sr, units: units, prio: rank(byID[units[0]])}}
		if c.f.failFast {
			parts = nil
			for _, u := range units {
				p := rank(byID[u])
				if n := len(parts); n == 0 || parts[n-1].prio != p {
					parts = append(parts, &batch{si: si, sr: sr, prio: p})
				}
				b := parts[len(parts)-1]
				b.units = append(b.units, u)
			}
		}
		if c.f.maxTime > 0 {
			est := unitEstimates(sr, recorded)
			var split []*batch
			for _, b := range parts {
				split = append(split, splitByTime(b, est, c.f.maxTime/timeSlices, byID)...)
			}
			parts = split
		}
		for i, b := range parts {
			b.part, b.parts = i+1, len(parts)
			b.full = full && len(parts) == 1
		}
		out = append(out, parts...)
	}
	slices.SortStableFunc(out, func(a, b *batch) int {
		return cmp.Or(cmp.Compare(a.prio, b.prio), cmp.Compare(a.si, b.si))
	})
	return out
}

func compareBool(a, b bool) int {
	switch {
	case a == b:
		return 0
	case !a:
		return -1
	}
	return 1
}

// prioLabel describes a priority for the run log.
func prioLabel(p int) string {
	switch p {
	case prioFailed:
		return "failed last time"
	case prioDirect:
		return "test or direct import changed"
	case prioIndirect:
		return "indirect dependency changed"
	}
	return "new or other"
}

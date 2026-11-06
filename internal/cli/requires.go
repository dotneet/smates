package cli

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/dotneet/smates/internal/config"
	"github.com/dotneet/smates/internal/runner"
	"github.com/dotneet/smates/internal/selector"
)

// ---- requires and --max-time: selected units a run leaves for later ----
//
// Units a run does not start because the environment lacks what they require (a suite's
// requires) or because --max-time is reached are neither run nor recorded: they stay
// selected, so the next run (or CI) runs them. Neither ever records a pass.

// requiresError is the error of a run that left units out because requirements they
// need are not met (wrapped in an envError: exit 4 when no test failed). The coding
// agent hooks tell it apart from other environment errors and only note it.
type requiresError struct{ error }

func (e requiresError) Unwrap() error { return e.error }

// overTime reports whether batch b would end after --max-time, by the recorded
// durations: the first batch that starts always runs, so every run makes progress; a
// batch of unknown duration (its suite has none recorded) starts while less than half
// the time is used.
func (c *cycle) overTime(b *batch) bool {
	if c.f.maxTime <= 0 || countUnits(c.started) == 0 {
		return false
	}
	elapsed := time.Since(c.start)
	if b.unknown {
		return elapsed >= c.f.maxTime/2
	}
	return elapsed+b.est > c.f.maxTime
}

// unitsFrom counts the units of batches.
func (c *cycle) unitsFrom(batches []*batch) int {
	n := 0
	for _, b := range batches {
		n += len(b.units)
	}
	return n
}

// deferUnmet takes out of batch b the units that need a requirement (requires) that is
// not met: they do not start and stay selected. A check that cannot tell (it fails to
// run, times out) counts as unmet. It reports whether nothing is left to run.
func (c *cycle) deferUnmet(ctx context.Context, b *batch) bool {
	sr := b.sr
	reqs := sr.Suite.Requires
	if len(reqs) == 0 {
		return false
	}
	var keep, left []string
	failed := map[int]error{}
	for _, u := range b.units {
		met := true
		for i, r := range reqs {
			if len(r.Tests) > 0 && !selector.UnitMatches(sr.Suite, u, unitFiles(sr, u), r.Tests) {
				continue
			}
			if err := c.checkRequirement(ctx, sr.Suite, r); err != nil {
				failed[i], met = err, false
			}
		}
		if met {
			keep = append(keep, u)
		} else {
			left = append(left, u)
		}
	}
	if ctx.Err() != nil {
		// Cancelled while checking: like a batch cut short.
		c.interrupted, c.stopped = true, true
		c.notRun[sr.Name] = append(c.notRun[sr.Name], b.units...)
		return true
	}
	if len(left) == 0 {
		return false
	}
	var why []string
	for i, r := range reqs {
		if err, ok := failed[i]; ok {
			why = append(why, fmt.Sprintf("%s (%v)", r, err))
		}
	}
	c.p.logf("%s: not runnable here: requires %s; %d test unit(s) not run (they stay selected)", sr.Name, strings.Join(why, ", "), len(left))
	c.unstarted[sr.Name] = append(c.unstarted[sr.Name], left...)
	c.notRun[sr.Name] = append(c.notRun[sr.Name], left...)
	c.unmet[sr.Name] = append(c.unmet[sr.Name], left...)
	for _, w := range why {
		if !slices.Contains(c.unmetWhy[sr.Name], w) {
			c.unmetWhy[sr.Name] = append(c.unmetWhy[sr.Name], w)
		}
	}
	b.units, b.full = keep, false
	return len(keep) == 0
}

// checkRequirement checks r for suite s once per run (the same check of suites with
// the same root and env is shared).
func (c *cycle) checkRequirement(ctx context.Context, s *runner.Suite, r config.Requirement) error {
	key := fmt.Sprint(s.Root, "\x00", r.String(), "\x00", r.TimeoutOrDefault(), "\x00", s.Env)
	if err, ok := c.checked[key]; ok {
		return err
	}
	err := runner.CheckRequirement(ctx, c.p.repo.Root, s, r)
	if ctx.Err() == nil {
		c.checked[key] = err
	}
	return err
}

// unmetError is the error the run reports for the units left out by unmet
// requirements (nil when there are none).
func (c *cycle) unmetError() error {
	if len(c.unmet) == 0 {
		return nil
	}
	var parts []string
	for _, sr := range c.sel.res.Suites {
		if units := c.unmet[sr.Name]; len(units) > 0 {
			parts = append(parts, fmt.Sprintf("%s: %d test unit(s) need %s", sr.Name, len(units), strings.Join(c.unmetWhy[sr.Name], ", ")))
		}
	}
	return envError{requiresError{fmt.Errorf("requirements not met, so these tests did not run and stay selected: %s", strings.Join(parts, "; "))}}
}

// deferredLine is one line saying which selected units the run left for later, and
// why: --max-time, unmet requirements ("" when it left none).
func (c *cycle) deferredLine() string {
	var parts []string
	if n := countUnits(c.notRun) - countUnits(c.unstarted); c.capped && n > 0 {
		parts = append(parts, fmt.Sprintf("%d by --max-time %s", n, c.f.maxTime))
	}
	for _, sr := range c.sel.res.Suites {
		if units := c.unmet[sr.Name]; len(units) > 0 {
			parts = append(parts, fmt.Sprintf("%d of %s, which needs %s", len(units), sr.Name, strings.Join(c.unmetWhy[sr.Name], ", ")))
		}
	}
	if len(parts) == 0 {
		return ""
	}
	return "smates: selected test units left for a later run (they stay selected): " + strings.Join(parts, "; ")
}

// joinAnd joins words as "a", "a and b", "a, b and c".
func joinAnd(words []string) string {
	if len(words) <= 1 {
		return strings.Join(words, "")
	}
	return strings.Join(words[:len(words)-1], ", ") + " and " + words[len(words)-1]
}

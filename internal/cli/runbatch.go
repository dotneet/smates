package cli

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/dotneet/smates/internal/runner"
	"github.com/dotneet/smates/internal/selector"
	"github.com/dotneet/smates/internal/state"
	"github.com/dotneet/smates/internal/trace"
)

// prepareTrace creates the trace directory and extracts the shims into it. cleanup
// removes it (unless SMATES_KEEP_TRACE is set).
func (c *cycle) prepareTrace() (cleanup func(), err error) {
	root, err := os.MkdirTemp("", "smates-trace-")
	if err != nil {
		return nil, err
	}
	cleanup = func() { os.RemoveAll(root) }
	if os.Getenv("SMATES_KEEP_TRACE") != "" {
		c.p.logf("keeping trace files in %s", root)
		cleanup = func() {}
	}
	c.traceRoot, c.shimDir = root, filepath.Join(root, "shims")
	if _, err := trace.ExtractShims(c.shimDir); err != nil {
		cleanup()
		return nil, err
	}
	return cleanup, nil
}

func (c *cycle) runContext(sr *selector.Suite, units []string, full bool) *runner.RunContext {
	rc := &runner.RunContext{
		Repo: c.p.repo, Suite: sr.Suite, Units: units, All: full && !sr.Suite.Narrowed, Record: c.record, Fresh: c.f.shadow, ExtraArgs: c.f.extra,
		ShimDir: c.shimDir, Self: c.self, Stdout: c.stdout, Stderr: c.stderr,
		IsTest: sr.Suite.IsTestFile,
	}
	unitOf := unitIndex(sr)
	rc.UnitOf = func(f string) string { return unitOf[f] }
	rc.Known = func(u string) []string { return sr.Known[u] }
	return rc
}

// suiteResult collects the outcomes of a suite's batches until they are recorded.
type suiteResult struct {
	obs     map[string]*runner.Observation
	readsBy map[string]map[string]bool
	retried map[string]*runner.Observation
	// full: the suite runs every test (its entries are replaced when every batch of it
	// ran).
	full bool
}

// runBatch runs the i-th batch and collects its outcomes. It returns the batch's exit
// code (after retries of known flaky tests).
func (c *cycle) runBatch(ctx context.Context, i int, b *batch) (int, error) {
	p, sr := c.p, b.sr
	rc := c.runContext(sr, b.units, b.full)
	if b.untraced {
		// Without code coverage where the runner supports it, else without recording.
		rc.Untraced = runner.TracesUntraced(sr.Suite.Runner)
		rc.Record = rc.Untraced
	}
	rc.TraceDir = filepath.Join(c.traceRoot, fmt.Sprintf("batch-%d", i))
	if err := os.MkdirAll(rc.TraceDir, 0o755); err != nil {
		return 0, envError{fmt.Errorf("%s: preparing the trace directory: %w", sr.Name, err)}
	}
	var buf *outputBuffer
	switch {
	case c.quiet:
		buf = &outputBuffer{}
		rc.Stdout, rc.Stderr = buf, buf
	case b.parts > 1:
		p.logf("%s: running %d test unit(s) with %s%s (batch %d of %d: %s)", sr.Name, len(b.units), sr.Suite.Runner, untracedNote(b, rc), b.part, b.parts, prioLabel(b.prio))
	default:
		p.logf("%s: running %d test unit(s) with %s%s", sr.Name, len(b.units), sr.Suite.Runner, untracedNote(b, rc))
	}
	if err := runner.Preflight(p.repo.Root, sr.Suite); err != nil {
		return 0, envError{fmt.Errorf("%s: %w", sr.Name, err)}
	}
	c.started[sr.Name] = append(c.started[sr.Name], b.units...)
	stop := c.startHeartbeat(sr.Name, countUnits(c.started), c.total, c.start)
	r, err := sr.Suite.Adapter.Run(ctx, rc)
	stop()
	if ctx.Err() != nil {
		c.interrupt(sr, b, buf)
		return 0, nil
	}
	if err != nil {
		if buf != nil {
			c.printFailedBatch(sr, b, buf, nil)
		}
		return 0, runError(sr.Name, err)
	}
	var retried map[string]*runner.Observation
	if c.record && len(c.f.extra) == 0 {
		r, retried, err = retryFlaky(ctx, p, rc, sr, r)
		if ctx.Err() != nil {
			c.interrupt(sr, b, buf)
			return 0, nil
		}
		if err != nil {
			if buf != nil {
				c.printFailedBatch(sr, b, buf, nil)
			}
			return 0, runError(sr.Name, err)
		}
	}
	// A launcher (sh, npm, yarn, pnpm) exits 126 or 127 when the command it runs is not
	// executable or not installed: when tests that report results reported none, they
	// never ran. The batch is still handled as a failure below.
	noResult := notStarted(sr.Suite.Runner, rc.Record, r)
	if !noResult {
		// Any other failing batch failed tests, even when no unit is known to have
		// failed (TestMain exiting 1, a teardown error, a coverage threshold).
		c.testsFailed = c.testsFailed || r.ExitCode != 0
	} else if c.envErr == nil {
		c.envErr = envError{fmt.Errorf("%s: the runner exited with code %d before any test reported a result: a command it runs is probably not installed or not executable (see its output)", sr.Name, r.ExitCode)}
	}
	c.worst = max(c.worst, r.ExitCode)
	for _, w := range r.Warnings {
		p.logf("%s: %s", sr.Name, w)
	}
	unreported := unreportedUnits(r, b.units)
	settleOutcomes(r, b.units, rc.Record, len(c.f.extra) > 0)
	if b.untraced && c.record {
		// Recorded from their known dependencies (selector.Record): no coverage, or no
		// trace at all, saw the code they ran.
		for _, o := range r.Observations {
			o.StaticOnly = true
		}
	}
	selector.DropIgnoredEnv(p.cfg, sr.Suite, r)
	if c.ran[sr.Name] == nil {
		c.ran[sr.Name] = map[string]string{}
	}
	if c.inferred[sr.Name] == nil {
		c.inferred[sr.Name] = map[string]bool{}
	}
	anyFail := false
	for u, o := range r.Observations {
		c.ran[sr.Name][u] = o.Status
		c.inferred[sr.Name][u] = o.Inferred
		anyFail = anyFail || o.Status == state.Fail
	}
	if r.ExitCode != 0 {
		c.failedBatches = append(c.failedBatches, failedBatch{suite: sr.Name, units: b.units, anyFail: anyFail, noResult: noResult})
		if buf != nil {
			c.printFailedBatch(sr, b, buf, r.Observations)
		}
	}
	if c.f.shadow {
		c.checkShadow(sr, r.Observations, unreported)
	}
	c.failures.add(sr, r.Observations) // before recording: compares with the previous entries
	if c.record {
		// A runner that cannot trace untraced units without coverage runs them without
		// recording: no trace is expected (their passes stay unknown, Suite.Untrusted
		// says why).
		if w := noTraceWarning(b.units, r, sr.Suite.Runner, c.sel.diffUnrecorded); w != "" && rc.Record {
			p.logf("%s: %s", sr.Name, w)
		} else if missing := len(b.units) - len(r.Observations); missing > 0 && r.ExitCode == 0 {
			p.debugf("%s: %d unit(s) produced no trace (kept as not recorded)", sr.Name, missing)
		}
		res := c.results[sr.Name]
		if res == nil {
			res = &suiteResult{obs: map[string]*runner.Observation{}, readsBy: map[string]map[string]bool{}, retried: map[string]*runner.Observation{}}
			_, res.full = c.units(sr)
			c.results[sr.Name] = res
		}
		maps.Copy(res.obs, r.Observations)
		maps.Copy(res.retried, retried)
		for f, keys := range r.ReadsBy {
			if res.readsBy[f] == nil {
				res.readsBy[f] = map[string]bool{}
			}
			maps.Copy(res.readsBy[f], keys)
		}
	}
	return r.ExitCode, nil
}

// interrupt handles a batch cut short by cancellation (Ctrl-C): its outcomes are
// incomplete (a killed runner fails its tests or reports none), so none is recorded
// and its units count as not run, like those of the batches after it.
func (c *cycle) interrupt(sr *selector.Suite, b *batch, buf *outputBuffer) {
	if buf != nil {
		c.printFailedBatch(sr, b, buf, nil)
	}
	c.interrupted, c.stopped = true, true
	c.notRun[sr.Name] = append(c.notRun[sr.Name], b.units...)
}

// untracedNote is what the run log adds for a batch of untraced units: how they run.
func untracedNote(b *batch, rc *runner.RunContext) string {
	switch {
	case !b.untraced:
		return ""
	case rc.Untraced:
		return " without code coverage (untraced)"
	}
	return " without tracing (untraced)"
}

// untrustedWarning says which tests recorded from their known dependencies alone
// (browser tests with trust_browser_passes, the units untraced lists) passed but are
// not recorded as passed, because those may not be all they use (see
// Suite.Untrusted), and why, for a few of them: they stay unknown and run again next
// time ("" when none).
func untrustedWarning(sr *selector.Suite, obs map[string]*runner.Observation) string {
	var browser, untraced []string
	for u, o := range obs {
		if o.StaticOnly && o.Status == state.Pass && sr.Untrusted[u] != "" {
			if selector.UntracedUnit(sr.Suite, u, sr.Files[u]) {
				untraced = append(untraced, u)
			} else {
				browser = append(browser, u)
			}
		}
	}
	var parts []string
	if len(browser) > 0 {
		parts = append(parts, untrustedList(sr, browser, "browser test(s) passed but are not trusted (trust_browser_passes)"))
	}
	if len(untraced) > 0 {
		parts = append(parts, untrustedList(sr, untraced, "untraced test(s) passed but smates cannot tell all they depend on without coverage"))
	}
	return strings.Join(parts, "; ")
}

// untrustedList is one sentence of untrustedWarning: the number of units, what, and a
// few of them with the reason.
func untrustedList(sr *selector.Suite, units []string, what string) string {
	slices.Sort(units)
	const shown = 3
	var b strings.Builder
	fmt.Fprintf(&b, "%d %s, so they will run again next time:", len(units), what)
	for _, u := range units[:min(shown, len(units))] {
		fmt.Fprintf(&b, " %s (%s);", u, sr.Untrusted[u])
	}
	if len(units) > shown {
		fmt.Fprintf(&b, " and %d more", len(units)-shown)
	}
	return strings.TrimSuffix(b.String(), ";")
}

// settleOutcomes corrects the outcomes a run reported for its units before they are
// recorded: record says the run was traced, extraArgs that arguments were passed
// through to the runner.
func settleOutcomes(r *runner.RunResult, units []string, record, extraArgs bool) {
	if r.Observations == nil {
		r.Observations = map[string]*runner.Observation{}
	}
	// Units that ran but left no trace (or, without recording, no reported status) when
	// the run failed (build or collection failure, a global setup error, an aborted
	// session) have an unknown outcome: they are retried next time.
	if r.ExitCode != 0 {
		for _, u := range units {
			if r.Observations[u] == nil {
				r.Observations[u] = &runner.Observation{Status: state.Unknown, Inferred: true}
			}
		}
	}
	// A failing run in which no unit failed (a crash after the tests, a coverage
	// threshold, a teardown error) must not record passes.
	if r.ExitCode != 0 {
		anyFail := false
		for _, o := range r.Observations {
			anyFail = anyFail || o.Status == state.Fail
		}
		if !anyFail {
			for _, o := range r.Observations {
				if o.Status == state.Pass {
					o.Status, o.Inferred = state.Unknown, true
				}
			}
		}
	}
	// Arguments passed through to the runner may filter tests (-t, -k, -run,
	// --test-name-pattern, --bail): a file that passed may not have run fully.
	if extraArgs {
		for _, o := range r.Observations {
			if o.Status == state.Pass {
				o.Status = state.Unknown
			}
		}
	}
}

// noTraceWarning warns about a recorded batch that passed while none of its units
// left a trace or reported a result: they stay unrecorded, and selection never learns
// what they depend on. What happens to them next depends on how unrecorded tests are
// decided (diffUnrecorded: against the base revision by static analysis, which misses
// what they read at run time; otherwise they run again as new). A custom suite is
// traced only through the Node and Python processes its command starts: one whose
// command starts none (a shell script, a JVM build) and writes no junit report can
// never be recorded, which is said instead of blaming a wrapper.
func noTraceWarning(units []string, r *runner.RunResult, runnerName string, diffUnrecorded bool) string {
	if r.ExitCode != 0 || len(units) == 0 || len(r.Observations) > 0 {
		return ""
	}
	next := "run again next time"
	if diffUnrecorded {
		next = "are decided against the base revision by static analysis next time, which does not see what they read at run time"
	}
	why := runner.NoTraceCauses
	if runnerName == "custom" {
		why = "a custom suite is recorded through the Node and Python processes its command starts, or a junit: report; this command left neither"
	}
	return fmt.Sprintf("%d test unit(s) ran and passed but left no trace, so they stay unrecorded and %s: %s", len(units), next, why)
}

// unreportedUnits returns the units of a failed run that nothing was observed for
// (no trace, no reported status): any of them may have failed.
func unreportedUnits(r *runner.RunResult, units []string) map[string]bool {
	out := map[string]bool{}
	if r.ExitCode == 0 {
		return out
	}
	for _, u := range units {
		if r.Observations[u] == nil {
			out[u] = true
		}
	}
	return out
}

// checkShadow notes the failing tests of a suite the selection would have skipped
// (--shadow). Only failures reported for the unit itself count: a failure inferred
// from a shared exit code, or a unit a failed run said nothing about, may belong to a
// selected test (they are possible escapes). A Go test function of a package whose
// compile check was selected and failed is no escape: it failed because its package
// does not build, which the check reports.
//
// A sharded shadow run judges only the units its shard owns, against the selection
// before sharding (preShard). A unit another shard owns is that shard's to report,
// whatever the runner said about it here. A function whose package's selected compile
// check another shard owns may have failed because the package does not build, which
// that shard reports: such a failure (the package did not build here either) is a
// possible escape. A function that ran and failed on its own is an escape.
func (c *cycle) checkShadow(sr *selector.Suite, obs map[string]*runner.Observation, unreported map[string]bool) {
	owned := func(t *selector.Test) bool { return !c.shadowShard() || t.Shard == c.f.shardI }
	selected := func(t *selector.Test) bool { return t.Selected || c.preShard[sr.Name][t.ID] }
	brokenBuild := map[string]bool{} // package directories whose selected check failed
	elsewhere := map[string]bool{}   // package directories whose selected check another shard runs
	for _, t := range sr.Tests {
		dir, _, ok := runner.SplitFuncUnit(t.ID)
		if !ok || !selected(t) || !runner.IsBuildCheck(t.ID) {
			continue
		}
		switch {
		case !owned(t):
			elsewhere[dir] = true
		case obs[t.ID] != nil && obs[t.ID].Status == state.Fail:
			brokenBuild[dir] = true
		}
	}
	for _, t := range sr.Tests {
		o := obs[t.ID]
		if selected(t) || !owned(t) || o == nil || (o.Status != state.Fail && !unreported[t.ID]) {
			continue
		}
		dir, _, isFunc := runner.SplitFuncUnit(t.ID)
		if isFunc && brokenBuild[dir] {
			continue
		}
		if o.Inferred || isFunc && elsewhere[dir] && o.BuildFailed {
			c.possible = append(c.possible, sr.Name+": "+t.ID)
		} else {
			c.escapes = append(c.escapes, sr.Name+": "+t.ID)
		}
	}
}

// notStarted reports a batch whose process exited 126 (not executable) or 127
// (command not found) although none of its tests reported a result, while they would
// have: the run is recorded and the runner (any but custom) reports per-test results.
// A custom runner, or a run with --no-record, may report nothing even when its tests
// ran: its exit code is a test failure.
func notStarted(runnerName string, record bool, r *runner.RunResult) bool {
	return (r.ExitCode == 126 || r.ExitCode == 127) && len(r.Observations) == 0 && record && runnerName != "custom"
}

// runError classifies an error of a runner that could not run a batch: a suite command
// that cannot be parsed is a configuration error, anything else (a runner or tool that
// is not installed or cannot start) an environment problem.
func runError(suite string, err error) error {
	err = fmt.Errorf("%s: %w", suite, err)
	var ce *runner.CommandError
	if errors.As(err, &ce) {
		return configError{err}
	}
	return envError{err}
}

// unitIndex maps every test file of a suite to its unit.
func unitIndex(sr *selector.Suite) map[string]string {
	idx := map[string]string{}
	for _, t := range sr.Tests {
		for _, f := range unitFiles(sr, t.ID) {
			idx[f] = t.ID
		}
	}
	return idx
}

// unitFiles lists the test files of a unit (a unit is a test file, for Go a package
// directory whose test files are known from its deps, or a test function).
func unitFiles(sr *selector.Suite, unit string) []string {
	if files, ok := sr.Files[unit]; ok {
		return files
	}
	if sr.Suite.IsTestFile(unit) {
		return []string{unit}
	}
	var out []string
	for _, d := range sr.Known[unit] {
		if sr.Suite.IsTestFile(d) && filepath.ToSlash(filepath.Dir(d)) == unit {
			out = append(out, d)
		}
	}
	return out
}

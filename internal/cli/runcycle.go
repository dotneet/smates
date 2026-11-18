package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/dotneet/smates/internal/runner"
	"github.com/dotneet/smates/internal/selector"
)

// runCycle is `smates run`: select, run every suite, record the outcomes and report.
// hooks (nil for `smates run`) let `smates watch` drive it.
func runCycle(ctx context.Context, f *runFlags, stdout, stderr io.Writer, hooks *runHooks) (int, error) {
	// --report - writes the report to stdout: everything else goes to stderr.
	reportOut := stdout
	if f.report == "-" {
		stdout = stderr
	}
	cf := f.commonFlags
	if len(f.onlySpecs) > 0 {
		if len(cf.suites) == 0 {
			cf.suites = onlySuites(f.onlySpecs)
		} else {
			for _, s := range onlySuites(f.onlySpecs) {
				if !slices.Contains(cf.suites, s) {
					return 2, usageError{fmt.Errorf("--only names suite %q, which --suite leaves out", s)}
				}
			}
		}
	}
	p, err := load(&cf, stderr)
	if err != nil {
		return 1, err
	}
	if len(f.extra) > 0 && !f.noRecord {
		p.logf("runner arguments may filter tests: passing tests are not recorded as passed in this run")
	}
	for _, s := range p.suites {
		s.ExtraArgs = f.extra // they can change which test files compile (go -tags)
	}

	all := f.all
	if !all && !f.selectOn && !f.shadow && len(f.onlySpecs) == 0 {
		if full, branch := fullRunBranch(p.ci, p.repo, p.cfg.FullRunBranches); full {
			p.logf("on %s (%s): running every test to refresh the recorded state (use --select to override)", branch, p.ci.Name)
			all = true
		}
	}
	selectStart := time.Now()
	sel, err := p.selectTests(ctx, selectOptions{all: all, base: f.base, noAgent: f.noAgent, orphanScope: hooks.scope(), noOrphans: len(f.onlySpecs) > 0, orphans: f.orphans, adds: f.adds, sharded: f.shardN > 1})
	if err != nil {
		return 1, err
	}
	if len(f.onlySpecs) > 0 {
		if err := applyOnly(sel.res, f.onlySpecs); err != nil {
			return 1, err
		}
	}
	hooks.onSelected(p, sel)
	if hooks != nil {
		sel.hint = false // smates watch: a person at the terminal, no calling agent
	}
	var preShard map[string]map[string]bool
	if f.shadow && f.shardN > 1 {
		preShard = selectedUnits(sel.res)
	}
	applyShard(sel.res, f.shardI, f.shardN, f.shadow)
	selectTime := time.Since(selectStart)
	printSelection(p, sel, stderr, f.explain)

	c := &cycle{p: p, f: f, sel: sel, preShard: preShard, stdout: stdout, stderr: stderr, reportOut: reportOut, record: !f.noRecord, self: selfPath(),
		quiet: f.output == outputFailures, heartbeat: f.heartbeat,
		timings: runTimings{Discovery: p.discovery.Milliseconds(), StateLoad: p.stateLoad.Milliseconds(), Selection: selectTime.Milliseconds()},
		results: map[string]*suiteResult{}, unmetWhy: map[string][]string{}, checked: map[string]error{}}
	// In shadow mode the selection is only evaluated: everything runs. A full run that is
	// sharded runs only this shard's share (applyShard narrowed the selection), and so
	// does a sharded shadow run (suiteRun).
	c.runAll = (sel.res.Mode == selector.ModeAll || f.shadow) && f.shardN <= 1
	prioritize(sel.res, p.state, p.direct())
	batches := c.plan()
	c.units.plan(batches)
	c.logSkipped()
	if f.dryRun || (f.skipEmpty && len(batches) == 0) {
		if f.dryRun {
			c.printCommands(batches)
		}
		// The orphans handled count as seen only when the orphan policy itself left
		// nothing to run: not for a dry run (nothing ran, whatever was selected), a
		// shard owning none of the tests selected for them, or the caller's
		// --orphans override.
		markOrphans := !f.dryRun && len(batches) == 0 && f.shardN <= 1 && f.orphans == ""
		if err := c.saveWithoutRun(markOrphans); err != nil {
			p.logf("could not save the agent's answer: %v", err)
		}
		return 0, nil
	}
	if hooks == nil && f.maxTime <= 0 {
		c.hintLongRun(batches)
	}
	c.total = 0
	for _, b := range batches {
		c.total += len(b.units)
	}
	// Without recording the directory still receives the per-file results of runner
	// reporters.
	cleanup, err := c.prepareTrace()
	if err != nil {
		return 0, envError{fmt.Errorf("preparing the trace directory: %w", err)}
	}
	defer cleanup()
	c.now = time.Now().UTC()
	start := time.Now()
	c.start = start
	// Files git does not list are recorded as they were before the tests ran. Failure
	// hints hash what changed while the batches run, with a hasher of their own: what
	// they read from disk mid-run must not stand in for what Record reads after the
	// last batch, or the recorded hashes would depend on whether an earlier batch failed.
	c.hasher = p.runHasher(sel.res, start)
	c.failures = newFailureHints(ctx, p, sel.res, p.runHasher(sel.res, start), f.base, f.noAgent)
	executeStart := time.Now()
	for i, b := range batches {
		if !c.admit(ctx, b, batches[i:]) {
			continue
		}
		batchStart := time.Now()
		code, err := c.runBatch(ctx, i, b)
		c.addRan(b, time.Since(batchStart))
		if err != nil {
			// The runner could not start (not installed, preflight failed): like a
			// batch whose tests never started, an environment error reported after
			// the other batches ran and were recorded. Its units count as not run
			// (they stay selected). A suite command that cannot be parsed stops the
			// run (every later batch is not run): what ran is still recorded and
			// reported, then the configuration error is.
			var ce configError
			switch {
			case errors.As(err, &ce):
				c.configErr, c.stop = err, stopConfig
			case c.envErr == nil:
				c.envErr = orEnvError(err)
			default:
				p.logf("%v", err) // only the first is reported at the end
			}
			c.units.set(b.sr.Name, b.units, unitUnstarted)
			continue
		}
		if code != 0 && f.failFast && i < len(batches)-1 && c.stop == stopNone {
			c.stop = stopFailFast
			if !c.quiet {
				p.logf("--fail-fast: %s failed; starting no further test batch", b.sr.Name)
			}
		}
	}
	c.timings.Execution = time.Since(executeStart).Milliseconds()
	if c.interrupted() {
		// What ran is recorded and reported; the run itself failed.
		c.worst = max(c.worst, ExitFailed)
	}
	c.unmetErr = c.unmetError()
	hooks.onRan(c.units.startedUnits())
	c.failures.wait()
	elapsed := time.Since(start).Round(time.Millisecond)
	// A state that cannot be saved (e.g. the shared state stays locked by another
	// worktree's run), or a report that cannot be written, is an environment error,
	// reported after the run's summary so that the test results are not lost.
	end := c.end()
	if c.record {
		recordStart := time.Now()
		c.recordSuites() // before runRecord: it counts the flakes recording found
		p.state.AddRun(c.runRecord(elapsed, hooks != nil))
		saveStart := time.Now()
		c.timings.Record = saveStart.Sub(recordStart).Milliseconds()
		if err := c.saveState(); err != nil {
			end.saveErr = envError{err}
		}
		c.timings.StateSave = time.Since(saveStart).Milliseconds()
	}
	c.exit = reportedExit(end)
	if f.report != "" {
		if err := c.writeReport(); err != nil {
			end.reportErr = envError{fmt.Errorf("writing the report: %w", err)}
			c.exit = reportedExit(end)
		}
	}
	if c.quiet {
		c.logShadowAndFlakes()
		c.printAgentSummary(elapsed.String())
	} else {
		c.failures.print(stderr)
		c.logOutcome(elapsed)
	}
	c.writeSummaries(elapsed)
	if f.deferred != nil {
		if line := c.deferredLine(); line != "" {
			f.deferred(line)
		}
	}
	code, err, notes := resolveExit(end)
	for _, n := range notes {
		p.logf("%v", n)
	}
	if cycleDone != nil {
		cycleDone(c)
	}
	return code, err
}

// cycleDone, when set (by tests), is called with every cycle that ran batches, once it
// has ended.
var cycleDone func(*cycle)

// end is how the cycle ended, for resolveExit (saveErr and reportErr are set by the
// caller).
func (c *cycle) end() runEnd {
	return runEnd{worst: c.worst, testsFailed: c.testsFailed, interrupted: c.interrupted(), configErr: c.configErr, envErr: c.envErr, unmetErr: c.unmetErr}
}

// runEnd is how a run ended: what its exit code is decided from (resolveExit).
type runEnd struct {
	worst       int  // the runners' worst exit code
	testsFailed bool // a batch's tests failed (not a runner exiting 126/127 before any result)
	interrupted bool // Ctrl-C, SIGTERM or SIGHUP cut the run short
	// configErr: a suite command that cannot be parsed stopped the run.
	configErr error
	// envErr: the first batch whose runner could not start (or whose tests never did).
	envErr error
	// unmetErr: units left out because their requirements are not met.
	unmetErr error
	// saveErr: the state could not be saved; reportErr: --report could not be written.
	saveErr   error
	reportErr error
}

// resolveExit decides what a run ends with: the exit code of its tests (the runners'
// worst, at least ExitFailed when interrupted) and the error that decides smates' exit
// code instead, if any; notes are the errors that lose to it, logged at the end.
//
//   - A configuration error (the run stopped at a suite command that cannot be parsed)
//     wins over everything.
//   - An interrupted run failed (testsFailedError): what ran was recorded.
//   - A runner that could not start, unmet requirements, a state that could not be saved
//     or a report that could not be written make the run an environment error, unless
//     tests failed: failing tests matter more (exit 1, the errors as notes). A runner
//     that could not start, or a state that could not be saved, is reported before
//     unmet requirements, which the hooks only note.
func resolveExit(e runEnd) (code int, err error, notes []error) {
	code = e.worst
	if e.interrupted {
		code = max(code, ExitFailed)
	}
	note := func(errs ...error) {
		for _, x := range errs {
			if x != nil {
				notes = append(notes, x)
			}
		}
	}
	// The state matters more than the report: one output error is reported, the
	// other noted.
	saveErr, reportNote := e.saveErr, error(nil)
	if saveErr == nil {
		saveErr = e.reportErr
	} else {
		reportNote = e.reportErr
	}
	switch {
	case e.configErr != nil:
		note(e.envErr, e.unmetErr, saveErr, reportNote)
		return code, e.configErr, notes
	case e.interrupted:
		note(e.envErr, e.unmetErr, saveErr, reportNote)
		return code, testsFailedError{errors.New("interrupted")}, notes
	}
	envErr := e.envErr
	switch {
	case envErr == nil && saveErr != nil && e.unmetErr != nil:
		envErr, saveErr = saveErr, nil
		note(e.unmetErr)
	case envErr == nil:
		envErr = e.unmetErr
	default:
		note(e.unmetErr)
	}
	switch {
	case envErr != nil && e.testsFailed:
		note(saveErr, envErr)
	case envErr != nil:
		note(saveErr)
		err = envErr
	case saveErr != nil && code != 0:
		note(saveErr) // failing tests matter more: exit 1, the error as a note
	default:
		err = saveErr
	}
	note(reportNote)
	return code, err, notes
}

// reportedExit is the exit code the log, the summary, the GitHub outputs and --report
// give for a run: the runners' worst exit code, or the code of the error smates exits
// with instead (ExitEnv for an environment error without failing tests, ExitConfig),
// as resolveExit decides.
func reportedExit(e runEnd) int {
	code, err, _ := resolveExit(e)
	if err != nil {
		if x := exitCodeFor(err); x != ExitFailed {
			return x
		}
	}
	return code
}

// cycle is one `smates run` over the selected suites.
type cycle struct {
	p   *project
	f   *runFlags
	sel *selection
	// preShard is the selection before applyShard (suite -> unit), kept by a sharded
	// shadow run to judge escapes against the whole run's selection.
	preShard map[string]map[string]bool
	stdout   io.Writer // runner output (stderr with --report -)
	stderr   io.Writer
	// reportOut is the real stdout, where --report - writes.
	reportOut io.Writer
	timings   runTimings
	// quiet is --output failures: runner output is buffered per batch and printed only
	// when the batch fails, and smates logs only the essentials.
	quiet bool
	// heartbeat is how often failures mode reports progress while a batch runs.
	heartbeat time.Duration
	start     time.Time // when the first batch started
	total     int       // units the batches run
	record    bool      // trace the run and update the state
	runAll    bool      // run every test of every suite, whatever was selected
	self      string

	traceRoot string
	shimDir   string
	hasher    *selector.Hasher
	failures  *failureHints
	now       time.Time

	// Accumulated over the batches.
	worst int
	exit  int // the run's exit code as reported (reportedExit), set after recording
	// units is what became of each planned unit (unitState): ran, not run, not started
	// or deferred by unmet requirements.
	units unitRuns
	// failedBatches are the batches whose tests ran and exited non-zero.
	failedBatches []failedBatch
	// envErr is set when a batch's tests never started (notStarted); testsFailed when
	// another batch exited non-zero. configErr: a suite command that cannot be parsed
	// stopped the run.
	envErr      error
	configErr   error
	testsFailed bool
	// stop is what stopped the run before every batch started (stopNone: nothing did).
	stop     stopReason
	results  map[string]*suiteResult // suite -> outcomes to record
	escapes  []string                // shadow: failing tests the selection would have skipped
	possible []string                // shadow: skipped tests failed only by the run's exit code
	flakes   []flakeReport
	// ran adds up the batches of known duration that ran, per suite name and for all
	// of them ("\x00all"): overTime adds their mean overrun to later estimates.
	batchTimes map[string]*ranTime
	// unmetWhy are the failed requirement checks (suite -> descriptions) of the units
	// left out by unmet requirements (unitUnmet); unmetErr the error the run reports for
	// them; checked caches the checks of the run.
	unmetWhy map[string][]string
	unmetErr error
	checked  map[string]error
}

// stopped reports whether something stopped the run before every batch started.
func (c *cycle) stopped() bool { return c.stop != stopNone }

// interrupted reports whether the run was cancelled (Ctrl-C, SIGTERM, SIGHUP): the
// batch it cut short counts as not run.
func (c *cycle) interrupted() bool { return c.stop == stopInterrupt }

// capped reports whether --max-time stopped the run.
func (c *cycle) capped() bool { return c.stop == stopMaxTime }

// selfPath is the smates executable with symlinks resolved ("" if unknown).
func selfPath() string {
	self, _ := os.Executable()
	if self != "" {
		if r, err := filepath.EvalSymlinks(self); err == nil {
			self = r
		}
	}
	return self
}

// shadowShard reports whether the cycle is a sharded shadow run (--shadow --shard).
func (c *cycle) shadowShard() bool {
	return c.f.shadow && c.f.shardN > 1
}

// selectedUnits is the set of selected units of a selection, per suite.
func selectedUnits(res *selector.Result) map[string]map[string]bool {
	out := map[string]map[string]bool{}
	for _, sr := range res.Suites {
		out[sr.Name] = map[string]bool{}
		for _, id := range sr.Selected() {
			out[sr.Name][id] = true
		}
	}
	return out
}

// printCommands prints the command each batch would run (--dry-run).
func (c *cycle) printCommands(batches []*batch) {
	for _, b := range batches {
		rc := c.runContext(b.sr, b.units, b.full)
		if cmd, ok := b.sr.Suite.Adapter.(runner.Commander); ok {
			if argvs, err := cmd.Commands(rc); err == nil && argvs != nil {
				for _, argv := range argvs {
					fmt.Fprintf(c.stdout, "(cd %s && %s)\n", shellJoin([]string{b.sr.Suite.Root}), shellJoin(argv))
				}
				continue
			}
		}
		argv, _ := rc.CommandLine(nil, rc.RelArgs(""))
		if rc.All {
			argv, _ = rc.CommandLine(nil, nil)
		}
		fmt.Fprintf(c.stdout, "(cd %s && %s)\n", shellJoin([]string{b.sr.Suite.Root}), shellJoin(argv))
	}
}

// logSkipped logs the suites that run nothing.
func (c *cycle) logSkipped() {
	if c.quiet {
		return
	}
	for _, sr := range c.sel.res.Suites {
		if len(c.suiteRun(sr).units) == 0 {
			c.p.logf("%s: no tests selected, skipping", sr.Name)
		}
	}
}

// runHasher is a hasher for what a run records (or its failure hints compare) with:
// the snapshot, files git does not list read from disk, existence checks of paths
// outside the snapshot pinned as they were before the tests started (PinBefore, for
// the entries of res's suites).
func (p *project) runHasher(res *selector.Result, start time.Time) *selector.Hasher {
	h := selector.NewHasher(p.snap, selector.IgnoreGlobs(p.cfg)...).WithRoot(p.repo.Root).WithConfig(p.cfg)
	names := make([]string, 0, len(res.Suites))
	for _, sr := range res.Suites {
		names = append(names, sr.Name)
	}
	h.PinBefore(p.state, names, start)
	return h
}

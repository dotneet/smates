package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/dotneet/smates/internal/gitx"
	"github.com/dotneet/smates/internal/hints"
	"github.com/dotneet/smates/internal/runner"
	"github.com/dotneet/smates/internal/selector"
	"github.com/dotneet/smates/internal/state"
	"github.com/dotneet/smates/internal/summary"
	"github.com/dotneet/smates/internal/trace"
)

type runFlags struct {
	commonFlags
	callerFlags
	all      bool
	selectOn bool
	base     string
	noAgent  bool
	noRecord bool
	shadow   bool
	failFast bool
	shard    string
	dryRun   bool
	explain  bool
	summary  string
	markdown string
	report   string
	// output is the output mode (outputFull, outputFailures); outputFlag is set when
	// --output gave it. only are the --only tests.
	output     string
	outputFlag bool
	only       stringList
	onlySpecs  []onlySpec
	// extra are the arguments passed through to the runner; shardI/shardN the parsed
	// --shard (0/0 when not sharded).
	extra          []string
	shardI, shardN int
	// budget is --budget; heartbeat the progress interval of failures mode (0: none).
	budget    time.Duration
	heartbeat time.Duration
	// maxTime is --max-time: no batch starts that would end after it (0: no limit).
	maxTime time.Duration
	// deferred receives, at the end of a run, one line saying which selected units did
	// not run and stay selected (--max-time, unmet requires); the coding agent hooks
	// print it instead of blocking.
	deferred func(string)
	// skipEmpty returns right after the selection when it runs nothing, saving only
	// the agent's new answer and the orphans the policy handled (saveWithoutRun): the
	// Claude Code Stop hook's fast path.
	skipEmpty bool
}

func cmdRun(ctx context.Context, args []string, stdout, stderr io.Writer) (int, error) {
	f, err := parseRunFlags(args, stderr)
	if err != nil {
		return 2, err
	}
	code, err := runCycle(ctx, f, stdout, stderr, nil)
	return testsExitCode(code), err
}

// testsExitCode is the exit code of smates for the exit code of the test runs: 1 when
// they failed, whatever code a runner exited with (codes above 1 tell smates' own
// errors apart, see Main).
func testsExitCode(code int) int {
	return min(code, ExitFailed)
}

// parseRunFlags parses the arguments of `smates run`.
func parseRunFlags(args []string, stderr io.Writer) (*runFlags, error) {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var f runFlags
	f.commonFlags.register(fs)
	f.callerFlags.register(fs)
	fs.BoolVar(&f.all, "all", false, "run every test (and rewrite the recorded state)")
	fs.BoolVar(&f.selectOn, "select", false, "select tests even where the default is a full run (default branch on CI)")
	fs.StringVar(&f.base, "base", "", "git revision to diff against when nothing is recorded yet")
	fs.BoolVar(&f.noAgent, "no-agent", false, "do not call the agent for unattributed changes")
	fs.BoolVar(&f.noRecord, "no-record", false, "run without tracing and without updating the state")
	fs.BoolVar(&f.shadow, "shadow", false, "run every test but report which failures the selection would have missed")
	fs.BoolVar(&f.failFast, "fail-fast", false, "start no further test batch after one fails (tests run likeliest-to-fail first)")
	fs.StringVar(&f.shard, "shard", "", "run shard `I/N` of the selected tests (tests are assigned to shards by a stable hash)")
	fs.BoolVar(&f.dryRun, "dry-run", false, "print the commands instead of running them")
	fs.BoolVar(&f.explain, "explain", false, "print why each test was selected")
	fs.StringVar(&f.summary, "summary", "", "append a markdown summary to `file` (default: $GITHUB_STEP_SUMMARY on GitHub Actions)")
	fs.StringVar(&f.report, "report", "", "write a JSON report (selection, per-test results, shadow escapes) to `file` (- for stdout; all other output then goes to stderr)")
	fs.StringVar(&f.output, "output", "", "`mode`: full streams the runner output; failures prints only the output of failing tests, then a short summary with a command that reruns them (default: $SMATES_OUTPUT, where agent means failures, else full)")
	fs.Var(&f.only, "only", "run exactly the test unit `SUITE:UNIT` (repeatable; recorded as usual)")
	fs.StringVar(&f.markdown, "markdown", "", "write the summary as Markdown to `file`, e.g. for a pull request comment")
	fs.DurationVar(&f.budget, "budget", defaultBudget, "print how to split the run when the recorded durations of the selected tests add up to more than this (0: never)")
	fs.DurationVar(&f.maxTime, "max-time", 0, "start no test batch that would end after `duration` by recorded durations; the tests left out stay selected (0: no limit)")
	if err := fs.Parse(args); err != nil {
		return nil, usageError{err}
	}
	if f.maxTime < 0 {
		return nil, usageError{fmt.Errorf("--max-time must not be negative (got %s)", f.maxTime)}
	}
	f.extra = fs.Args()
	f.heartbeat = heartbeatInterval()
	if err := f.callerFlags.validate(); err != nil {
		return nil, err
	}
	var err error
	fs.Visit(func(fl *flag.Flag) { f.outputFlag = f.outputFlag || fl.Name == "output" })
	if f.output, err = parseOutputMode(f.output, stderr); err != nil {
		return nil, err
	}
	if f.onlySpecs, err = parseOnly(f.only); err != nil {
		return nil, err
	}
	if len(f.onlySpecs) > 0 && (f.all || f.shadow) {
		return nil, usageError{fmt.Errorf("--only runs exactly the named tests and cannot be combined with --all or --shadow")}
	}
	if len(f.onlySpecs) > 0 && (len(f.adds) > 0 || f.orphans != "") {
		return nil, usageError{fmt.Errorf("--only runs exactly the named tests and cannot be combined with --add or --orphans")}
	}
	if f.shardI, f.shardN, err = parseShard(f.shard); err != nil {
		return nil, err
	}
	if f.shadow && f.failFast {
		return nil, usageError{fmt.Errorf("--shadow runs every test and cannot be combined with --fail-fast")}
	}
	if f.shadow && f.maxTime > 0 {
		return nil, usageError{fmt.Errorf("--shadow runs every test and cannot be combined with --max-time")}
	}
	if f.dryRun && (f.report != "" || f.markdown != "" || f.summary != "") {
		return nil, usageError{fmt.Errorf("--dry-run runs nothing and cannot be combined with --report, --markdown or --summary (use `smates select --format json|markdown` for the selection)")}
	}
	return &f, nil
}

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
	applyShard(sel.res, f.shardI, f.shardN)
	printSelection(p, sel, stderr, f.explain)

	c := &cycle{p: p, f: f, sel: sel, preShard: preShard, stdout: stdout, stderr: stderr, reportOut: reportOut, record: !f.noRecord, self: selfPath(),
		quiet: f.output == outputFailures, heartbeat: f.heartbeat,
		ran: map[string]map[string]string{}, inferred: map[string]map[string]bool{}, started: map[string][]string{}, notRun: map[string][]string{}, unstarted: map[string][]string{}, results: map[string]*suiteResult{},
		unmet: map[string][]string{}, unmetWhy: map[string][]string{}, checked: map[string]error{}}
	// In shadow mode the selection is only evaluated: everything runs. A full run that is
	// sharded runs only this shard's share (applyShard narrowed the selection), and so
	// does a sharded shadow run (units).
	c.runAll = (sel.res.Mode == selector.ModeAll || f.shadow) && f.shardN <= 1
	prioritize(sel.res, p.state, p.direct())
	batches := c.plan()
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
		return 1, err
	}
	defer cleanup()
	c.hasher = selector.NewHasher(p.snap, selector.IgnoreGlobs(p.cfg)...).WithRoot(p.repo.Root).WithConfig(p.cfg)
	c.failures = newFailureHints(ctx, p, sel.res, c.hasher, f.base, f.noAgent)
	c.now = time.Now().UTC()
	start := time.Now()
	c.start = start
	// Files git does not list are recorded as they were before the tests ran.
	var ran []string
	for _, sr := range sel.res.Suites {
		ran = append(ran, sr.Name)
	}
	c.hasher.PinBefore(p.state, ran, start)
	for i, b := range batches {
		if !c.stopped && ctx.Err() != nil {
			c.interrupted, c.stopped = true, true // Ctrl-C between batches
		}
		if !c.stopped && c.overTime(b) {
			c.stopped, c.capped = true, true
			p.logf("--max-time %s: starting no further test batch (%d selected unit(s) left; they stay selected)", f.maxTime, c.unitsFrom(batches[i:]))
		}
		if c.stopped {
			c.notRun[b.sr.Name] = append(c.notRun[b.sr.Name], b.units...)
			continue
		}
		if c.deferUnmet(ctx, b) {
			continue
		}
		batchStart := time.Now()
		code, err := c.runBatch(ctx, i, b)
		c.addRan(b, time.Since(batchStart))
		var ce configError
		if errors.As(err, &ce) {
			return 1, err
		}
		if err != nil {
			// The runner could not start (not installed, preflight failed): like a
			// batch whose tests never started, an environment error reported after
			// the other batches ran and were recorded. Its units count as not run
			// (they stay selected).
			if c.envErr == nil {
				c.envErr = err
			} else {
				p.logf("%v", err) // only the first is reported at the end
			}
			c.unstarted[b.sr.Name] = append(c.unstarted[b.sr.Name], b.units...)
			c.notRun[b.sr.Name] = append(c.notRun[b.sr.Name], b.units...)
			continue
		}
		if code != 0 && f.failFast && i < len(batches)-1 {
			c.stopped = true
			if !c.quiet {
				p.logf("--fail-fast: %s failed; starting no further test batch", b.sr.Name)
			}
		}
	}
	if c.interrupted {
		// What ran is recorded and reported; the run itself failed.
		c.worst = max(c.worst, ExitFailed)
	}
	c.unmetErr = c.unmetError()
	hooks.onRan(c.started)
	c.failures.wait()
	elapsed := time.Since(start).Round(time.Millisecond)
	// A state that cannot be saved (e.g. the shared state stays locked by another
	// worktree's run) is an environment error, reported after the run's summary and
	// report so that the test results are not lost.
	var saveErr error
	if c.record {
		c.recordSuites() // before runRecord: it counts the flakes recording found
		p.state.AddRun(c.runRecord(elapsed, hooks != nil))
		if err := c.saveState(); err != nil {
			saveErr = envError{err}
		}
	}
	c.exit = c.exitCode(saveErr)
	if c.quiet {
		c.logShadowAndFlakes()
		c.printAgentSummary(elapsed.String())
	} else {
		c.failures.print(stderr)
		c.logOutcome(elapsed)
	}
	c.writeSummaries(elapsed)
	if f.report != "" {
		if err := c.writeReport(); err != nil {
			return c.worst, fmt.Errorf("writing report: %w", err)
		}
	}
	if f.deferred != nil {
		if line := c.deferredLine(); line != "" {
			f.deferred(line)
		}
	}
	if c.interrupted {
		for _, err := range []error{c.envErr, c.unmetErr, saveErr} {
			if err != nil {
				p.logf("%v", err)
			}
		}
		return c.worst, errors.New("interrupted")
	}
	// A batch whose tests never started (notStarted), or units whose requirements are
	// not met, make the run an environment error, reported after the usual failure
	// handling and summary; when tests of another batch failed, they matter more (exit
	// 1, the error as a note). A runner that could not start, or a state that could not
	// be saved, is reported before unmet requirements, which the hooks only note.
	envErr := c.envErr
	switch {
	case envErr == nil && saveErr != nil && c.unmetErr != nil:
		envErr, saveErr = saveErr, nil
		p.logf("%v", c.unmetErr)
	case envErr == nil:
		envErr = c.unmetErr
	case c.unmetErr != nil:
		p.logf("%v", c.unmetErr)
	}
	if envErr != nil {
		if saveErr != nil {
			p.logf("%v", saveErr)
		}
		if c.testsFailed {
			p.logf("%v", envErr)
			return c.worst, nil
		}
		return c.worst, envErr
	}
	if saveErr != nil && c.worst != 0 {
		// Failing tests matter more: report them (exit 1) and the save error as a note.
		p.logf("%v", saveErr)
		return c.worst, nil
	}
	return c.worst, saveErr
}

// exitCode is the exit code the log, the summary, the GitHub outputs and --report give
// for the run: the runners' worst exit code, or ExitEnv when the run is an environment
// error without failing tests (a runner could not start, requirements are not met, the
// state could not be saved), as smates exits then.
func (c *cycle) exitCode(saveErr error) int {
	if !c.interrupted && !c.testsFailed && (c.envErr != nil || c.unmetErr != nil || saveErr != nil && c.worst == 0) {
		return ExitEnv
	}
	return c.worst
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
	worst    int
	exit     int                          // the run's exit code as reported (exitCode), set after recording
	ran      map[string]map[string]string // suite -> unit -> status
	inferred map[string]map[string]bool   // suite -> unit -> status inferred from the run's exit code
	// failedBatches are the batches that exited non-zero.
	failedBatches []failedBatch
	// envErr is set when a batch's tests never started (notStarted); testsFailed when
	// another batch exited non-zero.
	envErr      error
	testsFailed bool
	started     map[string][]string     // suite -> units started
	notRun      map[string][]string     // --fail-fast, --max-time, interrupt, unstarted: suite -> units not started (or cut short)
	unstarted   map[string][]string     // suite -> units of batches whose runner could not start, or whose requirements are not met (also in notRun)
	stopped     bool                    // --fail-fast, --max-time (or an interrupt) stopped the run
	results     map[string]*suiteResult // suite -> outcomes to record
	escapes     []string                // shadow: failing tests the selection would have skipped
	possible    []string                // shadow: skipped tests failed only by the run's exit code
	flakes      []flakeReport
	// interrupted: the run was cancelled (Ctrl-C); the batch it cut short counts as
	// not run.
	interrupted bool
	// capped: --max-time stopped the run.
	capped bool
	// ran adds up the batches of known duration that ran, per suite name and for all
	// of them ("\x00all"): overTime adds their mean overrun to later estimates.
	batchTimes map[string]*ranTime
	// unmet are the units (also in unstarted) left out because requirements they need
	// are not met (suite -> units); unmetWhy the failed checks (suite -> descriptions);
	// unmetErr the error the run reports for them; checked caches the checks of the run.
	unmet    map[string][]string
	unmetWhy map[string][]string
	unmetErr error
	checked  map[string]error
}

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

// units returns the units of a suite this cycle runs, and whether they are the whole
// suite. A sharded shadow run runs every unit its shard owns, selected or not, so the
// shards of one run together run every test once.
func (c *cycle) units(sr *selector.Suite) ([]string, bool) {
	if c.runAll || c.shadowShard() {
		ids := make([]string, 0, len(sr.Tests))
		for _, t := range sr.Tests {
			if c.runAll || t.Shard == c.f.shardI {
				ids = append(ids, t.ID)
			}
		}
		return ids, c.runAll
	}
	return sr.Selected(), sr.All && c.f.shardN <= 1
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
		if units, _ := c.units(sr); len(units) == 0 {
			c.p.logf("%s: no tests selected, skipping", sr.Name)
		}
	}
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
	rc.TraceDir = filepath.Join(c.traceRoot, fmt.Sprintf("batch-%d", i))
	if err := os.MkdirAll(rc.TraceDir, 0o755); err != nil {
		return 0, err
	}
	var buf *outputBuffer
	switch {
	case c.quiet:
		buf = &outputBuffer{}
		rc.Stdout, rc.Stderr = buf, buf
	case b.parts > 1:
		p.logf("%s: running %d test unit(s) with %s (batch %d of %d: %s)", sr.Name, len(b.units), sr.Suite.Runner, b.part, b.parts, prioLabel(b.prio))
	default:
		p.logf("%s: running %d test unit(s) with %s", sr.Name, len(b.units), sr.Suite.Runner)
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
	noResult := notStarted(sr.Suite.Runner, c.record, r)
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
	settleOutcomes(r, b.units, c.record, len(c.f.extra) > 0)
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
		if w := noTraceWarning(b.units, r, sr.Suite.Runner, c.sel.diffUnrecorded); w != "" {
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

// recordSuites writes the collected outcomes into the state. Units that did not run
// (--fail-fast) keep their entries, so they stay selected; a suite whose run covered
// all its tests replaces its entries only when every batch of it ran.
func (c *cycle) recordSuites() {
	p := c.p
	for _, sr := range c.sel.res.Suites {
		res := c.results[sr.Name]
		if res == nil {
			continue
		}
		full := res.full && len(c.notRun[sr.Name]) == 0
		if w := untrustedWarning(sr, res.obs); w != "" {
			p.logf("%s: %s", sr.Name, w)
		}
		events := selector.Record(p.state, sr, res.obs, c.hasher.ForSuite(sr.Suite), selector.RecordOptions{Commit: p.repo.Head(), Now: c.now, Full: full, ReadsBy: res.readsBy, Retried: res.retried, Unreliable: len(c.f.extra) > 0})
		c.flakes = append(c.flakes, flakeReports(sr.Name, events)...)
	}
}

// untrustedWarning says which browser tests' passes trust_browser_passes does not
// record as passed (their known dependencies may not be all, see Suite.Untrusted), and
// why, for a few of them: they stay unknown and run again next time ("" when none).
func untrustedWarning(sr *selector.Suite, obs map[string]*runner.Observation) string {
	var units []string
	for u, o := range obs {
		if o.StaticOnly && o.Status == state.Pass && sr.Untrusted[u] != "" {
			units = append(units, u)
		}
	}
	if len(units) == 0 {
		return ""
	}
	slices.Sort(units)
	const shown = 3
	var b strings.Builder
	fmt.Fprintf(&b, "%d browser test(s) passed but are not trusted (trust_browser_passes), so they will run again next time:", len(units))
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

// saveState updates the state's repository snapshot and saves the state.
func (c *cycle) saveState() error {
	p, res := c.p, c.sel.res
	picked, left := c.ranSuites()
	switch {
	case c.stopped || len(picked) == 0:
		// --fail-fast stopped the run (or no suite's runner could start): the
		// repository-wide snapshot stays as it was, so changes and orphans the tests
		// that did not run would have covered are seen again next time.
	case len(left) > 0 && (res.Mode == selector.ModeAll || c.f.shadow):
		// Every test of the suites --suite picked ran (a suite whose runner could not
		// start counts as left out). Files only they own get their hash; files suites
		// left out also own (or own alone) are marked as seen by the picked suites
		// (state.MarkerSnap), so the suites left out still find them changed on a
		// shared state (locally, after `state merge`, or in CI without a cache-key per
		// job). Commit and Created stay as they were on purpose: a partial refresh
		// must not win State.Merge over a full snapshot, and hooks judge staleness by
		// the last full run.
		if p.state.Files == nil {
			p.state.Files = map[string]string{}
		}
		own := selector.NewOwnership(picked, left)
		for f, h := range p.snap {
			p.recordSeen(own, f, h)
		}
		for f := range p.state.Files {
			if _, ok := p.snap[f]; !ok {
				p.recordSeen(own, f, "")
			}
		}
	case res.Mode == selector.ModeAll || c.f.shadow:
		p.state.Files = map[string]string(p.snap)
		p.state.Commit = p.repo.Head()
		p.state.Created = c.now
	default:
		c.markHandledOrphans(picked, left)
	}
	return c.save()
}

// ranSuites splits the suites of the run into those that ran (picked) and those
// --suite left out or whose runner could not start (left).
func (c *cycle) ranSuites() (picked, left []*runner.Suite) {
	p := c.p
	if len(c.unstarted) == 0 {
		return p.suites, p.others
	}
	left = slices.Clip(p.others)
	for _, s := range p.suites {
		if _, ok := c.unstarted[s.Name]; ok {
			left = append(left, s)
		} else {
			picked = append(picked, s)
		}
	}
	return picked, left
}

// markHandledOrphans records the orphans the run handled (by name, by the agent or by
// running the affected suites) with their current content, so the next run on this
// branch does not ask again. Other files stay as recorded; per-test entries track
// them. Orphans the policy was not applied to (a watch cycle's older changes, --only)
// stay unseen. With suites left out (ranSuites) an orphan is recorded as seen by the
// picked suites only, and only by those the snapshot already holds a view of
// (selector.Snapshotted): a marker naming a suite no full recording covered would make
// it count as snapshotted, and every marker it has not seen a change for it again. Such
// a suite is treated like one left out: an orphan only it owns may be asked about again.
func (c *cycle) markHandledOrphans(picked, left []*runner.Suite) {
	p, res := c.p, c.sel.res
	if len(p.state.Files) == 0 {
		return
	}
	if len(left) > 0 {
		snapshotted := selector.Snapshotted(p.state)
		var kept []*runner.Suite
		left = slices.Clone(left)
		for _, s := range picked {
			if snapshotted(s.Name) {
				kept = append(kept, s)
			} else {
				left = append(left, s)
			}
		}
		if picked = kept; len(picked) == 0 {
			return
		}
	}
	own := selector.NewOwnership(picked, left)
	for _, o := range res.Orphans {
		if _, handled := res.OrphanHandling[o]; !handled {
			continue
		}
		if len(left) > 0 {
			p.recordSeen(own, o, p.snap[o])
		} else if h, ok := p.snap[o]; ok {
			p.state.Files[o] = h
		} else {
			delete(p.state.Files, o)
		}
	}
}

// saveWithoutRun saves what the selection learned when no test runs (--dry-run, or a
// selection that runs nothing under skipEmpty): the agent's new answer, so the next
// run on the same change does not ask again (as `smates select` does), and, with
// markOrphans, the orphans handled (the orphan policy left no test to run for them).
// No outcome is recorded.
func (c *cycle) saveWithoutRun(markOrphans bool) error {
	if !c.record {
		return nil
	}
	fresh := c.sel.agentDec != nil && !c.sel.agentCache
	handled := markOrphans && len(c.sel.res.OrphanHandling) > 0 && len(c.p.state.Files) > 0
	if !fresh && !handled {
		return nil
	}
	if handled {
		c.markHandledOrphans(c.p.suites, c.p.others)
	}
	return c.save()
}

// save writes the state.
func (c *cycle) save() error {
	p := c.p
	p.state.Tool = "smates/" + Version
	if err := p.store.Save(p.state); err != nil {
		return fmt.Errorf("saving state: %w", err)
	}
	return nil
}

// recordSeen records in the snapshot that the suites of a --suite run (own's picked
// suites) saw f with hash h ("" = absent): the hash itself when no suite left out owns
// f, a marker otherwise (state.MarkerSnap). An entry still matching h (gitx.SameHash:
// a hash an older smates recorded without the executable marker) stays as it is, and
// so does the content hash of a file only suites left out own (they have not compared
// it yet).
func (p *project) recordSeen(own selector.Ownership, f, h string) {
	v, ok := p.state.Snap(f)
	switch {
	case own.OwnedOnly(f):
		p.state.SetSnap(f, state.HashSnap(h))
	case ok && !v.Unrecorded() && gitx.SameHash(v.String(), h):
	case ok && !v.Unrecorded() && !own.Owned(f):
	case !ok && h == "":
	default:
		// Once every suite that owns f has seen this content, a plain entry.
		p.state.SetSnap(f, v.MarkSeen(h, own.Picked()).CollapseIfComplete(own.Owners(f)))
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

// unstartedSection lists, for the summary, the suites whose runner could not start or
// whose requirements are not met.
func (c *cycle) unstartedSection() (sec summary.Section, ok bool) {
	if len(c.unstarted) == 0 {
		return sec, false
	}
	var b strings.Builder
	if countUnits(c.unstarted) > countUnits(c.unmet) {
		b.WriteString("The runner could not start (see the log); these units did not run and stay selected:\n")
		for _, sr := range c.sel.res.Suites {
			if n := len(c.unstarted[sr.Name]) - len(c.unmet[sr.Name]); n > 0 {
				fmt.Fprintf(&b, "- `%s`: %d test unit(s)\n", sr.Name, n)
			}
		}
	}
	if len(c.unmet) > 0 {
		b.WriteString("Requirements (`requires`) are not met here; these units did not run and stay selected:\n")
		for _, sr := range c.sel.res.Suites {
			if units := c.unmet[sr.Name]; len(units) > 0 {
				fmt.Fprintf(&b, "- `%s`: %d test unit(s), need %s\n", sr.Name, len(units), strings.Join(c.unmetWhy[sr.Name], ", "))
			}
		}
	}
	return summary.Section{Title: fmt.Sprintf(":warning: %d test unit(s) not run", countUnits(c.unstarted)), Body: b.String()}, true
}

// notStartedBy names what left the units of notStartedUnits not started (when the run
// was not interrupted): --fail-fast or --max-time, unmet requirements, a runner that
// could not start (or exited 126/127 before any test reported a result).
func (c *cycle) notStartedBy() string {
	var by []string
	if countUnits(c.notRun) > countUnits(c.unstarted) {
		if c.capped {
			by = append(by, "--max-time")
		} else {
			by = append(by, "--fail-fast")
		}
	}
	if len(c.unmet) > 0 {
		by = append(by, "unmet requirements")
	}
	if countUnits(c.unstarted) > countUnits(c.unmet) || c.noResultUnits() > 0 {
		by = append(by, "a runner that could not start")
	}
	if len(by) == 0 {
		return "--fail-fast"
	}
	return joinAnd(by)
}

// notStartedUnits counts the selected units that did not start: those of notRun and
// those of the batches whose runner exited 126/127 before any test reported a result.
func (c *cycle) notStartedUnits() int {
	return countUnits(c.notRun) + c.noResultUnits()
}

// noResultUnits counts the units of the batches whose runner exited 126/127 before any
// test reported a result (notStarted): they did not run, and are recorded with an
// unknown outcome, so they stay selected.
func (c *cycle) noResultUnits() int {
	n := 0
	for _, fb := range c.failedBatches {
		if fb.noResult {
			n += len(fb.units)
		}
	}
	return n
}

// skippedNote is what the outcome line adds for the units that skipped themselves in
// this run (state.Skip): they did not fail, but they are not proven to pass, so they
// stay selected. "" when none did.
func (c *cycle) skippedNote() string {
	n := 0
	for _, units := range c.ran {
		for _, st := range units {
			if st == state.Skip {
				n++
			}
		}
	}
	if n == 0 {
		return ""
	}
	return fmt.Sprintf("; %d skipped (not proven: they run again next time)", n)
}

// logShadowAndFlakes logs, in failures mode, what logOutcome logs besides the outcome
// line: shadow escapes and flaky tests.
func (c *cycle) logShadowAndFlakes() {
	if c.f.shadow && len(c.escapes) > 0 {
		c.p.logf("SHADOW: %d failing test(s) would NOT have been selected: %s", len(c.escapes), strings.Join(limit(c.escapes, 10), ", "))
	}
	logFlakes(c.p, c.stderr, c.flakes)
}

// logOutcome logs how the run went.
func (c *cycle) logOutcome(elapsed time.Duration) {
	p := c.p
	selected, total := c.sel.res.Counts()
	if c.f.shadow {
		if len(c.escapes) > 0 {
			p.logf("SHADOW: %d failing test(s) would NOT have been selected:", len(c.escapes))
			for _, e := range c.escapes {
				fmt.Fprintf(c.stderr, "  - %s\n", e)
			}
		} else {
			if c.shadowShard() {
				// Out of the units this shard owns, all of which it ran.
				var r state.Run
				countSelection(&r, c.sel.res, c.f.shardI, c.f.shardN)
				selected, total = r.Selected, r.Total
			}
			p.logf("shadow: every failing test was selected (%d/%d would have run)", selected, total)
		}
		if len(c.possible) > 0 {
			p.logf("shadow: %d unselected test(s) are marked failed only because the run failed (the runner reports no per-file status): %s", len(c.possible), strings.Join(limit(c.possible, 10), ", "))
		}
	} else if n := countUnits(c.notRun); n > 0 && c.interrupted {
		p.logf("done in %s: ran %d of %d test unit(s), exit code %d; interrupted, %d selected unit(s) not run (they stay selected)", elapsed, selected-n, total, c.exit, n)
	} else if n := c.notStartedUnits(); n > 0 {
		p.logf("done in %s: ran %d of %d test unit(s), exit code %d; %s left %d selected unit(s) not started (they stay selected)%s", elapsed, selected-n, total, c.exit, c.notStartedBy(), n, c.skippedNote())
	} else {
		p.logf("done in %s: ran %d of %d test unit(s), exit code %d%s", elapsed, selected, total, c.exit, c.skippedNote())
	}
	logFlakes(p, c.stderr, c.flakes)
}

// writeSummaries writes the Markdown summaries and the GitHub outputs.
func (c *cycle) writeSummaries(elapsed time.Duration) {
	p, f := c.p, c.f
	sum := summaryInput(c.sel, f.shardI, f.shardN)
	sum.Run = &summary.Run{ExitCode: c.exit, Elapsed: elapsed, Shadow: f.shadow, Escapes: c.escapes, Possible: c.possible, Interrupted: c.interrupted}
	if c.capped {
		sum.Run.MaxTime = f.maxTime
	}
	sum.Savings = cumulativeSavings(p.state.Runs)
	for _, sr := range c.sel.res.Suites {
		for _, t := range sr.Tests {
			if c.ran[sr.Name][t.ID] == state.Fail {
				sum.Run.Failed = append(sum.Run.Failed, sr.Name+": "+t.ID)
			}
		}
		for _, u := range c.notRun[sr.Name] {
			if !slices.Contains(c.unstarted[sr.Name], u) {
				sum.Run.NotRun = append(sum.Run.NotRun, sr.Name+": "+u)
			}
		}
	}
	if sec, ok := c.unstartedSection(); ok {
		sum.Sections = append(sum.Sections, sec)
	}
	if sec, ok := flakySection(c.flakes); ok {
		sum.Sections = append(sum.Sections, sec)
	}
	if sec, ok := c.failures.section(); ok {
		sum.Sections = append(sum.Sections, sec)
	}
	writeRunSummary(p, f.summary, sum)
	writeMarkdown(p, f.markdown, sum)
	if os.Getenv("GITHUB_OUTPUT") != "" {
		if err := writeGitHubOutputs(c.stdout, c.sel, append([]string{fmt.Sprintf("exit-code=%d", c.exit)}, flakyOutputs(c.flakes)...)...); err != nil {
			p.logf("could not write GitHub outputs: %v", err)
		}
	}
}

// writeReport writes the JSON report (--report).
func (c *cycle) writeReport() error {
	rep := struct {
		jsonOut
		Results  map[string]map[string]string `json:"results"`
		Escapes  []string                     `json:"escapes"`
		Possible []string                     `json:"possible_escapes"`
		ExitCode int                          `json:"exit_code"`
		Shadow   bool                         `json:"shadow"`
		Flaky    []flakeReport                `json:"flaky"`
		Failures []*hints.Hint                `json:"failure_hints"`
		FailFast bool                         `json:"fail_fast"`
		NotRun   map[string][]string          `json:"not_run"` // --fail-fast, --max-time, interrupt, unmet requires: selected units not run
		// Interrupted: the run was cancelled (Ctrl-C); what did not finish is in not_run.
		Interrupted bool `json:"interrupted"`
		// MaxTimeReached: --max-time left the units of not_run out (except unmet_requires).
		MaxTimeReached bool `json:"max_time_reached"`
		// UnmetRequires: suite -> the requirements not met (the units are in not_run).
		UnmetRequires map[string][]string `json:"unmet_requires"`
	}{jsonSelection(c.sel), c.ran, c.escapes, c.possible, c.exit, c.f.shadow, c.flakes, c.failures.all(), c.f.failFast, c.notRun, c.interrupted, c.capped, c.unmetWhy}
	if rep.Flaky == nil {
		rep.Flaky = []flakeReport{}
	}
	if rep.Escapes == nil {
		rep.Escapes = []string{}
	}
	if rep.Possible == nil {
		rep.Possible = []string{}
	}
	data, err := json.MarshalIndent(rep, "", "  ")
	if err != nil {
		return err
	}
	if c.f.report == "-" {
		_, err := c.reportOut.Write(append(data, '\n'))
		return err
	}
	return os.WriteFile(c.f.report, data, 0o644)
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

type selectFlags struct {
	commonFlags
	callerFlags
	all      bool
	base     string
	noAgent  bool
	format   string
	shard    string
	explain  bool
	markdown string
	// maxShards (--max-shards) asks for a shard count sized to the selection, at most
	// this many; shardTime (--shard-time) is the recorded test time to aim for per shard.
	maxShards int
	shardTime time.Duration
}

// validateShards checks --max-shards and --shard-time (set: the flags given).
func (f *selectFlags) validateShards(set map[string]bool) error {
	switch {
	case set["max-shards"] && (f.maxShards < 1 || f.maxShards > maxShardsLimit):
		return usageError{fmt.Errorf("invalid --max-shards %d (want 1 to %d)", f.maxShards, maxShardsLimit)}
	case set["shard-time"] && !set["max-shards"]:
		return usageError{fmt.Errorf("--shard-time sizes --max-shards and needs it")}
	case f.shardTime < time.Second:
		return usageError{fmt.Errorf("invalid --shard-time %s (want at least 1s)", f.shardTime)}
	case set["max-shards"] && f.shard != "":
		return usageError{fmt.Errorf("--max-shards sizes the shards of the whole selection and cannot be combined with --shard")}
	}
	return nil
}

func cmdSelect(ctx context.Context, args []string, stdout, stderr io.Writer) (int, error) {
	fs := flag.NewFlagSet("select", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var f selectFlags
	f.commonFlags.register(fs)
	f.callerFlags.register(fs)
	fs.BoolVar(&f.all, "all", false, "select everything")
	fs.StringVar(&f.base, "base", "", "git revision to diff against when nothing is recorded yet")
	fs.BoolVar(&f.noAgent, "no-agent", false, "do not call the agent for unattributed changes")
	fs.StringVar(&f.format, "format", "text", "output: text, json, files, github, markdown")
	fs.StringVar(&f.shard, "shard", "", "only shard `I/N` of the selection")
	fs.BoolVar(&f.explain, "explain", false, "print why each test was selected")
	fs.StringVar(&f.markdown, "markdown", "", "also write the summary as Markdown to `file`, e.g. for a pull request comment")
	fs.IntVar(&f.maxShards, "max-shards", 0, "also size a shard count for the selection: 0 when nothing is selected, else 1 to `N` (outputs shards and shard-matrix)")
	fs.DurationVar(&f.shardTime, "shard-time", defaultShardTime, "with --max-shards: recorded test time to aim for per shard")
	if err := fs.Parse(args); err != nil {
		return 2, usageError{err}
	}
	// Before selecting: a selection can call the agent, write --markdown and save the state.
	if !slices.Contains([]string{"text", "files", "json", "markdown", "github"}, f.format) {
		return 2, usageError{fmt.Errorf("unknown format %q", f.format)}
	}
	if err := f.callerFlags.validate(); err != nil {
		return 2, err
	}
	set := map[string]bool{}
	fs.Visit(func(fl *flag.Flag) { set[fl.Name] = true })
	if err := f.validateShards(set); err != nil {
		return 2, err
	}
	shardI, shardN, err := parseShard(f.shard)
	if err != nil {
		return 2, err
	}
	p, err := load(&f.commonFlags, stderr)
	if err != nil {
		return 1, err
	}
	all := f.all
	if !all {
		// The selection (its skip and suites outputs, which gate jobs, and the shard
		// count) is what `smates run` will run: everything where it runs everything by
		// default (the default branch on CI), or a gated job would skip the full run
		// that refreshes the state.
		if full, branch := fullRunBranch(p.ci, p.repo, p.cfg.FullRunBranches); full {
			p.logf("on %s (%s): selecting every test, as `smates run` runs every test there", branch, p.ci.Name)
			all = true
		}
	}
	sel, err := p.selectTests(ctx, selectOptions{all: all, base: f.base, noAgent: f.noAgent, orphans: f.orphans, adds: f.adds, sharded: shardN > 1})
	if err != nil {
		return 1, err
	}
	applyShard(sel.res, shardI, shardN)
	var shardLines []string
	if f.maxShards > 0 {
		n := shardCount(sel.res, f.maxShards, f.shardTime)
		p.logf("%d shard(s) for this selection (--max-shards %d, about %s of recorded test time per shard)", n, f.maxShards, f.shardTime)
		shardLines = shardOutputs(n)
		sel.shards = &n
	}
	if sel.agentDec != nil && !sel.agentCache {
		// Keep the agent's answer for the next run on the same change.
		if err := p.store.Save(p.state); err != nil {
			p.logf("could not save agent cache: %v", err)
		}
	}
	sum := summaryInput(sel, shardI, shardN)
	writeMarkdown(p, f.markdown, sum)
	switch f.format {
	case "markdown":
		printSelection(p, sel, stderr, f.explain)
		_, err := io.WriteString(stdout, summary.Markdown(sum))
		return 0, err
	case "text", "files":
		if f.format == "text" {
			printSelection(p, sel, stderr, f.explain)
		}
		for _, s := range sel.res.Suites {
			for _, id := range s.Selected() {
				fmt.Fprintln(stdout, id)
			}
		}
	case "json":
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		return 0, enc.Encode(jsonSelection(sel))
	case "github":
		printSelection(p, sel, stderr, f.explain)
		if err := writeGitHubOutputs(stdout, sel, shardLines...); err != nil {
			return 0, err
		}
		writeRunSummary(p, "", sum)
	default:
		return 2, usageError{fmt.Errorf("unknown format %q", f.format)}
	}
	return 0, nil
}

type jsonOut struct {
	*selector.Result
	Selected int                 `json:"selected"`
	Total    int                 `json:"total"`
	Agent    any                 `json:"agent,omitempty"`
	Note     string              `json:"agent_note,omitempty"`
	Tests    map[string][]string `json:"selected_tests"`
	// Candidates describe the orphans for a calling agent that decides about them
	// itself (--add, --orphans).
	Candidates []orphanCandidate `json:"orphan_candidates"`
	// Shards is the shard count `select --max-shards` sized (absent without it).
	Shards *int `json:"shards,omitempty"`
}

func jsonSelection(sel *selection) jsonOut {
	s, t := sel.res.Counts()
	out := jsonOut{Result: sel.res, Selected: s, Total: t, Note: sel.agentNote, Tests: map[string][]string{}, Candidates: sel.candidates, Shards: sel.shards}
	if out.Candidates == nil {
		out.Candidates = []orphanCandidate{}
	}
	if sel.agentDec != nil {
		out.Agent = sel.agentDec
	}
	for _, su := range sel.res.Suites {
		out.Tests[su.Name] = append([]string{}, su.Selected()...)
	}
	return out
}

func printSelection(p *project, sel *selection, w io.Writer, explain bool) {
	res := sel.res
	s, t := res.Counts()
	for _, n := range res.Notes {
		p.logf("%s", n)
	}
	if res.Mode == selector.ModeDiff {
		p.logf("no recorded state: selected by static analysis against the base revision")
	}
	if len(res.Orphans) > 0 {
		p.logf("%d changed file(s) not attributed to any test: %s", len(res.Orphans), strings.Join(limit(res.Orphans, 8), ", "))
	}
	if sel.agentNote != "" {
		p.logf("%s", sel.agentNote)
	}
	if sel.hint {
		p.logf("%s", orphanHint)
	}
	pct := 0.0
	if t > 0 {
		pct = 100 * float64(t-s) / float64(t)
	}
	p.logf("selected %d of %d test unit(s) across %d suite(s) (%.0f%% skipped)", s, t, len(res.Suites), pct)
	if !explain && !p.verbose {
		return
	}
	for _, su := range res.Suites {
		ss := len(su.Selected())
		fmt.Fprintf(w, "  %s (%s): %d/%d\n", su.Name, su.Suite.Runner, ss, len(su.Tests))
		for _, tt := range su.Tests {
			if !tt.Selected {
				continue
			}
			fmt.Fprintf(w, "    + %s  [%s]\n", tt.ID, selectionReason(tt))
		}
	}
}

// suiteOutput is one suite of the `suites` GitHub output. Skip is true only when the
// suite selected nothing. A workflow skips a job only when `.skip` is true, never when
// `.selected` is 0: a suite the output does not list (left out by --suite, a plan job
// that failed) reads as null, which equals 0 in a workflow expression but not true.
type suiteOutput struct {
	Selected int  `json:"selected"`
	Total    int  `json:"total"`
	All      bool `json:"all"`
	Skip     bool `json:"skip"`
}

// githubOutputs are the selection's step outputs, as name=value lines.
func githubOutputs(sel *selection) []string {
	s, t := sel.res.Counts()
	runAll := true
	tests := map[string][]string{}
	suites := map[string]suiteOutput{}
	for _, su := range sel.res.Suites {
		if !su.All {
			runAll = false
		}
		ids := append([]string{}, su.Selected()...)
		tests[su.Name] = ids
		suites[su.Name] = suiteOutput{Selected: len(ids), Total: len(su.Tests), All: su.All && len(su.Tests) > 0, Skip: len(ids) == 0}
	}
	tj, _ := json.Marshal(tests)
	sj, _ := json.Marshal(suites)
	return []string{
		fmt.Sprintf("selected=%d", s), fmt.Sprintf("total=%d", t), fmt.Sprintf("run-all=%t", runAll && t > 0),
		fmt.Sprintf("skip=%t", s == 0), "tests=" + string(tj), "suites=" + string(sj),
	}
}

// writeGitHubOutputs writes selection outputs to $GITHUB_OUTPUT (stdout when unset).
func writeGitHubOutputs(stdout io.Writer, sel *selection, extra ...string) error {
	lines := append(githubOutputs(sel), extra...)
	if out := os.Getenv("GITHUB_OUTPUT"); out != "" {
		f, err := os.OpenFile(out, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if err != nil {
			return err
		}
		defer f.Close()
		for _, l := range lines {
			fmt.Fprintln(f, l)
		}
	} else {
		for _, l := range lines {
			fmt.Fprintln(stdout, l)
		}
	}
	return nil
}

// summaryInput is the Markdown summary of a selection (summary.Input.Run is set by run).
func summaryInput(sel *selection, shardI, shardN int) *summary.Input {
	return &summary.Input{Result: sel.res, Agent: sel.agentDec, AgentNote: sel.agentNote, Shard: shardI, Shards: shardN}
}

// writeRunSummary appends the Markdown summary to path ($GITHUB_STEP_SUMMARY by default).
func writeRunSummary(p *project, path string, in *summary.Input) {
	if path == "" {
		path = os.Getenv("GITHUB_STEP_SUMMARY")
	}
	if path == "" {
		return
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		p.logf("could not write summary: %v", err)
		return
	}
	defer f.Close()
	io.WriteString(f, summary.Markdown(in)+"\n")
}

// writeMarkdown writes (replaces) the Markdown summary at path, for a pull request comment.
func writeMarkdown(p *project, path string, in *summary.Input) {
	if path == "" {
		return
	}
	if err := os.WriteFile(path, []byte(summary.Markdown(in)), 0o644); err != nil {
		p.logf("could not write %s: %v", path, err)
	}
}

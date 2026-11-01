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
	if err := fs.Parse(args); err != nil {
		return nil, usageError{err}
	}
	f.extra = fs.Args()
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
	if f.shadow && f.shardN > 1 {
		return nil, usageError{fmt.Errorf("--shadow runs every test and cannot be combined with --shard")}
	}
	if f.shadow && f.failFast {
		return nil, usageError{fmt.Errorf("--shadow runs every test and cannot be combined with --fail-fast")}
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

	all := f.all
	if !all && !f.selectOn && !f.shadow && len(f.onlySpecs) == 0 {
		if full, branch := fullRunBranch(p.ci, p.repo, p.cfg.FullRunBranches); full {
			p.logf("on %s (%s): running every test to refresh the recorded state (use --select to override)", branch, p.ci.Name)
			all = true
		}
	}
	sel, err := p.selectTests(ctx, selectOptions{all: all, base: f.base, noAgent: f.noAgent, orphanScope: hooks.scope(), noOrphans: len(f.onlySpecs) > 0, orphans: f.orphans, adds: f.adds})
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
	applyShard(sel.res, f.shardI, f.shardN)
	printSelection(p, sel, stderr, f.explain)

	c := &cycle{p: p, f: f, sel: sel, stdout: stdout, stderr: stderr, reportOut: reportOut, record: !f.noRecord, self: selfPath(),
		quiet: f.output == outputFailures,
		ran:   map[string]map[string]string{}, inferred: map[string]map[string]bool{}, started: map[string][]string{}, notRun: map[string][]string{}, results: map[string]*suiteResult{}}
	// In shadow mode the selection is only evaluated: everything runs. A full run that is
	// sharded runs only this shard's share (applyShard narrowed the selection).
	c.runAll = (sel.res.Mode == selector.ModeAll || f.shadow) && f.shardN <= 1
	prioritize(sel.res, p.state, p.direct())
	batches := c.plan()
	c.logSkipped()
	if f.dryRun {
		c.printCommands(batches)
		return 0, nil
	}
	if c.record {
		cleanup, err := c.prepareTrace()
		if err != nil {
			return 1, err
		}
		defer cleanup()
	}
	c.hasher = selector.NewHasher(p.snap, selector.IgnoreGlobs(p.cfg)...).WithRoot(p.repo.Root).WithConfig(p.cfg)
	c.failures = newFailureHints(ctx, p, sel.res, c.hasher, f.base, f.noAgent)
	c.now = time.Now().UTC()
	start := time.Now()
	for i, b := range batches {
		if c.stopped {
			c.notRun[b.sr.Name] = append(c.notRun[b.sr.Name], b.units...)
			continue
		}
		code, err := c.runBatch(ctx, i, b)
		if err != nil {
			return 1, err
		}
		if code != 0 && f.failFast && i < len(batches)-1 {
			c.stopped = true
			if !c.quiet {
				p.logf("--fail-fast: %s failed; starting no further test batch", b.sr.Name)
			}
		}
	}
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
	// A batch whose tests never started (notStarted) makes the run an environment
	// error, reported after the usual failure handling and summary.
	if c.envErr != nil {
		if saveErr != nil {
			p.logf("%v", saveErr)
		}
		return c.worst, c.envErr
	}
	if saveErr != nil && c.worst != 0 {
		// Failing tests matter more: report them (exit 1) and the save error as a note.
		p.logf("%v", saveErr)
		return c.worst, nil
	}
	return c.worst, saveErr
}

// cycle is one `smates run` over the selected suites.
type cycle struct {
	p      *project
	f      *runFlags
	sel    *selection
	stdout io.Writer // runner output (stderr with --report -)
	stderr io.Writer
	// reportOut is the real stdout, where --report - writes.
	reportOut io.Writer
	// quiet is --output failures: runner output is buffered per batch and printed only
	// when the batch fails, and smates logs only the essentials.
	quiet  bool
	record bool // trace the run and update the state
	runAll bool // run every test of every suite, whatever was selected
	self   string

	traceRoot string
	shimDir   string
	hasher    *selector.Hasher
	failures  *failureHints
	now       time.Time

	// Accumulated over the batches.
	worst    int
	ran      map[string]map[string]string // suite -> unit -> status
	inferred map[string]map[string]bool   // suite -> unit -> status inferred from the run's exit code
	// failedBatches are the batches that exited non-zero.
	failedBatches []failedBatch
	// envErr is set when a batch's tests never started (notStarted).
	envErr   error
	started  map[string][]string     // suite -> units started
	notRun   map[string][]string     // --fail-fast: suite -> units not started after a failure
	stopped  bool                    // --fail-fast stopped the run
	results  map[string]*suiteResult // suite -> outcomes to record
	escapes  []string                // shadow: failing tests the selection would have skipped
	possible []string                // shadow: skipped tests failed only by the run's exit code
	flakes   []flakeReport
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
// suite.
func (c *cycle) units(sr *selector.Suite) ([]string, bool) {
	if c.runAll {
		ids := make([]string, 0, len(sr.Tests))
		for _, t := range sr.Tests {
			ids = append(ids, t.ID)
		}
		return ids, true
	}
	return sr.Selected(), sr.All && c.f.shardN <= 1
}

func (c *cycle) runContext(sr *selector.Suite, units []string, full bool) *runner.RunContext {
	rc := &runner.RunContext{
		Repo: c.p.repo, Suite: sr.Suite, Units: units, All: full && !sr.Suite.Narrowed, Record: c.record, ExtraArgs: c.f.extra,
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
					fmt.Fprintf(c.stdout, "(cd %s && %s)\n", b.sr.Suite.Root, strings.Join(argv, " "))
				}
				continue
			}
		}
		argv, _ := rc.CommandLine(nil, rc.RelArgs(""))
		if rc.All {
			argv, _ = rc.CommandLine(nil, nil)
		}
		fmt.Fprintf(c.stdout, "(cd %s && %s)\n", b.sr.Suite.Root, strings.Join(argv, " "))
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
	if c.record {
		rc.TraceDir = filepath.Join(c.traceRoot, fmt.Sprintf("batch-%d", i))
		if err := os.MkdirAll(rc.TraceDir, 0o755); err != nil {
			return 0, err
		}
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
	r, err := sr.Suite.Adapter.Run(ctx, rc)
	if err != nil {
		if buf != nil {
			c.printFailedBatch(sr, b, buf, nil)
		}
		return 0, runError(sr.Name, err)
	}
	var retried map[string]*runner.Observation
	if c.record && len(c.f.extra) == 0 {
		if r, retried, err = retryFlaky(ctx, p, rc, sr, r); err != nil {
			if buf != nil {
				c.printFailedBatch(sr, b, buf, nil)
			}
			return 0, runError(sr.Name, err)
		}
	}
	// A launcher (sh, npm, yarn, pnpm) exits 126 or 127 when the command it runs is not
	// executable or not installed: when tests that report results reported none, they
	// never ran. The batch is still handled as a failure below.
	if c.envErr == nil && notStarted(sr.Suite.Runner, c.record, r) {
		c.envErr = envError{fmt.Errorf("%s: the runner exited with code %d before any test reported a result: a command it runs is probably not installed or not executable (see its output)", sr.Name, r.ExitCode)}
	}
	c.worst = max(c.worst, r.ExitCode)
	for _, w := range r.Warnings {
		p.logf("%s: %s", sr.Name, w)
	}
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
		c.failedBatches = append(c.failedBatches, failedBatch{suite: sr.Name, units: b.units, anyFail: anyFail})
		if buf != nil {
			c.printFailedBatch(sr, b, buf, r.Observations)
		}
	}
	if c.f.shadow {
		c.checkShadow(sr, r.Observations)
	}
	c.failures.add(sr, r.Observations) // before recording: compares with the previous entries
	if c.record {
		if missing := len(b.units) - len(r.Observations); missing > 0 && r.ExitCode == 0 {
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
		events := selector.Record(p.state, sr, res.obs, c.hasher.ForSuite(sr.Suite), selector.RecordOptions{Commit: p.repo.Head(), Now: c.now, Full: full, ReadsBy: res.readsBy, Retried: res.retried, Unreliable: len(c.f.extra) > 0})
		c.flakes = append(c.flakes, flakeReports(sr.Name, events)...)
	}
}

// settleOutcomes corrects the outcomes a run reported for its units before they are
// recorded: record says the run was traced, extraArgs that arguments were passed
// through to the runner.
func settleOutcomes(r *runner.RunResult, units []string, record, extraArgs bool) {
	if r.Observations == nil {
		r.Observations = map[string]*runner.Observation{}
	}
	// Units that ran but left no trace when the run failed (build or collection
	// failure, an aborted session) have an unknown outcome: they are retried next time.
	if r.ExitCode != 0 && record {
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

// checkShadow notes the failing tests of a suite the selection would have skipped
// (--shadow). Only failures reported for the unit itself count: a failure inferred
// from a shared exit code may belong to a selected test.
func (c *cycle) checkShadow(sr *selector.Suite, obs map[string]*runner.Observation) {
	for _, t := range sr.Tests {
		o := obs[t.ID]
		if t.Selected || o == nil || o.Status != state.Fail {
			continue
		}
		if o.Inferred {
			c.possible = append(c.possible, sr.Name+": "+t.ID)
		} else {
			c.escapes = append(c.escapes, sr.Name+": "+t.ID)
		}
	}
}

// saveState updates the state's repository snapshot and saves the state.
func (c *cycle) saveState() error {
	p, res := c.p, c.sel.res
	switch {
	case c.stopped:
		// --fail-fast stopped the run: the repository-wide snapshot stays as it was,
		// so changes and orphans the tests that did not run would have covered are
		// seen again next time.
	case len(p.others) > 0 && (res.Mode == selector.ModeAll || c.f.shadow):
		// Every test of the suites --suite picked ran. Files only they own get their
		// hash; files suites left out also own (or own alone) are marked as seen by
		// the picked suites (selector.Marker), so the suites left out still find
		// them changed on a shared state (locally, after `state merge`, or in CI
		// without a cache-key per job). Commit and Created stay as they were on
		// purpose: a partial refresh must not win State.Merge over a full
		// snapshot, and hooks judge staleness by the last full run.
		if p.state.Files == nil {
			p.state.Files = map[string]string{}
		}
		for f, h := range p.snap {
			p.recordSeen(f, h)
		}
		for f := range p.state.Files {
			if _, ok := p.snap[f]; !ok {
				p.recordSeen(f, "")
			}
		}
	case res.Mode == selector.ModeAll || c.f.shadow:
		p.state.Files = map[string]string(p.snap)
		p.state.Commit = p.repo.Head()
		p.state.Created = c.now
	case len(p.state.Files) > 0:
		// Orphans were handled by this run (by name, by the agent or by running the
		// affected suites): remember their current content so the next run on this
		// branch does not ask again. Other files stay as recorded; per-test entries
		// track them.
		for _, o := range res.Orphans {
			if len(p.others) > 0 {
				p.recordSeen(o, p.snap[o])
			} else if h, ok := p.snap[o]; ok {
				p.state.Files[o] = h
			} else {
				delete(p.state.Files, o)
			}
		}
	}
	p.state.Tool = "smates/" + Version
	if err := p.store.Save(p.state); err != nil {
		return fmt.Errorf("saving state: %w", err)
	}
	return nil
}

// recordSeen records in the snapshot that the suites of a --suite run saw f with hash h
// ("" = absent): the hash itself when no suite left out owns f, a marker otherwise
// (selector.Marker). An entry still equal to h stays as it is, and so does the content
// hash of a file only suites left out own (they have not compared it yet).
func (p *project) recordSeen(f, h string) {
	all := append(slices.Clip(p.suites), p.others...)
	n := len(p.suites)
	v, ok := p.state.Files[f]
	_, _, marked := selector.ParseMarker(v)
	switch {
	case selector.OwnedOnly(f, all, n):
		if h == "" {
			delete(p.state.Files, f)
		} else {
			p.state.Files[f] = h
		}
	case ok && !marked && v == h:
	case ok && !marked && !selector.Owned(f, all, n):
	case !ok && h == "":
	default:
		var names []string
		for _, s := range p.suites {
			names = append(names, s.Name)
		}
		m := selector.MarkSeen(v, h, names)
		if _, seen, _ := selector.ParseMarker(m); subsetOf(selector.Owners(f, all), seen) {
			// Every suite that owns f has now seen this content: a plain entry.
			if h == "" {
				delete(p.state.Files, f)
			} else {
				p.state.Files[f] = h
			}
			return
		}
		p.state.Files[f] = m
	}
}

func subsetOf(a, b []string) bool {
	for _, x := range a {
		if !slices.Contains(b, x) {
			return false
		}
	}
	return true
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
			p.logf("shadow: every failing test was selected (%d/%d would have run)", selected, total)
		}
		if len(c.possible) > 0 {
			p.logf("shadow: %d unselected test(s) are marked failed only because the run failed (the runner reports no per-file status): %s", len(c.possible), strings.Join(limit(c.possible, 10), ", "))
		}
	} else if n := countUnits(c.notRun); n > 0 {
		p.logf("done in %s: ran %d of %d test unit(s), exit code %d; --fail-fast left %d selected unit(s) not started (they stay selected)", elapsed, selected-n, total, c.worst, n)
	} else {
		p.logf("done in %s: ran %d of %d test unit(s), exit code %d", elapsed, selected, total, c.worst)
	}
	logFlakes(p, c.stderr, c.flakes)
}

// writeSummaries writes the Markdown summaries and the GitHub outputs.
func (c *cycle) writeSummaries(elapsed time.Duration) {
	p, f := c.p, c.f
	sum := summaryInput(c.sel, f.shardI, f.shardN)
	sum.Run = &summary.Run{ExitCode: c.worst, Elapsed: elapsed, Shadow: f.shadow, Escapes: c.escapes, Possible: c.possible}
	sum.Savings = cumulativeSavings(p.state.Runs)
	for _, sr := range c.sel.res.Suites {
		for _, t := range sr.Tests {
			if c.ran[sr.Name][t.ID] == state.Fail {
				sum.Run.Failed = append(sum.Run.Failed, sr.Name+": "+t.ID)
			}
		}
		for _, u := range c.notRun[sr.Name] {
			sum.Run.NotRun = append(sum.Run.NotRun, sr.Name+": "+u)
		}
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
		if err := writeGitHubOutputs(c.stdout, c.sel, append([]string{fmt.Sprintf("exit-code=%d", c.worst)}, flakyOutputs(c.flakes)...)...); err != nil {
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
		NotRun   map[string][]string          `json:"not_run"` // --fail-fast: selected units not started
	}{jsonSelection(c.sel), c.ran, c.escapes, c.possible, c.worst, c.f.shadow, c.flakes, c.failures.all(), c.f.failFast, c.notRun}
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
	if err := fs.Parse(args); err != nil {
		return 2, usageError{err}
	}
	if err := f.callerFlags.validate(); err != nil {
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
	sel, err := p.selectTests(ctx, selectOptions{all: f.all, base: f.base, noAgent: f.noAgent, orphans: f.orphans, adds: f.adds})
	if err != nil {
		return 1, err
	}
	applyShard(sel.res, shardI, shardN)
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
		if err := writeGitHubOutputs(stdout, sel); err != nil {
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
}

func jsonSelection(sel *selection) jsonOut {
	s, t := sel.res.Counts()
	out := jsonOut{Result: sel.res, Selected: s, Total: t, Note: sel.agentNote, Tests: map[string][]string{}, Candidates: sel.candidates}
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

// writeGitHubOutputs writes selection outputs to $GITHUB_OUTPUT (stdout when unset).
func writeGitHubOutputs(stdout io.Writer, sel *selection, extra ...string) error {
	s, t := sel.res.Counts()
	runAll := true
	tests := map[string][]string{}
	for _, su := range sel.res.Suites {
		if !su.All {
			runAll = false
		}
		tests[su.Name] = append([]string{}, su.Selected()...)
	}
	tj, _ := json.Marshal(tests)
	lines := []string{
		fmt.Sprintf("selected=%d", s), fmt.Sprintf("total=%d", t), fmt.Sprintf("run-all=%t", runAll && t > 0),
		fmt.Sprintf("skip=%t", s == 0), "tests=" + string(tj),
	}
	lines = append(lines, extra...)
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

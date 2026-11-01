package cli

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
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
	all      bool
	selectOn bool
	base     string
	noAgent  bool
	noRecord bool
	shadow   bool
	shard    string
	dryRun   bool
	explain  bool
	summary  string
	markdown string
	report   string
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
	return runCycle(ctx, f, stdout, stderr, nil)
}

// parseRunFlags parses the arguments of `smates run`.
func parseRunFlags(args []string, stderr io.Writer) (*runFlags, error) {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var f runFlags
	f.register(fs)
	fs.BoolVar(&f.all, "all", false, "run every test (and rewrite the recorded state)")
	fs.BoolVar(&f.selectOn, "select", false, "select tests even where the default is a full run (default branch on CI)")
	fs.StringVar(&f.base, "base", "", "git revision to diff against when nothing is recorded yet")
	fs.BoolVar(&f.noAgent, "no-agent", false, "do not call the agent for unattributed changes")
	fs.BoolVar(&f.noRecord, "no-record", false, "run without tracing and without updating the state")
	fs.BoolVar(&f.shadow, "shadow", false, "run every test but report which failures the selection would have missed")
	fs.StringVar(&f.shard, "shard", "", "run shard `I/N` of the selected tests (tests are assigned to shards by a stable hash)")
	fs.BoolVar(&f.dryRun, "dry-run", false, "print the commands instead of running them")
	fs.BoolVar(&f.explain, "explain", false, "print why each test was selected")
	fs.StringVar(&f.summary, "summary", "", "append a markdown summary to `file` (default: $GITHUB_STEP_SUMMARY on GitHub Actions)")
	fs.StringVar(&f.report, "report", "", "write a JSON report (selection, per-test results, shadow escapes) to `file`")
	fs.StringVar(&f.markdown, "markdown", "", "write the summary as Markdown to `file`, e.g. for a pull request comment")
	if err := fs.Parse(args); err != nil {
		return nil, usageError{err}
	}
	f.extra = fs.Args()
	var err error
	if f.shardI, f.shardN, err = parseShard(f.shard); err != nil {
		return nil, err
	}
	if f.shadow && f.shardN > 1 {
		return nil, usageError{fmt.Errorf("--shadow runs every test and cannot be combined with --shard")}
	}
	return &f, nil
}

// runCycle is `smates run`: select, run every suite, record the outcomes and report.
// hooks (nil for `smates run`) let `smates watch` drive it.
func runCycle(ctx context.Context, f *runFlags, stdout, stderr io.Writer, hooks *runHooks) (int, error) {
	p, err := load(&f.commonFlags, stderr)
	if err != nil {
		return 1, err
	}
	if len(f.extra) > 0 && !f.noRecord {
		p.logf("runner arguments may filter tests: passing tests are not recorded as passed in this run")
	}

	all := f.all
	if !all && !f.selectOn && !f.shadow {
		if full, branch := fullRunBranch(p.ci, p.repo, p.cfg.FullRunBranches); full {
			p.logf("on %s (%s): running every test to refresh the recorded state (use --select to override)", branch, p.ci.Name)
			all = true
		}
	}
	sel, err := p.selectTests(ctx, selectOptions{all: all, base: f.base, noAgent: f.noAgent, orphanScope: hooks.scope()})
	if err != nil {
		return 1, err
	}
	hooks.onSelected(p, sel)
	applyShard(sel.res, f.shardI, f.shardN)
	printSelection(p, sel, stderr, f.explain)

	c := &cycle{p: p, f: f, sel: sel, stdout: stdout, stderr: stderr, record: !f.noRecord, self: selfPath(), ran: map[string]map[string]string{}}
	// In shadow mode the selection is only evaluated: everything runs. A full run that is
	// sharded runs only this shard's share (applyShard narrowed the selection).
	c.runAll = (sel.res.Mode == selector.ModeAll || f.shadow) && f.shardN <= 1
	if f.dryRun {
		c.printCommands()
		return 0, nil
	}
	if c.record {
		cleanup, err := c.prepareTrace()
		if err != nil {
			return 1, err
		}
		defer cleanup()
	}
	c.hasher = selector.NewHasher(p.snap, selector.IgnoreGlobs(p.cfg)...).WithRoot(p.repo.Root)
	c.failures = newFailureHints(ctx, p, sel.res, c.hasher, f.base, f.noAgent)
	c.now = time.Now().UTC()
	start := time.Now()
	for i, sr := range sel.res.Suites {
		if err := c.runSuite(ctx, i, sr); err != nil {
			return 1, err
		}
	}
	c.failures.wait()
	if c.record {
		if err := c.saveState(); err != nil {
			return 1, err
		}
	}
	c.failures.print(stderr)
	elapsed := time.Since(start).Round(time.Millisecond)
	c.logOutcome(elapsed)
	c.writeSummaries(elapsed)
	if f.report != "" {
		if err := c.writeReport(); err != nil {
			return c.worst, fmt.Errorf("writing report: %w", err)
		}
	}
	return c.worst, nil
}

// cycle is one `smates run` over the selected suites.
type cycle struct {
	p      *project
	f      *runFlags
	sel    *selection
	stdout io.Writer
	stderr io.Writer
	record bool // trace the run and update the state
	runAll bool // run every test of every suite, whatever was selected
	self   string

	traceRoot string
	shimDir   string
	hasher    *selector.Hasher
	failures  *failureHints
	now       time.Time

	// Accumulated over the suites.
	worst    int
	ran      map[string]map[string]string // suite -> unit -> status
	escapes  []string                     // shadow: failing tests the selection would have skipped
	possible []string                     // shadow: skipped tests failed only by the run's exit code
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
	return rc
}

// printCommands prints the command each suite would run (--dry-run).
func (c *cycle) printCommands() {
	for _, sr := range c.sel.res.Suites {
		units, full := c.units(sr)
		if len(units) == 0 {
			c.p.logf("%s: no tests selected, skipping", sr.Name)
			continue
		}
		rc := c.runContext(sr, units, full)
		argv, _ := rc.CommandLine(nil, rc.RelArgs(""))
		if rc.All {
			argv, _ = rc.CommandLine(nil, nil)
		}
		fmt.Fprintf(c.stdout, "(cd %s && %s)\n", sr.Suite.Root, strings.Join(argv, " "))
	}
}

// runSuite runs the units of the i-th suite and records their outcomes.
func (c *cycle) runSuite(ctx context.Context, i int, sr *selector.Suite) error {
	p := c.p
	units, full := c.units(sr)
	if len(units) == 0 {
		p.logf("%s: no tests selected, skipping", sr.Name)
		return nil
	}
	rc := c.runContext(sr, units, full)
	if c.record {
		rc.TraceDir = filepath.Join(c.traceRoot, fmt.Sprintf("suite-%d", i))
		if err := os.MkdirAll(rc.TraceDir, 0o755); err != nil {
			return err
		}
	}
	p.logf("%s: running %d test unit(s) with %s", sr.Name, len(units), sr.Suite.Runner)
	r, err := sr.Suite.Adapter.Run(ctx, rc)
	if err != nil {
		return fmt.Errorf("%s: %w", sr.Name, err)
	}
	var retried map[string]*runner.Observation
	if c.record && len(c.f.extra) == 0 {
		if r, retried, err = retryFlaky(ctx, p, rc, sr, r); err != nil {
			return err
		}
	}
	c.worst = max(c.worst, r.ExitCode)
	for _, w := range r.Warnings {
		p.logf("%s: %s", sr.Name, w)
	}
	settleOutcomes(r, units, c.record, len(c.f.extra) > 0)
	statuses := map[string]string{}
	for u, o := range r.Observations {
		statuses[u] = o.Status
	}
	c.ran[sr.Name] = statuses
	if c.f.shadow {
		c.checkShadow(sr, r.Observations)
	}
	c.failures.add(sr, r.Observations) // before recording: compares with the previous entries
	if c.record {
		if missing := len(units) - len(r.Observations); missing > 0 && r.ExitCode == 0 {
			p.debugf("%s: %d unit(s) produced no trace (kept as not recorded)", sr.Name, missing)
		}
		events := selector.Record(p.state, sr, r.Observations, c.hasher, selector.RecordOptions{Commit: p.repo.Head(), Now: c.now, Full: full, ReadsBy: r.ReadsBy, Retried: retried, Unreliable: len(c.f.extra) > 0})
		c.flakes = append(c.flakes, flakeReports(sr.Name, events)...)
	}
	return nil
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
	case p.filtered:
		// Only some suites ran: the repository-wide snapshot stays as it was.
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
			if h, ok := p.snap[o]; ok {
				p.state.Files[o] = h
			} else {
				delete(p.state.Files, o)
			}
		}
	}
	p.state.Tool = "smates/" + Version
	if err := p.state.Save(p.statePath); err != nil {
		return fmt.Errorf("saving state: %w", err)
	}
	return nil
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
	for _, sr := range c.sel.res.Suites {
		for _, t := range sr.Tests {
			if c.ran[sr.Name][t.ID] == state.Fail {
				sum.Run.Failed = append(sum.Run.Failed, sr.Name+": "+t.ID)
			}
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
	}{jsonSelection(c.sel), c.ran, c.escapes, c.possible, c.worst, c.f.shadow, c.flakes, c.failures.all()}
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

// unitFiles lists the test files of a unit (a unit is a test file, or for Go a
// package directory whose test files are known from its deps).
func unitFiles(sr *selector.Suite, unit string) []string {
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
	f.register(fs)
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
	shardI, shardN, err := parseShard(f.shard)
	if err != nil {
		return 2, err
	}
	p, err := load(&f.commonFlags, stderr)
	if err != nil {
		return 1, err
	}
	sel, err := p.selectTests(ctx, selectOptions{all: f.all, base: f.base, noAgent: f.noAgent})
	if err != nil {
		return 1, err
	}
	applyShard(sel.res, shardI, shardN)
	if sel.agentDec != nil && !sel.agentCache {
		// Keep the agent's answer for the next run on the same change.
		if err := p.state.Save(p.statePath); err != nil {
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
}

func jsonSelection(sel *selection) jsonOut {
	s, t := sel.res.Counts()
	out := jsonOut{Result: sel.res, Selected: s, Total: t, Note: sel.agentNote, Tests: map[string][]string{}}
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
			why := tt.Reason
			if len(tt.Changed) > 0 {
				why += ": " + strings.Join(limit(tt.Changed, 3), ", ")
			} else if tt.Detail != "" {
				why += ": " + tt.Detail
			}
			fmt.Fprintf(w, "    + %s  [%s]\n", tt.ID, why)
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

package cli

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/dotneet/smates/internal/runner"
	"github.com/dotneet/smates/internal/selector"
	"github.com/dotneet/smates/internal/state"
	"github.com/dotneet/smates/internal/summary"
	"github.com/dotneet/smates/internal/trace"
)

func jsonMarshal(v any) ([]byte, error)   { return json.Marshal(v) }
func jsonUnmarshal(b []byte, v any) error { return json.Unmarshal(b, v) }

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
}

func cmdRun(ctx context.Context, args []string, stdout, stderr io.Writer) (int, error) {
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
	fs.StringVar(&f.shard, "shard", "", "run shard `I/N` of the selected tests (balanced by recorded duration)")
	fs.BoolVar(&f.dryRun, "dry-run", false, "print the commands instead of running them")
	fs.BoolVar(&f.explain, "explain", false, "print why each test was selected")
	fs.StringVar(&f.summary, "summary", "", "append a markdown summary to `file` (default: $GITHUB_STEP_SUMMARY on GitHub Actions)")
	fs.StringVar(&f.report, "report", "", "write a JSON report (selection, per-test results, shadow escapes) to `file`")
	fs.StringVar(&f.markdown, "markdown", "", "write the summary as Markdown to `file`, e.g. for a pull request comment")
	if err := fs.Parse(args); err != nil {
		return 2, usageError{err}
	}
	extra := fs.Args()
	shardI, shardN, err := parseShard(f.shard)
	if err != nil {
		return 2, err
	}
	if f.shadow && shardN > 1 {
		return 2, usageError{fmt.Errorf("--shadow runs every test and cannot be combined with --shard")}
	}
	p, err := load(&f.commonFlags, stderr)
	if err != nil {
		return 1, err
	}

	if len(extra) > 0 && !f.noRecord {
		p.logf("runner arguments may filter tests: passing tests are not recorded as passed in this run")
	}

	all := f.all
	if !all && !f.selectOn && !f.shadow {
		if full, branch := fullRunBranch(p.ci, p.repo, p.cfg.FullRunBranches); full {
			p.logf("on %s (%s): running every test to refresh the recorded state (use --select to override)", branch, p.ci.Name)
			all = true
		}
	}
	sel, err := p.selectTests(ctx, selectOptions{all: all, base: f.base, noAgent: f.noAgent})
	if err != nil {
		return 1, err
	}
	res := sel.res
	applyShard(res, shardI, shardN)
	printSelection(p, sel, stderr, f.explain)

	// In shadow mode the selection is only evaluated: everything runs. A full run that is
	// sharded runs only this shard's share (applyShard narrowed the selection).
	runAllNow := (res.Mode == "all" || f.shadow) && shardN <= 1
	record := !f.noRecord
	traceRoot := ""
	if record && !f.dryRun {
		traceRoot, err = os.MkdirTemp("", "smates-trace-")
		if err != nil {
			return 1, err
		}
		if os.Getenv("SMATES_KEEP_TRACE") != "" {
			p.logf("keeping trace files in %s", traceRoot)
		} else {
			defer os.RemoveAll(traceRoot)
		}
	}
	shimDir := ""
	if traceRoot != "" {
		shimDir = filepath.Join(traceRoot, "shims")
		if _, err := trace.ExtractShims(shimDir); err != nil {
			return 1, err
		}
	}
	self, _ := os.Executable()
	if self != "" {
		if r, err := filepath.EvalSymlinks(self); err == nil {
			self = r
		}
	}

	hasher := selector.NewHasher(p.snap, selector.IgnoreGlobs(p.cfg)...).WithRoot(p.repo.Root)
	now := time.Now().UTC()
	worst := 0
	start := time.Now()
	ran := map[string]map[string]string{} // suite -> unit -> status
	var escapes, possible []string
	for i, sr := range res.Suites {
		units := sr.Selected()
		full := runAllNow || (sr.All && shardN <= 1)
		if runAllNow {
			units = nil
			for _, t := range sr.Tests {
				units = append(units, t.ID)
			}
		}
		if len(units) == 0 {
			p.logf("%s: no tests selected, skipping", sr.Name)
			continue
		}
		rc := &runner.RunContext{
			Repo: p.repo, Suite: sr.Suite, Units: units, All: full && shardN <= 1, Record: record, ExtraArgs: extra,
			ShimDir: shimDir, Self: self, Stdout: stdout, Stderr: stderr,
			IsTest: sr.Suite.IsTestFile,
		}
		unitOf := unitIndex(sr)
		rc.UnitOf = func(f string) string { return unitOf[f] }
		if f.dryRun {
			argv, _ := rc.CommandLine(nil, rc.RelArgs(""))
			if rc.All {
				argv, _ = rc.CommandLine(nil, nil)
			}
			fmt.Fprintf(stdout, "(cd %s && %s)\n", sr.Suite.Root, strings.Join(argv, " "))
			continue
		}
		if record {
			rc.TraceDir = filepath.Join(traceRoot, fmt.Sprintf("suite-%d", i))
			if err := os.MkdirAll(rc.TraceDir, 0o755); err != nil {
				return 1, err
			}
		}
		p.logf("%s: running %d test unit(s) with %s", sr.Name, len(units), sr.Suite.Runner)
		r, err := sr.Suite.Adapter.Run(ctx, rc)
		if err != nil {
			return 1, fmt.Errorf("%s: %w", sr.Name, err)
		}
		worst = max(worst, r.ExitCode)
		for _, w := range r.Warnings {
			p.logf("%s: %s", sr.Name, w)
		}
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
		if len(extra) > 0 {
			for _, o := range r.Observations {
				if o.Status == state.Pass {
					o.Status = state.Unknown
				}
			}
		}
		statuses := map[string]string{}
		inferred := map[string]bool{}
		for u, o := range r.Observations {
			statuses[u] = o.Status
			inferred[u] = o.Inferred
		}
		ran[sr.Name] = statuses
		if record {
			if missing := len(units) - len(r.Observations); missing > 0 && r.ExitCode == 0 {
				p.debugf("%s: %d unit(s) produced no trace (kept as not recorded)", sr.Name, missing)
			}
			selector.Record(p.state, sr, r.Observations, hasher, selector.RecordOptions{Commit: p.repo.Head(), Now: now, Full: full && shardN <= 1, ReadsBy: r.ReadsBy})
		}
		if f.shadow {
			for _, t := range sr.Tests {
				// Only failures reported for the unit itself count: a failure inferred
				// from a shared exit code may belong to a selected test.
				if !t.Selected && statuses[t.ID] == state.Fail {
					if inferred[t.ID] {
						possible = append(possible, sr.Name+": "+t.ID)
					} else {
						escapes = append(escapes, sr.Name+": "+t.ID)
					}
				}
			}
		}
	}
	if f.dryRun {
		return 0, nil
	}
	if record {
		if p.filtered {
			// Only some suites ran: the repository-wide snapshot stays as it was.
		} else if res.Mode == "all" || f.shadow {
			p.state.Files = map[string]string(p.snap)
			p.state.Commit = p.repo.Head()
			p.state.Created = now
		} else if len(p.state.Files) > 0 {
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
			return 1, fmt.Errorf("saving state: %w", err)
		}
	}
	sel2, total := res.Counts()
	elapsed := time.Since(start).Round(time.Millisecond)
	if f.shadow {
		if len(escapes) > 0 {
			p.logf("SHADOW: %d failing test(s) would NOT have been selected:", len(escapes))
			for _, e := range escapes {
				fmt.Fprintf(stderr, "  - %s\n", e)
			}
		} else {
			p.logf("shadow: every failing test was selected (%d/%d would have run)", sel2, total)
		}
		if len(possible) > 0 {
			p.logf("shadow: %d unselected test(s) are marked failed only because the run failed (the runner reports no per-file status): %s", len(possible), strings.Join(limit(possible, 10), ", "))
		}
	} else {
		p.logf("done in %s: ran %d of %d test unit(s), exit code %d", elapsed, sel2, total, worst)
	}
	sum := summaryInput(sel, shardI, shardN)
	sum.Run = &summary.Run{ExitCode: worst, Elapsed: elapsed, Shadow: f.shadow, Escapes: escapes, Possible: possible}
	for _, sr := range res.Suites {
		for _, t := range sr.Tests {
			if ran[sr.Name][t.ID] == state.Fail {
				sum.Run.Failed = append(sum.Run.Failed, sr.Name+": "+t.ID)
			}
		}
	}
	writeRunSummary(p, f.summary, sum)
	writeMarkdown(p, f.markdown, sum)
	if os.Getenv("GITHUB_OUTPUT") != "" {
		if err := writeGitHubOutputs(sel, fmt.Sprintf("exit-code=%d", worst)); err != nil {
			p.logf("could not write GitHub outputs: %v", err)
		}
	}
	if f.report != "" {
		rep := struct {
			jsonOut
			Results  map[string]map[string]string `json:"results"`
			Escapes  []string                     `json:"escapes"`
			Possible []string                     `json:"possible_escapes"`
			ExitCode int                          `json:"exit_code"`
			Shadow   bool                         `json:"shadow"`
		}{jsonSelection(sel), ran, escapes, possible, worst, f.shadow}
		if rep.Escapes == nil {
			rep.Escapes = []string{}
		}
		if rep.Possible == nil {
			rep.Possible = []string{}
		}
		data, err := json.MarshalIndent(rep, "", "  ")
		if err == nil {
			err = os.WriteFile(f.report, data, 0o644)
		}
		if err != nil {
			return worst, fmt.Errorf("writing report: %w", err)
		}
	}
	return worst, nil
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
	case "text":
		printSelection(p, sel, stderr, f.explain)
		for _, s := range sel.res.Suites {
			for _, id := range s.Selected() {
				fmt.Fprintln(stdout, id)
			}
		}
	case "files":
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
		if err := writeGitHubOutputs(sel); err != nil {
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
	switch res.Mode {
	case "diff":
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
func writeGitHubOutputs(sel *selection, extra ...string) error {
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
			fmt.Println(l)
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

// sortedKeys returns the sorted keys of a map.
func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

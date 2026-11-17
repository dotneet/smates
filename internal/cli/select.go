package cli

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/dotneet/smates/internal/selector"
	"github.com/dotneet/smates/internal/summary"
)

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

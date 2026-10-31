// Package cli implements the smates command line.
package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"hash/fnv"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/dotneet/smates/internal/agent"
	"github.com/dotneet/smates/internal/config"
	"github.com/dotneet/smates/internal/detect"
	"github.com/dotneet/smates/internal/gitx"
	"github.com/dotneet/smates/internal/runner"
	"github.com/dotneet/smates/internal/selector"
	"github.com/dotneet/smates/internal/state"
	"github.com/dotneet/smates/internal/static"
)

// Version is set at build time.
var Version = "dev"

const usage = `smates - run only the tests a change can affect

Usage:
  smates run [flags] [-- runner args]   select tests, run them with tracing, update state
  smates select [flags]                 print the selection without running anything
  smates watch [flags] [-- runner args] rerun the affected tests on every change
  smates init                           write smates.yml for this repository
  smates why <file>                     tests that depend on a file
  smates deps <test>                    files a test depends on
  smates flaky [--format json] [--clear] tests whose outcome flipped with unchanged inputs
  smates list                           list suites and test units
  smates state show|path|pull|push|merge|clear
  smates doctor                         check setup
  smates version

Run 'smates <command> -h' for flags. Docs: https://github.com/dotneet/smates
`

// Main runs the CLI and returns the process exit code.
func Main(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return 2
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	cmd, rest := args[0], args[1:]
	var err error
	code := 0
	switch cmd {
	case "run":
		code, err = cmdRun(ctx, rest, stdout, stderr)
	case "watch":
		code, err = cmdWatch(ctx, rest, os.Stdin, stdout, stderr)
	case "select":
		code, err = cmdSelect(ctx, rest, stdout, stderr)
	case "init":
		err = cmdInit(rest, stdout, stderr)
	case "why":
		err = cmdWhy(rest, stdout, stderr)
	case "deps":
		err = cmdDeps(rest, stdout, stderr)
	case "flaky":
		err = cmdFlaky(rest, stdout, stderr)
	case "list", "ls":
		err = cmdList(rest, stdout, stderr)
	case "state":
		err = cmdState(rest, stdout, stderr)
	case "doctor":
		code, err = cmdDoctor(rest, stdout, stderr)
	case "version", "--version", "-v":
		fmt.Fprintln(stdout, "smates", Version)
	case "help", "-h", "--help":
		fmt.Fprint(stdout, usage)
	default:
		fmt.Fprintf(stderr, "smates: unknown command %q\n\n%s", cmd, usage)
		return 2
	}
	if err != nil {
		var ue usageError
		if errors.As(err, &ue) {
			fmt.Fprintf(stderr, "smates %s: %v\n", cmd, err)
			return 2
		}
		fmt.Fprintf(stderr, "smates: %v\n", err)
		if code == 0 {
			code = 1
		}
	}
	return code
}

type usageError struct{ error }

// commonFlags are shared by commands that load the project.
type commonFlags struct {
	dir     string
	config  string
	state   string
	suites  stringList
	verbose bool
}

func (c *commonFlags) register(fs *flag.FlagSet) {
	fs.StringVar(&c.dir, "C", "", "run as if started in `dir`")
	fs.StringVar(&c.config, "config", "", "config file (default: smates.yml at the repository root)")
	fs.StringVar(&c.state, "state", "", "state file (default: .smates/state.json.gz)")
	fs.Var(&c.suites, "suite", "limit to a suite (repeatable)")
	fs.BoolVar(&c.verbose, "v", false, "verbose output")
}

type stringList []string

func (s *stringList) String() string     { return strings.Join(*s, ",") }
func (s *stringList) Set(v string) error { *s = append(*s, v); return nil }

// project is a loaded repository.
type project struct {
	repo      *gitx.Repo
	cfg       *config.Config
	suites    []*runner.Suite
	statePath string
	state     *state.State
	snap      gitx.Snapshot
	files     []string
	graph     *static.Graph
	ci        ciInfo
	log       io.Writer
	verbose   bool
	// filtered is set when --suite limited the suites (repository-wide bookkeeping
	// such as the snapshot is then left alone).
	filtered bool
}

func load(cf *commonFlags, log io.Writer) (*project, error) {
	dir := cf.dir
	if dir == "" {
		dir = "."
	}
	repo, err := gitx.Open(dir)
	if err != nil {
		return nil, err
	}
	var cfg *config.Config
	if cf.config != "" {
		data, err := os.ReadFile(cf.config)
		if err != nil {
			return nil, err
		}
		if cfg, err = config.Parse(data); err != nil {
			return nil, fmt.Errorf("%s: %w", cf.config, err)
		}
		if rel, err := filepath.Rel(repo.Root, absPath(cf.config)); err == nil && !strings.HasPrefix(rel, "..") {
			cfg.Path = filepath.ToSlash(rel)
		}
	} else if cfg, err = config.Load(repo.Root); err != nil {
		return nil, err
	}
	snap, err := repo.Snapshot()
	if err != nil {
		return nil, err
	}
	files := make([]string, 0, len(snap))
	for f := range snap {
		files = append(files, f)
	}
	sort.Strings(files)
	if cfg == nil {
		cfg = &config.Config{}
		cfg.Suites = detect.Suites(repo, files)
		if err := cfg.Normalize(); err != nil {
			return nil, err
		}
	}
	suites, err := runner.Resolve(repo, cfg.Suites)
	if err != nil {
		return nil, err
	}
	if len(cf.suites) > 0 {
		var keep []*runner.Suite
		for _, s := range suites {
			for _, n := range cf.suites {
				if s.Name == n {
					keep = append(keep, s)
				}
			}
		}
		if len(keep) != len(cf.suites) {
			return nil, usageError{fmt.Errorf("unknown suite in %v (have: %s)", []string(cf.suites), suiteNames(suites))}
		}
		suites = keep
	}
	statePath := cf.state
	if statePath == "" {
		statePath = filepath.Join(repo.Root, filepath.FromSlash(cfg.State))
	}
	st, err := state.Load(statePath)
	if errors.Is(err, state.ErrUnreadable) {
		fmt.Fprintf(log, "smates: ignoring unreadable state at %s (%v); starting cold\n", statePath, err)
		st, err = state.New(), nil
	}
	if err != nil {
		return nil, err
	}
	var roots []string
	for _, s := range suites {
		for _, r := range s.PythonRoots {
			roots = append(roots, filepath.ToSlash(filepath.Join(s.Root, r)))
		}
	}
	p := &project{
		repo: repo, cfg: cfg, suites: suites, statePath: statePath, state: st, snap: snap, files: files,
		graph: static.New(repo.Root, files, static.Options{PythonRoots: roots}), ci: detectCI(), log: log, verbose: cf.verbose,
		filtered: len(cf.suites) > 0,
	}
	return p, nil
}

func absPath(p string) string {
	a, err := filepath.Abs(p)
	if err != nil {
		return p
	}
	return a
}

func suiteNames(s []*runner.Suite) string {
	var n []string
	for _, x := range s {
		n = append(n, x.Name)
	}
	if len(n) == 0 {
		return "none detected"
	}
	return strings.Join(n, ", ")
}

func (p *project) logf(format string, args ...any) {
	fmt.Fprintf(p.log, "smates: "+format+"\n", args...)
}

func (p *project) debugf(format string, args ...any) {
	if p.verbose {
		p.logf(format, args...)
	}
}

// selectOptions control selection.
type selectOptions struct {
	all     bool
	base    string
	noAgent bool
	// orphanScope, when non-nil, limits the orphan policy to these files (`smates
	// watch`: files changed since the previous cycle).
	orphanScope map[string]bool
}

// selection runs deterministic selection and the orphan policy.
type selection struct {
	res        *selector.Result
	agentNote  string
	agentDec   *agent.Decision
	agentCache bool
}

func (p *project) selectTests(ctx context.Context, opt selectOptions) (*selection, error) {
	if len(p.suites) == 0 {
		return nil, fmt.Errorf("no test suites configured or detected; run `smates init` or write smates.yml")
	}
	base := opt.base
	// Locally the state only knows the tests that ran on this machine: tests it has
	// not recorded yet are judged against the base branch instead of all running as new.
	diffUnrecorded := p.ci.Name == "" && !opt.all
	// Without a recorded snapshot, changed files (and so orphans) come from the base.
	if base == "" && !opt.all && (p.state.Empty() || diffUnrecorded || len(p.state.Files) == 0) {
		base = autoBase(p.ci, p.repo)
		if base != "" {
			p.debugf("comparing unrecorded tests with %s", base)
		}
	}
	res, err := selector.Select(&selector.Input{
		Repo: p.repo, Config: p.cfg, Suites: p.suites, State: p.state, Snap: p.snap, Graph: p.graph, Base: base, All: opt.all,
		DiffUnrecorded: diffUnrecorded,
	})
	if err != nil {
		return nil, err
	}
	sel := &selection{res: res}
	if len(res.Orphans) == 0 {
		return sel, nil
	}

	// 1. Deterministic: tests that mention an orphan by name.
	refs, explained := res.References(p.graph, res.Orphans)
	res.Apply(refs)
	remaining := subtract(res.Orphans, explained)
	if opt.orphanScope != nil {
		var in []string
		for _, o := range remaining {
			if opt.orphanScope[o] {
				in = append(in, o)
			}
		}
		if n := len(remaining) - len(in); n > 0 {
			p.debugf("%d unattributed change(s) predate this cycle: orphan policy not applied to them", n)
		}
		remaining = in
	}
	if len(remaining) == 0 {
		return sel, nil
	}

	// 2. Policy for what is left.
	policy := p.cfg.Orphans
	if policy == "ignore" {
		res.Notes = append(res.Notes, fmt.Sprintf("ignored %d unattributed change(s) (orphans: ignore)", len(remaining)))
		return sel, nil
	}
	if policy == "agent" && !opt.noAgent && p.cfg.Agent.Enabled != "never" {
		ok, why := agent.Available(p.cfg.Agent)
		if ok {
			dec, cached, err := p.runAgent(ctx, res, remaining, base)
			if err == nil {
				sel.agentDec, sel.agentCache = dec, cached
				applyDecision(res, dec)
				return sel, nil
			}
			sel.agentNote = "agent failed (" + err.Error() + "); running affected suites in full"
		} else {
			sel.agentNote = "agent unavailable (" + why + "); running affected suites in full"
		}
	}
	res.Apply(res.OrphanAll(remaining))
	return sel, nil
}

func subtract(a, b []string) []string {
	drop := map[string]bool{}
	for _, x := range b {
		drop[x] = true
	}
	var out []string
	for _, x := range a {
		if !drop[x] {
			out = append(out, x)
		}
	}
	return out
}

func applyDecision(res *selector.Result, dec *agent.Decision) {
	if dec.RunAll {
		var adds []selector.Addition
		for _, s := range res.Suites {
			adds = append(adds, selector.Addition{Suite: s.Name, Reason: selector.ReasonAgent, Detail: "agent: " + dec.Summary})
		}
		res.Apply(adds)
		return
	}
	res.Apply(dec.Add)
}

func (p *project) runAgent(ctx context.Context, res *selector.Result, orphans []string, base string) (*agent.Decision, bool, error) {
	req := &agent.Request{Repo: p.repo, Config: p.cfg.Agent, Selection: res, Log: io.Discard}
	if p.verbose {
		req.Log = p.log
	}
	for _, o := range orphans {
		old, hadOld := p.state.Files[o]
		cur, has := p.snap[o]
		st := "modified"
		switch {
		case !has:
			st = "deleted"
		case !hadOld && len(p.state.Files) > 0:
			st = "added"
		}
		req.Orphans = append(req.Orphans, agent.Orphan{Path: o, Status: st, OldBlob: old, NewBlob: cur, Base: base})
	}
	key := agent.CacheKey(req)
	if raw, ok := p.state.AgentCache[key]; ok {
		var dec agent.Decision
		if err := jsonUnmarshal(raw, &dec); err == nil {
			p.logf("agent: reusing cached decision for %d orphan file(s)", len(orphans))
			return &dec, true, nil
		}
	}
	p.logf("agent: asking %s about %d unattributed change(s): %s", p.cfg.Agent.Model, len(orphans), strings.Join(limit(orphans, 5), ", "))
	ctx, cancel := context.WithTimeout(ctx, p.cfg.Agent.Timeout)
	defer cancel()
	start := time.Now()
	dec, err := agent.New(p.cfg.Agent).Decide(ctx, req)
	if err != nil {
		return nil, false, err
	}
	p.logf("agent: %s (%d turns, %d in / %d out tokens, %s)", oneLine(dec.Summary), dec.Usage.Turns, dec.Usage.InputTokens+dec.Usage.CacheReadTokens+dec.Usage.CacheWriteTokens, dec.Usage.OutputTokens, time.Since(start).Round(100*time.Millisecond))
	if raw, err := jsonMarshal(dec); err == nil {
		p.state.AgentCache[key] = raw
	}
	return dec, false, nil
}

func limit(s []string, n int) []string {
	if len(s) <= n {
		return s
	}
	return append(append([]string{}, s[:n]...), fmt.Sprintf("… (+%d)", len(s)-n))
}

func oneLine(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > 160 {
		s = s[:157] + "..."
	}
	return s
}

// parseShard parses "i/n" (1-based).
func parseShard(s string) (int, int, error) {
	if s == "" {
		return 0, 0, nil
	}
	a, b, ok := strings.Cut(s, "/")
	i, err1 := strconv.Atoi(a)
	n, err2 := strconv.Atoi(b)
	if !ok || err1 != nil || err2 != nil || n < 1 || i < 1 || i > n {
		return 0, 0, usageError{fmt.Errorf("invalid --shard %q (want I/N, e.g. 1/4)", s)}
	}
	return i, n, nil
}

// applyShard keeps the selected tests owned by shard i of n. Ownership is a stable hash
// of suite and test ID, so every shard agrees on it whatever state each one restored
// (shards may hold different recorded durations and selections).
func applyShard(res *selector.Result, i, n int) {
	if n <= 1 {
		return
	}
	for _, s := range res.Suites {
		for _, t := range s.Tests {
			h := fnv.New32a()
			h.Write([]byte(s.Name + "\x00" + t.ID))
			owner := int(h.Sum32()%uint32(n)) + 1
			if t.Selected && owner != i {
				t.Selected = false
				t.Detail = fmt.Sprintf("in shard %d/%d", owner, n)
			}
		}
		s.All = false
	}
}

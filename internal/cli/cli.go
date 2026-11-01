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
	"slices"
	"sort"
	"strconv"
	"strings"
	"syscall"

	"github.com/dotneet/smates/docs"
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
  smates init [--github] [--agents-md] [--format json] write smates.yml for this repository
  smates why <file> [--format json]     tests that depend on a file
  smates deps <test> [--format json]    files a test depends on
  smates flaky [--format json] [--clear] tests whose outcome flipped with unchanged inputs
  smates stats [--since 30d] [--format json] time saved, shadow misses, orphans and agent use
  smates orphans [--history 100] [--format json] [--apply [--dry-run]] recent changes no test explains, and ignore globs for them
  smates list [--format json]           list suites and test units
  smates config add-ignore|add-input <glob>... | set <key> <value> [--dry-run]  edit smates.yml safely
  smates state show [--format json]|path|pull|push|merge|clear
  smates hook install|uninstall|status  run the affected tests from a git pre-push hook
                                        (--claude: from a Claude Code Stop hook)
  smates guide                          how a coding agent should use smates
  smates doctor [--format json]         check setup
  smates version

Exit codes:
  0  success
  1  tests failed
  2  usage error (invalid command line)
  3  configuration error (invalid smates.yml, unknown runner, a suite or test it does not define)
  4  environment problem (git missing, not a git repository, a suite command that cannot start,
     an npx/bunx tool missing from node_modules, or, in a recorded run of any runner but
     custom, a runner exiting 126/127 before any test reported a result; with --no-record
     or the custom runner such an exit is a test failure)

For agents: 'smates run --output failures' (or SMATES_OUTPUT=agent) prints only the output
of failing tests, then a short summary with the command that reruns just those tests.
'smates guide' has the rest.

Run 'smates <command> -h' for flags. Docs: https://github.com/dotneet/smates
`

// Main runs the CLI and returns the process exit code.
func Main(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return 2
	}
	// SIGTERM and SIGHUP (timeout(1), a cancelled CI job, a closed terminal) stop smates
	// as Ctrl-C does: by cancelling, so that what runs in a process group of its own (an
	// agent command, the tests of a watch run) is stopped with it.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
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
	case "stats":
		err = cmdStats(rest, stdout, stderr)
	case "orphans":
		err = cmdOrphans(rest, stdout, stderr)
	case "config":
		err = cmdConfig(rest, stdout, stderr)
	case "list", "ls":
		err = cmdList(rest, stdout, stderr)
	case "state":
		err = cmdState(rest, stdout, stderr)
	case "hook":
		code, err = cmdHook(ctx, rest, os.Stdin, stdout, stderr)
	case "doctor":
		code, err = cmdDoctor(rest, stdout, stderr)
	case "guide":
		_, err = io.WriteString(stdout, docs.Agents)
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
			return ExitUsage
		}
		fmt.Fprintf(stderr, "smates: %v\n", err)
		var ce configError
		var ee envError
		switch {
		case errors.As(err, &ce):
			return ExitConfig
		case errors.As(err, &ee):
			return ExitEnv
		}
		if code == 0 {
			code = ExitFailed
		}
	}
	return code
}

// Exit codes of smates (see `smates help`).
const (
	ExitOK     = 0 // success
	ExitFailed = 1 // tests failed (or an unexpected error)
	ExitUsage  = 2 // invalid command line
	ExitConfig = 3 // invalid configuration: smates.yml, a suite or test the configuration does not define
	ExitEnv    = 4 // environment problem: git, the repository, a runner or tool that cannot start
)

// usageError is an invalid command line (exit code 2).
type usageError struct{ error }

// configError is an invalid configuration (exit code 3): fix smates.yml, or the suite
// or test named on the command line.
type configError struct{ error }

func (e configError) Unwrap() error { return e.error }

// envError is a problem of the environment smates runs in (exit code 4): git missing,
// not a git repository, a runner or tool that is not installed or cannot start.
type envError struct{ error }

func (e envError) Unwrap() error { return e.error }

// asConfigError marks err (nil stays nil) as a configuration error.
func asConfigError(err error) error {
	if err == nil {
		return nil
	}
	return configError{err}
}

// asEnvError marks err (nil stays nil) as an environment error.
func asEnvError(err error) error {
	if err == nil {
		return nil
	}
	return envError{err}
}

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
	store     *stateStore
	state     *state.State
	snap      gitx.Snapshot
	files     []string
	graph     *static.Graph
	ci        ciInfo
	log       io.Writer
	verbose   bool
	dir       string // -C
	// others are the suites --suite left out (they still explain changes, see
	// selector.Input.Others).
	others []*runner.Suite
}

func load(cf *commonFlags, log io.Writer) (*project, error) {
	dir := cf.dir
	if dir == "" {
		dir = "."
	}
	repo, err := gitx.Open(dir)
	if err != nil {
		return nil, asEnvError(err)
	}
	var cfg *config.Config
	if cf.config != "" {
		data, err := os.ReadFile(cf.config)
		if err != nil {
			return nil, asConfigError(err)
		}
		if cfg, err = config.Parse(data); err != nil {
			return nil, configError{fmt.Errorf("%s: %w", cf.config, err)}
		}
		if rel, err := filepath.Rel(repo.Root, absPath(cf.config)); err == nil && !strings.HasPrefix(rel, "..") {
			cfg.Path = filepath.ToSlash(rel)
		}
	} else if cfg, err = config.Load(repo.Root); err != nil {
		return nil, asConfigError(err)
	}
	snap, err := repo.Snapshot()
	if err != nil {
		return nil, asEnvError(err)
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
			return nil, asConfigError(err)
		}
	}
	suites, err := runner.Resolve(repo, cfg.Suites)
	if err != nil {
		return nil, asConfigError(err)
	}
	var roots []string
	for _, s := range suites {
		for _, r := range s.PythonRoots {
			roots = append(roots, filepath.ToSlash(filepath.Join(s.Root, r)))
		}
	}
	var others []*runner.Suite
	if len(cf.suites) > 0 {
		for _, n := range cf.suites {
			if !slices.ContainsFunc(suites, func(s *runner.Suite) bool { return s.Name == n }) {
				return nil, configError{fmt.Errorf("unknown suite in %v (have: %s)", []string(cf.suites), suiteNames(suites))}
			}
		}
		var keep []*runner.Suite
		for _, s := range suites {
			if slices.Contains(cf.suites, s.Name) {
				keep = append(keep, s)
			} else {
				others = append(others, s)
			}
		}
		suites = keep
	}
	store := openStore(repo, cfg, cf.state, log)
	statePath := store.path
	st, err := store.Load()
	if errors.Is(err, state.ErrUnreadable) {
		fmt.Fprintf(log, "smates: ignoring unreadable state at %s (%v); starting cold\n", statePath, err)
		st, err = state.New(), nil
	}
	if err != nil {
		return nil, err
	}
	p := &project{
		repo: repo, cfg: cfg, suites: suites, others: others, statePath: statePath, store: store, state: st, snap: snap, files: files,
		graph: static.New(repo.Root, files, static.Options{PythonRoots: roots}), ci: detectCI(), log: log, verbose: cf.verbose, dir: cf.dir,
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
	// noOrphans skips the orphan policy (--only runs exactly the named tests).
	noOrphans bool
	// orphanScope, when non-nil, limits the orphan policy to these files (`smates
	// watch`: files changed since the previous cycle).
	orphanScope map[string]bool
	// orphans overrides the orphan policy (--orphans: agent, full or skip); adds are
	// the tests the caller adds (--add).
	orphans string
	adds    []string
}

// selection runs deterministic selection and the orphan policy.
type selection struct {
	res        *selector.Result
	agentNote  string
	agentDec   *agent.Decision
	agentCache bool
	// mentions are the tests that name a changed file (selector.Mentions).
	mentions []selector.Mention
	// candidates describe the orphans for a calling agent (orphanCandidates).
	candidates []orphanCandidate
	// hint: suites run in full for orphans the caller has not decided about.
	hint bool
}

func (p *project) selectTests(ctx context.Context, opt selectOptions) (*selection, error) {
	if len(p.suites) == 0 {
		return nil, configError{fmt.Errorf("no test suites configured or detected; run `smates init` or write smates.yml")}
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
		Repo: p.repo, Config: p.cfg, Suites: p.suites, Others: p.others, State: p.state, Snap: p.snap, Graph: p.graph, Base: base, All: opt.all,
		DiffUnrecorded: diffUnrecorded,
		NoOrphanPolicy: opt.orphans == orphansSkip || opt.orphans == "" && p.cfg.Orphans == config.OrphansIgnore,
	})
	if err != nil {
		return nil, err
	}
	sel := &selection{res: res}
	if !opt.noOrphans {
		p.handleOrphans(ctx, sel, opt, base)
	}
	if err := p.applyCallerAdds(res, opt.adds); err != nil {
		return nil, err
	}
	sel.candidates = p.orphanCandidates(sel)
	return sel, nil
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
			t.Shard = owner
			if t.Selected && owner != i {
				t.Selected = false
				t.Detail = fmt.Sprintf("in shard %d/%d", owner, n)
			}
		}
		s.All = false
	}
}

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
	"sync"
	"syscall"
	"time"

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
                                        (--claude, --codex: from a Claude Code or Codex Stop hook)
  smates guide                          how a coding agent should use smates
  smates doctor [--format json]         check setup
  smates version

Exit codes:
  0  success
  1  tests failed (also an interrupted run, and a failing 'smates doctor' check)
  2  usage error (invalid command line)
  3  configuration error (invalid smates.yml, unknown runner, a suite or test it does not define)
  4  environment problem (git missing, not a git repository, a suite command that cannot start,
     an npx/bunx tool missing from node_modules, or, in a recorded run of any runner but
     custom, a runner exiting 126/127 before any test reported a result; with --no-record
     or the custom runner such an exit is a test failure; a suite's requires not met;
     a state or report that cannot be read or written; any other unexpected error)

For agents: 'smates run --output failures' (or SMATES_OUTPUT=agent) prints only the output
of failing tests, then a short summary with the command that reruns just those tests.
'smates guide' has the rest.

Run 'smates <command> -h' for flags. Docs: https://dotneet.github.io/smates/
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
	ctx, stop := notifyOnce(context.Background(), os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
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
		if err = noArgs(rest); err == nil {
			_, err = io.WriteString(stdout, docs.Agents)
		}
	case "version", "--version", "-v":
		if err = noArgs(rest); err == nil {
			fmt.Fprintln(stdout, "smates", Version)
		}
	case "help", "-h", "--help":
		fmt.Fprint(stdout, usage)
	default:
		fmt.Fprintf(stderr, "smates: unknown command %q\n\n%s", cmd, usage)
		return 2
	}
	if err != nil {
		// -h and --help: the flag set has printed the usage (to stderr).
		if errors.Is(err, flag.ErrHelp) {
			return ExitOK
		}
		// An error decides the exit code on its own, whatever code came with it.
		code = exitCodeFor(err)
		if code == ExitUsage {
			fmt.Fprintf(stderr, "smates %s: %v\n", cmd, err)
		} else {
			fmt.Fprintf(stderr, "smates: %v\n", err)
		}
	}
	return code
}

// noArgs rejects the arguments of a command that takes none and has no flags.
func noArgs(args []string) error {
	if len(args) > 0 {
		return usageError{fmt.Errorf("unexpected arguments %v", args)}
	}
	return nil
}

// notifyOnce is signal.NotifyContext for the first of sigs only: once ctx is done (the
// first signal, or the parent's cancellation), a second signal terminates smates while
// it still winds down (saving the state waits up to state.LockWait for the lock, a
// runner may ignore the first signal). Before it does, it kills the process groups of
// the tests still running: they are not in the terminal's foreground group, so the
// second Ctrl-C does not reach them, and the SIGKILL their grace would send dies with
// smates.
func notifyOnce(parent context.Context, sigs ...os.Signal) (context.Context, context.CancelFunc) {
	ctx, stop := signal.NotifyContext(parent, sigs...)
	done := make(chan struct{})
	var once sync.Once
	go func() {
		<-ctx.Done()
		second := make(chan os.Signal, 1)
		signal.Notify(second, sigs...) // before stop: no window with the default action
		stop()
		var sig os.Signal
		select {
		case sig = <-second:
		case <-done: // the command returned
			signal.Stop(second)
			return
		}
		runner.KillProcessGroups()
		signal.Reset(sigs...)
		if p, err := os.FindProcess(os.Getpid()); err == nil && p.Signal(sig) == nil {
			time.Sleep(time.Second) // the default action ends smates
		}
		os.Exit(ExitFailed)
	}()
	return ctx, func() {
		stop()
		once.Do(func() { close(done) })
	}
}

// exitCodeFor is the exit code of an error a command returned (see the table in
// usage). Only testsFailedError exits 1: an error nothing classified (a file that
// cannot be written, an unexpected failure of git or the file system) is a problem of
// the environment, never "tests failed", which would send the user (or an agent) to
// fix code that is fine.
func exitCodeFor(err error) int {
	if code, ok := classifiedExit(err); ok {
		return code
	}
	return ExitEnv
}

// classifiedExit is the exit code the type of err decides (usageError, configError,
// envError, testsFailedError, in that order), and whether it has one of them.
func classifiedExit(err error) (int, bool) {
	var ue usageError
	var ce configError
	var ee envError
	var tf testsFailedError
	switch {
	case errors.As(err, &ue):
		return ExitUsage, true
	case errors.As(err, &ce):
		return ExitConfig, true
	case errors.As(err, &ee):
		return ExitEnv, true
	case errors.As(err, &tf):
		return ExitFailed, true
	}
	return 0, false
}

// Exit codes of smates (see `smates help`). A command returns an exit code with a nil
// error (tests failed: ExitFailed), or an error, whose type alone decides the code
// (exitCodeFor): usageError, configError, envError, testsFailedError; any other error
// exits ExitEnv.
const (
	ExitOK     = 0 // success
	ExitFailed = 1 // tests failed (or were interrupted); a failing `smates doctor` check
	ExitUsage  = 2 // invalid command line
	ExitConfig = 3 // invalid configuration: smates.yml, a suite or test the configuration does not define
	ExitEnv    = 4 // environment problem: git, the repository, a runner or tool that cannot start
)

// usageError is an invalid command line (exit code 2).
type usageError struct{ error }

func (e usageError) Unwrap() error { return e.error }

// configError is an invalid configuration (exit code 3): fix smates.yml, or the suite
// or test named on the command line.
type configError struct{ error }

func (e configError) Unwrap() error { return e.error }

// envError is a problem of the environment smates runs in (exit code 4): git missing,
// not a git repository, a runner or tool that is not installed or cannot start.
type envError struct{ error }

func (e envError) Unwrap() error { return e.error }

// testsFailedError ends a run whose tests failed or did not finish (an interrupted run
// whose results were recorded): exit code 1. It is the only error that exits 1.
type testsFailedError struct{ error }

func (e testsFailedError) Unwrap() error { return e.error }

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

// orEnvError returns err unchanged when it already decides an exit code (usageError,
// configError, envError, testsFailedError), else marks it as an environment error.
func orEnvError(err error) error {
	if _, ok := classifiedExit(err); err == nil || ok {
		return err
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
	discovery time.Duration
	stateLoad time.Duration
	// others are the suites --suite left out (they still explain changes, see
	// selector.Input.Others).
	others []*runner.Suite
}

func load(cf *commonFlags, log io.Writer) (*project, error) {
	start := time.Now()
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
	// The working tree's files: a skip-worktree file left out of a sparse checkout is in
	// the snapshot (its hash says it is off disk) but is not a file to detect, list or
	// run, as the selection leaves it out too.
	files := make([]string, 0, len(snap))
	for f, h := range snap {
		if !gitx.IsOffDisk(h) {
			files = append(files, f)
		}
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
	stateStart := time.Now()
	st, err := loadStoreState(store, log)
	if err != nil {
		return nil, err
	}
	stateLoad := time.Since(stateStart)
	p := &project{
		repo: repo, cfg: cfg, suites: suites, others: others, statePath: statePath, store: store, state: st, snap: snap, files: files,
		graph: static.New(repo.Root, files, static.Options{PythonRoots: roots}), ci: detectCI(), log: log, verbose: cf.verbose, dir: cf.dir,
		stateLoad: stateLoad,
	}
	p.discovery = time.Since(start) - stateLoad
	return p, nil
}

// loadStoreState loads the store's state. A state whose content cannot be decoded (damaged,
// or written by a newer smates) is ignored with a warning: the command starts cold
// (selection then runs more, never less) and the next save replaces the file. A file
// that cannot be read at all (permission denied, a directory in its place) is an
// environment error: a save would fail the same way, after a cold run of every test.
func loadStoreState(store *stateStore, log io.Writer) (*state.State, error) {
	st, err := store.Load()
	if errors.Is(err, state.ErrUnreadable) {
		fmt.Fprintf(log, "smates: ignoring unreadable state at %s (%v); starting cold\n", store.path, err)
		return state.New(), nil
	}
	if err != nil {
		return nil, envError{fmt.Errorf("reading the state at %s: %w", store.path, err)}
	}
	return st, nil
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
	// sharded: --shard splits the run, so the agent is not asked (a cached decision
	// is still used): each shard would ask on its own and could get another answer.
	sharded bool
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
	// shards is the shard count `select --max-shards` sized for the selection.
	shards *int
	// diffUnrecorded: tests with no recorded entry were decided against the base
	// revision (Input.DiffUnrecorded), not run as new.
	diffUnrecorded bool
}

// checkBase checks that an explicit --base names a commit: a usage error when it does
// not, an environment error when the clone is shallow (the base was not fetched).
func checkBase(repo *gitx.Repo, base string) error {
	if repo.RefExists(base) {
		return nil
	}
	if out, err := repo.Git("rev-parse", "--is-shallow-repository"); err == nil && strings.TrimSpace(out) == "true" {
		return envError{fmt.Errorf("--base %s: no such revision in this shallow clone; fetch it (actions/checkout with fetch-depth: 0, or git fetch origin <branch>)", base)}
	}
	return usageError{fmt.Errorf("--base %s: no such revision", base)}
}

func (p *project) selectTests(ctx context.Context, opt selectOptions) (*selection, error) {
	if len(p.suites) == 0 {
		return nil, configError{fmt.Errorf("no test suites configured or detected; run `smates init` or write smates.yml")}
	}
	base := opt.base
	// Locally the state only knows the tests that ran on this machine: tests it has
	// not recorded yet are judged against the base branch instead of all running as new.
	// In CI the state is the one the pipeline records, so a test it lacks is new,
	// unless --base names a revision to judge such tests against (suites being
	// adopted one at a time).
	diffUnrecorded := (p.ci.Name == "" || opt.base != "") && !opt.all
	if opt.base != "" && !opt.all {
		if err := checkBase(p.repo, opt.base); err != nil {
			return nil, err
		}
	}
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
		// git failing to diff or list the repository: never "tests failed".
		var ce configError
		if !errors.As(err, &ce) {
			err = asEnvError(err)
		}
		return nil, err
	}
	sel := &selection{res: res, diffUnrecorded: diffUnrecorded && base != ""}
	if !opt.noOrphans {
		p.handleOrphans(ctx, sel, opt, base)
	}
	if err := p.applyCallerAdds(res, opt.adds); err != nil {
		return nil, err
	}
	res.Apply(res.WholeSuites())
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
// (shards may hold different recorded durations and selections). A suite that must run
// whole (mustRunWhole; everything: a shadow run, which runs every test) is never split:
// one shard (wholeShard) owns all of its tests and runs it unfiltered, the others none.
func applyShard(res *selector.Result, i, n int, everything bool) {
	if n <= 1 {
		return
	}
	for _, s := range res.Suites {
		if mustRunWhole(s, everything) {
			owner := wholeShard(s.Name, n)
			for _, t := range s.Tests {
				t.Shard = owner
				if t.Selected && owner != i {
					t.Selected = false
					t.Detail = fmt.Sprintf("in shard %d/%d, which runs the suite whole", owner, n)
				}
			}
			if owner != i {
				s.All = false
			}
			continue
		}
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

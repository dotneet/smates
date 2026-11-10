// Package runner defines test-runner adapters: how a runner's tests are discovered,
// how selected tests are passed to it, and how a run is traced to learn dependencies.
package runner

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/dotneet/smates/internal/config"
	"github.com/dotneet/smates/internal/gitx"
	"github.com/dotneet/smates/internal/pathutil"
)

// Adapter implements one test runner.
type Adapter interface {
	// Name is the runner name used in smates.yml.
	Name() string
	// Defaults returns default settings for a suite rooted at root (repo-relative).
	Defaults(repo *gitx.Repo, root string) Defaults
	// Units groups discovered test files (repo-relative) into unit IDs (repo-relative).
	// For most runners a unit is a test file; for Go it is a package directory.
	Units(s *Suite, testFiles []string) []string
	// Run executes the suite (all tests, or the given units) and, when rc.Record is set,
	// returns per-unit observations. Without rc.Record it returns the statuses the
	// runner reported per unit when it can do so without tracing (no dependencies).
	Run(ctx context.Context, rc *RunContext) (*RunResult, error)
}

// CommandDefaulter is implemented by adapters whose defaults depend on the configured
// command (unittest: the discovery start directory and pattern name the test files).
// Resolve then calls DefaultsFor instead of Defaults; command is "" when none is
// configured.
type CommandDefaulter interface {
	DefaultsFor(repo *gitx.Repo, root, command string) Defaults
}

// SuiteDefaulter is implemented by adapters whose defaults depend on more of the
// suite's configuration (go: GOWORK in env: decides which nested modules belong to the
// suite). Resolve then calls DefaultsForSuite instead of Defaults.
type SuiteDefaulter interface {
	DefaultsForSuite(repo *gitx.Repo, c config.Suite) Defaults
}

// StaticMapper is implemented by adapters whose units depend on less than the static
// import closure of their test files (Go test functions): MapStatic returns the
// dependency keys of unit given that closure.
type StaticMapper interface {
	MapStatic(s *Suite, unit string, closure []string) []string
}

// Commander is implemented by adapters that run something other than the suite
// command with the units appended; Commands returns what Run would execute without
// recording (for --dry-run).
type Commander interface {
	Commands(rc *RunContext) ([][]string, error)
}

// Defaults are adapter-provided defaults.
type Defaults struct {
	Tests   []string // globs relative to the suite root
	Exclude []string // globs relative to the suite root
	Inputs  []string // globs relative to the suite root; a leading "/" means repo-relative
	Command string
}

// Suite is a configured suite with its adapter and defaults applied.
type Suite struct {
	config.Suite
	// Configured is the suite's entry as smates.yml has it, before the defaults (which
	// depend on the machine: a virtualenv, GOWORK) were applied to Suite.
	Configured config.Suite
	Adapter    Adapter
	// Narrowed is set when smates.yml configures tests or exclude: the runner's own
	// discovery would then run a different set of files, so units are always passed.
	Narrowed bool
	// Discovered selects test files from the runner's own configuration (see
	// Discovery); when set it replaces Tests.
	Discovered []Matcher
	// TestsSource says where the test patterns come from: SourceConfig, SourceDefaults
	// or the runner configuration they were read from.
	TestsSource string
	// DiscoveryWarning says why the runner configuration could not be read.
	DiscoveryWarning string
	// ConfigFiles are the runner configuration files discovery read (repo-relative).
	ConfigFiles []string
	// RepoRoot is the absolute repository root ("" when unknown), for adapters whose
	// units are found in the test files' contents.
	RepoRoot string
	// ExtraArgs are the arguments `smates run -- ...` passes through to the runner, set
	// before selection: they can change which test files the runner compiles (Go's
	// -tags, see excludedTestFiles).
	ExtraArgs []string
}

// RunContext carries everything an adapter needs to run a suite.
type RunContext struct {
	Repo  *gitx.Repo
	Suite *Suite
	// Units to run (repo-relative IDs; always the full list). When All is set, adapters
	// may let the runner discover tests itself instead of passing Units.
	Units  []string
	All    bool
	Record bool
	// Fresh asks for outcomes the runner did not take from a cache of earlier runs
	// (shadow mode evaluates the selection against them). Recording always runs fresh.
	Fresh     bool
	ExtraArgs []string
	// TraceDir is an empty directory for trace output owned by this run. Without Record
	// it only receives the per-file results of runner reporters ("" for none).
	TraceDir string
	// ShimDir holds the extracted language shims (see package trace).
	ShimDir string
	// Self is the path of the smates executable (used for exec wrappers).
	Self   string
	Stdout io.Writer
	Stderr io.Writer
	// IsTest reports whether a repo-relative path is a test file of this suite.
	IsTest func(string) bool
	// UnitOf maps a test file to its unit ID ("" if not a test).
	UnitOf func(string) string
	// Known returns the dependency keys a unit is known to have before it runs (static
	// closure, inputs, rules; nil when unknown).
	Known func(unit string) []string
}

// RunResult is the outcome of running a suite.
type RunResult struct {
	ExitCode int
	Duration time.Duration
	// Observations per unit ID (without recording: only reported statuses).
	Observations map[string]*Observation
	// Warnings to show the user (e.g. attribution was coarse).
	Warnings []string
	// ReadsBy maps source files to dependency keys they read when they ran (see
	// trace.Proc.ReadsBy); recording adds them to every unit depending on the reader.
	ReadsBy map[string]map[string]bool
	// excluded holds the Go package directories of the run whose Go files build
	// constraints all exclude in the flags go test got, and that failed for nothing
	// else (see buildFailures.onlyExcl): their tests did not run, a skip.
	excluded map[string]bool
}

// Observation is what tracing saw for one unit.
type Observation struct {
	Files      map[string]bool // repo-relative paths the unit used
	Status     string          // state.Pass / state.Fail / state.Unknown
	DurationMs int64
	// Inferred is set when Status was not reported for this unit but derived from the
	// exit code of a run covering several units (a failure may belong to another unit).
	Inferred bool
	// Failure is the failure output a runner plugin reported for the unit (bounded).
	Failure string
	// BuildFailed is set for a Go test function failed because its package did not
	// build (no test ran), as opposed to a failure the test itself reported.
	BuildFailed bool
	// StaticOnly is set for a pass trust_browser_passes took from the reporter alone (a
	// test that runs in a browser, untraced): its dependencies are the known ones, and
	// recording keeps it unknown when those may not be all (see selector.Record).
	StaticOnly bool
	// NoTrace is set for an outcome a runner reported without tracing anything for the
	// unit (gradle, which records no dependencies yet): the run reports it, but a pass
	// is recorded as unknown, so the unit runs again next time.
	NoTrace bool
	// untraced: the runner reported the unit but its process left no trace (observeAll).
	untraced bool
}

// AddFile adds a repo-relative file.
func (o *Observation) AddFile(p string) {
	if o.Files == nil {
		o.Files = map[string]bool{}
	}
	o.Files[p] = true
}

var registry = map[string]Adapter{}

// Register adds an adapter.
func Register(a Adapter) { registry[a.Name()] = a }

// Get returns the adapter for a runner name.
func Get(name string) (Adapter, bool) {
	a, ok := registry[name]
	return a, ok
}

// Names lists registered runner names.
func Names() []string {
	var n []string
	for k := range registry {
		n = append(n, k)
	}
	sort.Strings(n)
	return n
}

// Resolve applies adapter defaults to configured suites.
func Resolve(repo *gitx.Repo, cs []config.Suite) ([]*Suite, error) {
	var out []*Suite
	for _, c := range cs {
		s, err := NewSuite(repo, c)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, nil
}

// NewSuite applies adapter defaults, and the test files the runner's configuration
// describes, to a configured suite.
func NewSuite(repo *gitx.Repo, c config.Suite) (*Suite, error) {
	a, ok := Get(c.Runner)
	if !ok {
		return nil, fmt.Errorf("suite %q: unknown runner %q (known: %s)", c.Name, c.Runner, strings.Join(Names(), ", "))
	}
	var d Defaults
	if sd, ok := a.(SuiteDefaulter); ok {
		d = sd.DefaultsForSuite(repo, c)
	} else if cd, ok := a.(CommandDefaulter); ok {
		d = cd.DefaultsFor(repo, c.Root, c.Command)
	} else {
		d = a.Defaults(repo, c.Root)
	}
	s := &Suite{Suite: c, Configured: c, Adapter: a, Narrowed: len(c.Tests) > 0 || len(c.Exclude) > 0, TestsSource: SourceConfig}
	if repo != nil {
		s.RepoRoot = repo.Root
	}
	if s.Command == "" {
		s.Command = d.Command
	}
	if s.Command == "" {
		return nil, fmt.Errorf("suite %q: no command configured and none could be inferred", c.Name)
	}
	exclude := d.Exclude
	if len(s.Tests) == 0 {
		s.Tests = d.Tests
		s.TestsSource = SourceDefaults
		if dc, ok := a.(discoverer); ok && repo != nil {
			disc := dc.Discover(repo, s)
			s.DiscoveryWarning, s.ConfigFiles = disc.Warning, disc.Files
			if len(disc.Matchers) > 0 {
				s.Discovered, s.TestsSource, s.Tests = disc.Matchers, disc.Source, nil
				if disc.Exclude != nil {
					exclude = disc.Exclude
				}
			}
		}
	}
	s.Exclude = append(append([]string{}, exclude...), s.Exclude...)
	s.Inputs = append(append([]string{}, d.Inputs...), s.Inputs...)
	return s, nil
}

// TestFiles returns the repo-relative test files of the suite among files.
func (s *Suite) TestFiles(files []string) []string {
	var out []string
	for _, f := range files {
		if s.IsTestFile(f) {
			out = append(out, f)
		}
	}
	sort.Strings(out)
	return out
}

// IsTestFile reports whether a repo-relative path is one of the suite's test files.
func (s *Suite) IsTestFile(f string) bool {
	if !pathutil.Under(f, s.Root) || pathutil.IsVendored(f) {
		return false
	}
	rel := pathutil.RelTo(f, s.Root)
	if pathutil.Match(s.Exclude, rel) {
		return false
	}
	if len(s.Discovered) > 0 {
		for i := range s.Discovered {
			if s.Discovered[i].Match(f) {
				return true
			}
		}
		return false
	}
	return pathutil.Match(s.Tests, rel)
}

// InputMatch reports whether a repo-relative path matches the suite's inputs.
func (s *Suite) InputMatch(f string) bool {
	for _, g := range s.Inputs {
		if strings.HasPrefix(g, "/") {
			if pathutil.Match([]string{g[1:]}, f) {
				return true
			}
			continue
		}
		if pathutil.Under(f, s.Root) && pathutil.Match([]string{g}, pathutil.RelTo(f, s.Root)) {
			return true
		}
	}
	return false
}

// Dir returns the absolute directory of the suite.
func (rc *RunContext) Dir() string {
	return filepath.Join(rc.Repo.Root, filepath.FromSlash(rc.Suite.Root))
}

// RelArgs converts unit IDs to paths relative to the suite root, with prefix (e.g. "./").
func (rc *RunContext) RelArgs(prefix string) []string {
	out := make([]string, 0, len(rc.Units))
	for _, u := range rc.Units {
		r := pathutil.RelTo(u, rc.Suite.Root)
		if prefix != "" && !strings.HasPrefix(r, "../") {
			r = prefix + r
		}
		out = append(out, r)
	}
	return out
}

// CommandError is a suite command that cannot be parsed (a configuration error).
type CommandError struct {
	Suite string
	Err   error
}

func (e *CommandError) Error() string { return fmt.Sprintf("suite %q command: %v", e.Suite, e.Err) }
func (e *CommandError) Unwrap() error { return e.Err }

// CommandLine builds argv from the suite command: "{tests}" is replaced by testArgs,
// otherwise testArgs are appended; extra args go before the tests.
func (rc *RunContext) CommandLine(extra []string, testArgs []string) ([]string, error) {
	words, err := SplitWords(rc.Suite.Command)
	if err != nil {
		return nil, &CommandError{Suite: rc.Suite.Name, Err: err}
	}
	var argv []string
	replaced := false
	for _, w := range words {
		if w == "{tests}" {
			argv = append(argv, extra...)
			argv = append(argv, rc.ExtraArgs...)
			argv = append(argv, testArgs...)
			replaced = true
			continue
		}
		argv = append(argv, w)
	}
	if !replaced {
		argv = append(argv, extra...)
		argv = append(argv, rc.ExtraArgs...)
		argv = append(argv, testArgs...)
	}
	return argv, nil
}

// Exec runs argv in the suite directory with extra environment, streaming output.
// It returns the process exit code (non-zero codes are not errors).
func (rc *RunContext) Exec(ctx context.Context, argv []string, env []string) (int, error) {
	return rc.ExecIn(ctx, rc.Dir(), argv, env, rc.Stdout, rc.Stderr)
}

// ExecIn is Exec with an explicit directory and output streams.
func (rc *RunContext) ExecIn(ctx context.Context, dir string, argv []string, env []string, stdout, stderr io.Writer) (int, error) {
	if len(argv) == 0 {
		return 0, fmt.Errorf("empty command")
	}
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir = dir
	cmd.Env = withPWD(MergeEnv(baseEnv(), suiteEnv(rc.Suite), env), dir)
	cmd.Stdout, cmd.Stderr, cmd.Stdin = stdout, stderr, nil
	err := runCmd(ctx, cmd)
	if ee, ok := err.(*exec.ExitError); ok {
		code := ee.ExitCode()
		if code < 0 {
			code = 1
		}
		return code, nil
	}
	if err != nil {
		return 127, fmt.Errorf("running %s: %w", argv[0], err)
	}
	return 0, nil
}

// withPWD sets PWD in env to dir, the directory a command runs in, as exec.Cmd does
// itself only for an inherited environment. Programs that trust $PWD when it names
// their directory (os.Getwd, so the go command) would otherwise see the user's PWD: a
// path through a symlink (/tmp on macOS) that the repository root, resolved, does not
// match, or another directory altogether. Windows has no PWD.
func withPWD(env []string, dir string) []string {
	if runtime.GOOS == "windows" || dir == "" {
		return env
	}
	if abs, err := filepath.Abs(dir); err == nil {
		return MergeEnv(env, []string{"PWD=" + abs})
	}
	return env
}

// AgentSecrets are the credentials of every agent provider (see package agent): tests
// do not get them, whichever provider smates uses (a suite that needs them can set them
// in env:).
var AgentSecrets = []string{"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "OPENAI_API_KEY", "OPENROUTER_API_KEY"}

// baseEnv is smates' environment without AgentSecrets.
func baseEnv() []string {
	var out []string
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		if !slices.Contains(AgentSecrets, k) {
			out = append(out, kv)
		}
	}
	return out
}

// LookupEnv returns an environment variable as the tests of suite s (nil: no suite)
// see it: the suite's env, otherwise smates' own environment without AgentSecrets.
// Variables the adapters set to trace a run are not taken into account.
func (s *Suite) LookupEnv(name string) (string, bool) {
	if s != nil {
		if v, ok := s.Env[name]; ok {
			return v, true
		}
	}
	if slices.Contains(AgentSecrets, name) {
		return "", false
	}
	return os.LookupEnv(name)
}

func suiteEnv(s *Suite) []string {
	var env []string
	for k, v := range s.Env {
		env = append(env, k+"="+v)
	}
	sort.Strings(env)
	return env
}

// MergeEnv overlays KEY=VALUE lists; later lists win.
func MergeEnv(lists ...[]string) []string {
	idx := map[string]int{}
	var out []string
	for _, l := range lists {
		for _, kv := range l {
			k := kv
			if i := strings.IndexByte(kv, '='); i >= 0 {
				k = kv[:i]
			}
			if j, ok := idx[k]; ok {
				out[j] = kv
				continue
			}
			idx[k] = len(out)
			out = append(out, kv)
		}
	}
	return out
}

// Getenv looks a key up in a KEY=VALUE list, falling back to the process environment.
func Getenv(env []string, key string) string {
	for i := len(env) - 1; i >= 0; i-- {
		if strings.HasPrefix(env[i], key+"=") {
			return env[i][len(key)+1:]
		}
	}
	return os.Getenv(key)
}

// SplitWords splits a command line using POSIX shell quoting rules (no expansion).
func SplitWords(s string) ([]string, error) {
	var words []string
	var cur strings.Builder
	inWord := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '\\' && i+1 < len(s):
			i++
			cur.WriteByte(s[i])
			inWord = true
		case c == '\'':
			j := strings.IndexByte(s[i+1:], '\'')
			if j < 0 {
				return nil, fmt.Errorf("unterminated single quote")
			}
			cur.WriteString(s[i+1 : i+1+j])
			i += j + 1
			inWord = true
		case c == '"':
			i++
			for ; i < len(s) && s[i] != '"'; i++ {
				if s[i] == '\\' && i+1 < len(s) && strings.IndexByte("\"\\$`", s[i+1]) >= 0 {
					i++
				}
				cur.WriteByte(s[i])
			}
			if i >= len(s) {
				return nil, fmt.Errorf("unterminated double quote")
			}
			inWord = true
		case c == ' ' || c == '\t' || c == '\n':
			if inWord {
				words = append(words, cur.String())
				cur.Reset()
				inWord = false
			}
		default:
			cur.WriteByte(c)
			inWord = true
		}
	}
	if inWord {
		words = append(words, cur.String())
	}
	return words, nil
}

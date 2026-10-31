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
	// returns per-unit observations.
	Run(ctx context.Context, rc *RunContext) (*RunResult, error)
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
	Adapter Adapter
}

// RunContext carries everything an adapter needs to run a suite.
type RunContext struct {
	Repo  *gitx.Repo
	Suite *Suite
	// Units to run (repo-relative IDs; always the full list). When All is set, adapters
	// may let the runner discover tests itself instead of passing Units.
	Units     []string
	All       bool
	Record    bool
	ExtraArgs []string
	// TraceDir is an empty directory for trace output owned by this run.
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
}

// RunResult is the outcome of running a suite.
type RunResult struct {
	ExitCode int
	Duration time.Duration
	// Observations per unit ID (only when recording).
	Observations map[string]*Observation
	// Warnings to show the user (e.g. attribution was coarse).
	Warnings []string
	// ReadsBy maps source files to dependency keys they read when they ran (see
	// trace.Proc.ReadsBy); recording adds them to every unit depending on the reader.
	ReadsBy map[string]map[string]bool
}

// Observation is what tracing saw for one unit.
type Observation struct {
	Files      map[string]bool // repo-relative paths the unit used
	Status     string          // state.Pass / state.Fail / state.Unknown
	DurationMs int64
	// Inferred is set when Status was not reported for this unit but derived from the
	// exit code of a run covering several units (a failure may belong to another unit).
	Inferred bool
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
		a, ok := Get(c.Runner)
		if !ok {
			return nil, fmt.Errorf("suite %q: unknown runner %q (known: %s)", c.Name, c.Runner, strings.Join(Names(), ", "))
		}
		d := a.Defaults(repo, c.Root)
		s := &Suite{Suite: c, Adapter: a}
		if len(s.Tests) == 0 {
			s.Tests = d.Tests
		}
		s.Exclude = append(append([]string{}, d.Exclude...), s.Exclude...)
		s.Inputs = append(append([]string{}, d.Inputs...), s.Inputs...)
		if s.Command == "" {
			s.Command = d.Command
		}
		if s.Command == "" {
			return nil, fmt.Errorf("suite %q: no command configured and none could be inferred", c.Name)
		}
		out = append(out, s)
	}
	return out, nil
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
	return pathutil.Match(s.Tests, rel) && !pathutil.Match(s.Exclude, rel)
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

// CommandLine builds argv from the suite command: "{tests}" is replaced by testArgs,
// otherwise testArgs are appended; extra args go before the tests.
func (rc *RunContext) CommandLine(extra []string, testArgs []string) ([]string, error) {
	words, err := SplitWords(rc.Suite.Command)
	if err != nil {
		return nil, fmt.Errorf("suite %q command: %w", rc.Suite.Name, err)
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
	cmd.Env = MergeEnv(baseEnv(), suiteEnv(rc.Suite), env)
	cmd.Stdout, cmd.Stderr, cmd.Stdin = stdout, stderr, nil
	err := cmd.Run()
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

// agentSecrets are smates' own credentials: tests do not get them (a suite that needs
// them can set them in env:).
var agentSecrets = map[string]bool{"ANTHROPIC_API_KEY": true, "ANTHROPIC_AUTH_TOKEN": true}

func baseEnv() []string {
	var out []string
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		if !agentSecrets[k] {
			out = append(out, kv)
		}
	}
	return out
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

// Exists reports whether a path relative to the repo root exists.
func Exists(repo *gitx.Repo, rel string) bool {
	_, err := os.Stat(filepath.Join(repo.Root, filepath.FromSlash(rel)))
	return err == nil
}

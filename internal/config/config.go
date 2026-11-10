// Package config loads smates.yml.
package config

import (
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/bmatcuk/doublestar/v4"
	"gopkg.in/yaml.v3"
)

// FileNames are the config file names looked up at the repository root, in order.
var FileNames = []string{"smates.yml", "smates.yaml", ".smates.yml", ".smates.yaml"}

// Config is the whole configuration.
type Config struct {
	Version int     `yaml:"version"`
	Suites  []Suite `yaml:"suites"`
	// Ignore: changed files matching these globs are never orphans (docs, images...).
	Ignore []string `yaml:"ignore"`
	// Inputs: files every test of every suite depends on (lockfiles, CI toolchain pins...).
	Inputs []string `yaml:"inputs"`
	// Orphans: what to do with changed files no test is known to depend on:
	// "agent" (ask the agent, fall back to all), "all", or "ignore".
	Orphans string `yaml:"orphans"`
	// Untested: what a change to source code no test reaches is: "skip" (default: no
	// test can be affected; reported as a change to untested code) or "orphan" (handled
	// like any other orphan).
	Untested string      `yaml:"untested"`
	Agent    AgentConfig `yaml:"agent"`
	// State: path of the state file, relative to the repository root.
	State string `yaml:"state"`
	// StateSeed: in a linked git worktree without a state, start from the main
	// worktree's state (or the remote one) instead of cold (default true).
	StateSeed *bool `yaml:"state_seed"`
	// StateShared: keep the per-test entries in one file under the git common
	// directory, shared by all worktrees; each worktree keeps its own snapshot.
	StateShared bool `yaml:"state_shared"`
	// FullRunBranches: branches on which `smates run` runs everything (default: the default branch).
	FullRunBranches []string `yaml:"full_run_branches"`
	// Flaky configures what happens to tests known to be flaky.
	Flaky FlakyConfig `yaml:"flaky"`
	// EnvIgnore: environment variables (names or globs such as "MY_*") whose values
	// tests may read without being selected again when they change; "*" turns off
	// environment tracking. Added to DefaultEnvIgnore.
	EnvIgnore []string `yaml:"env_ignore"`
	// EnvTrack: variables to track although DefaultEnvIgnore lists them (e.g. "LANG").
	EnvTrack []string `yaml:"env_track"`

	// Path is the file the config was loaded from ("" when auto-detected).
	Path string `yaml:"-"`
}

// Suite is one test runner invocation rooted at a directory.
type Suite struct {
	Name string `yaml:"name"`
	// Root is the directory (repo-relative) the command runs in.
	Root string `yaml:"root"`
	// Runner: vitest | jest | node | bun | pytest | go | custom
	Runner string `yaml:"runner"`
	// Tests are globs (relative to Root) that identify test files.
	Tests   []string `yaml:"tests"`
	Exclude []string `yaml:"exclude"`
	// Command is the base command line; selected test paths are appended, or substituted
	// for "{tests}" when present.
	Command string `yaml:"command"`
	// Inputs are extra globs (relative to Root, or repo-relative with a leading "/")
	// every test of the suite depends on.
	Inputs []string `yaml:"inputs"`
	Rules  []Rule   `yaml:"rules"`
	// DependsOn declares code the suite's tests exercise across a process boundary
	// tracing cannot follow (a server started by docker compose, an HTTP API): another
	// suite's code, or repo-relative globs.
	DependsOn []Dependency `yaml:"depends_on"`
	// Serves: repo-relative globs of the code of a server the suite's tests talk to;
	// shorthand for depends_on: [{paths: [...]}].
	Serves []string          `yaml:"serves"`
	Env    map[string]string `yaml:"env"`
	// Isolate re-runs each test file in its own process while recording, for runners
	// that share worker processes between files (jest, vitest with isolate: false).
	Isolate *bool `yaml:"isolate"`
	// RecordJobs bounds how many per-file processes run at once while recording with
	// one process per file (bun always; jest/vitest/node with isolate). Default: 1 for bun
	// (bun test runs files one after another) and for commands that run files serially
	// (--runInBand, --no-file-parallelism, --test-isolation=none, --test-concurrency=1),
	// the CPU count otherwise. With Go
	// granularity "func", it bounds how many test functions of one package run at once
	// while recording (default 1, as go test runs them one after another).
	RecordJobs int `yaml:"record_jobs"`
	// Granularity of Go units: "package" (default) or "func" (one unit per top-level
	// test function, selected by per-test coverage; recording runs one process per
	// function).
	Granularity string `yaml:"granularity"`
	// Static enables import-graph analysis (default true).
	Static *bool `yaml:"static"`
	// TrustBrowserPasses records a vitest test that runs only in a browser (browser
	// mode, Storybook stories), where no trace sees what it loads, as passed when the
	// reporter says so, with its static dependencies; otherwise (the default) such a
	// test is unknown after every run and runs again next time. Left out of the
	// settings hash when unset, so that the hash of a suite that does not set it is the
	// one an older smates recorded.
	TrustBrowserPasses *bool `yaml:"trust_browser_passes" json:",omitempty"`
	// Junit: optional path (relative to Root) of a JUnit XML report the command writes;
	// used for per-file pass/fail when the runner cannot report it otherwise.
	Junit string `yaml:"junit"`
	// PythonRoots: extra import roots for Python static analysis (relative to Root).
	PythonRoots []string `yaml:"python_roots"`
	// EnvIgnore and EnvTrack add to the top-level lists for this suite only.
	EnvIgnore []string `yaml:"env_ignore"`
	EnvTrack  []string `yaml:"env_track"`
	// Volatile: globs (relative to Root, matched like the tests of a rule) of the test
	// units that run every time, whatever changed: tests that depend on something
	// smates cannot track, such as the date.
	Volatile []string `yaml:"volatile" json:"-"`
	// Untraced: globs (relative to Root, matched like Volatile) of the test units that
	// must run without tracing while smates records, because tracing changes what they
	// measure (V8 coverage slows code down unevenly, so a test asserting timings or
	// their ratios fails under it). They run in batches of their own, and a pass is
	// recorded with the dependencies smates knows statically (selector.UntracedUnit).
	// Left out of the settings hash: it changes how the tests are observed, not what
	// they test, and adding a glob must not rerun the suite.
	Untraced []string `yaml:"untraced" json:"-"`
	// MaxAge: a unit whose last recorded run is older than this runs again, whatever
	// changed (0: never). Neither it nor Volatile changes how the tests run, so they are
	// left out of the settings hash (selector.ConfigHash).
	MaxAge time.Duration `yaml:"max_age" json:"-"`
	// Requires are preconditions of the environment (a database listening, a browser
	// installed): when one is not met, `smates run` does not start the units it covers
	// and leaves them selected. Like Volatile and MaxAge they choose when tests run, not
	// how, so they are left out of the settings hash: adding a check or tuning its
	// timeout reruns nothing.
	Requires []Requirement `yaml:"requires" json:"-"`
}

// Rule declares dependencies tracing cannot see. It needs Tests or Reaches; with both,
// a unit must match both.
type Rule struct {
	// Tests: globs relative to the suite root; Deps: repo-relative globs.
	Tests []string `yaml:"tests"`
	// Reaches narrows the rule to the units whose static import closure reaches a file
	// one of these repo-relative globs matches, or, for an entry "$NAME" (a glob over
	// variable names), whose recorded run read that environment variable. A unit whose
	// closure is unknown (static analysis off, no analyzer for its files) gets the deps.
	// Left out of the settings hash when empty, so that the hash of a suite that does
	// not use it is the one an older smates recorded.
	Reaches []string `yaml:"reaches" json:",omitempty"`
	Deps    []string `yaml:"deps"`
}

// ReachesEnv splits Reaches into file globs and environment variable name patterns
// (the "$NAME" entries, without the "$").
func (r Rule) ReachesEnv() (files, env []string) {
	for _, g := range r.Reaches {
		if name, ok := strings.CutPrefix(g, "$"); ok {
			env = append(env, name)
		} else {
			files = append(files, g)
		}
	}
	return files, env
}

// checkRules validates the rules of suite s, and strips a leading "./" from their
// globs.
func checkRules(s *Suite) error {
	for i := range s.Rules {
		r := &s.Rules[i]
		at := fmt.Sprintf("suite %q: rules[%d]", s.Name, i)
		trimDotSlash(r.Tests)
		trimDotSlash(r.Deps)
		for j, g := range r.Reaches {
			if !strings.HasPrefix(g, "$") {
				for strings.HasPrefix(g, "./") {
					g = g[2:]
				}
				r.Reaches[j] = g
			}
		}
		files, env := r.ReachesEnv()
		switch {
		case len(r.Tests) == 0 && len(r.Reaches) == 0:
			return fmt.Errorf("%s: needs tests or reaches (a rule with neither applies to no test)", at)
		case len(r.Deps) == 0:
			return fmt.Errorf("%s: needs deps", at)
		}
		if err := checkGlobs(at+": tests", r.Tests); err != nil {
			return err
		}
		if err := checkRepoGlobs(at+": deps", r.Deps); err != nil {
			return err
		}
		if err := checkRepoGlobs(at+": reaches", files); err != nil {
			return err
		}
		if err := checkEnvPatterns(at+": reaches", env); err != nil {
			return err
		}
	}
	return nil
}

// Requirement is one entry of Suite.Requires: something the environment must provide
// for the suite's tests to run. Exactly one of TCP, Cmd, Env and File is set.
type Requirement struct {
	// TCP: "host:port" that must accept a TCP connection.
	TCP string `yaml:"tcp"`
	// Cmd: a shell command, run in the suite root with the suite's env, that must exit 0.
	Cmd string `yaml:"cmd"`
	// Env: an environment variable (or a key of the suite's env) that must be non-empty.
	Env string `yaml:"env"`
	// File: a path or glob, relative to the suite root or absolute, that must match an
	// existing file or directory.
	File string `yaml:"file"`
	// Tests: globs relative to the suite root narrowing the units that need it
	// (default: all).
	Tests []string `yaml:"tests"`
	// Timeout bounds a tcp or cmd check (default DefaultTCPTimeout, DefaultCmdTimeout).
	Timeout time.Duration `yaml:"timeout"`
}

// Default timeouts of the requires checks.
const (
	DefaultTCPTimeout = 2 * time.Second
	DefaultCmdTimeout = 10 * time.Second
)

// requirementKeys are the keys of a requires entry.
var requirementKeys = []string{"tcp", "cmd", "env", "file", "tests", "timeout"}

// UnmarshalYAML reports an unknown key of a requires entry with the keys it accepts.
func (r *Requirement) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind != yaml.MappingNode {
		hint := ""
		if n.Kind == yaml.ScalarNode {
			hint = fmt.Sprintf(" (for an address, write `tcp: %s`)", n.Value)
		}
		return fmt.Errorf("line %d: a requires entry is a mapping with one of tcp, cmd, env and file%s", n.Line, hint)
	}
	// Decoding through a custom unmarshaler loses the decoder's KnownFields check.
	{
		for i := 0; i+1 < len(n.Content); i += 2 {
			if k := n.Content[i]; !slices.Contains(requirementKeys, k.Value) {
				return fmt.Errorf("line %d: unknown key %q in requires entry (want one of tcp, cmd, env and file, with optional tests and timeout)", k.Line, k.Value)
			}
		}
	}
	type plain Requirement
	return n.Decode((*plain)(r))
}

// Kind is the kind of check: "tcp", "cmd", "env" or "file" ("" when none is set).
func (r Requirement) Kind() string {
	kind, _ := r.kindValue()
	return kind
}

func (r Requirement) kindValue() (string, string) {
	switch {
	case r.TCP != "":
		return "tcp", r.TCP
	case r.Cmd != "":
		return "cmd", r.Cmd
	case r.Env != "":
		return "env", r.Env
	case r.File != "":
		return "file", r.File
	}
	return "", ""
}

// String describes the requirement in logs ("tcp 127.0.0.1:5432").
func (r Requirement) String() string {
	kind, v := r.kindValue()
	return kind + " " + v
}

// TimeoutOrDefault is the time a tcp or cmd check may take.
func (r Requirement) TimeoutOrDefault() time.Duration {
	switch {
	case r.Timeout > 0:
		return r.Timeout
	case r.Cmd != "":
		return DefaultCmdTimeout
	}
	return DefaultTCPTimeout
}

var envNameRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// checkRequires validates requires, and strips a leading "./" from its tests globs.
func checkRequires(s *Suite) error {
	for i := range s.Requires {
		r := &s.Requires[i]
		at := fmt.Sprintf("suite %q: requires[%d]", s.Name, i)
		set := 0
		for _, v := range []string{r.TCP, r.Cmd, r.Env, r.File} {
			if v != "" {
				set++
			}
		}
		if set != 1 {
			return fmt.Errorf("%s: set exactly one of tcp, cmd, env and file", at)
		}
		switch {
		case strings.TrimSpace(r.TCP+r.Cmd+r.Env+r.File) == "":
			return fmt.Errorf("%s: %s is empty", at, r.Kind())
		case r.TCP != "":
			if err := CheckVarRefs(r.TCP); err != nil {
				return fmt.Errorf("%s: tcp %q: %v", at, r.TCP, err)
			}
			if err := checkHostPort(r.TCP); err != nil {
				return fmt.Errorf("%s: tcp %q: %v", at, r.TCP, err)
			}
		case r.Env != "":
			if !envNameRe.MatchString(r.Env) {
				return fmt.Errorf("%s: env %q is not a variable name", at, r.Env)
			}
		case r.File != "":
			if err := CheckVarRefs(r.File); err != nil {
				return fmt.Errorf("%s: file %q: %v", at, r.File, err)
			}
			if strings.Contains(r.File, "\\") {
				return fmt.Errorf("%s: file %q: use / as the path separator", at, r.File)
			}
			if !doublestar.ValidatePattern(r.File) {
				return fmt.Errorf("%s: file: bad glob %q", at, r.File)
			}
		}
		if r.Timeout < 0 {
			return fmt.Errorf("%s: timeout must not be negative (got %s)", at, r.Timeout)
		}
		if r.Timeout > 0 && r.TCP == "" && r.Cmd == "" {
			return fmt.Errorf("%s: timeout applies to tcp and cmd only", at)
		}
		trimDotSlash(r.Tests)
		if err := checkGlobs(at+": tests", r.Tests); err != nil {
			return err
		}
	}
	return nil
}

// CheckVarRefs checks that every "$" of a tcp or file requirement starts a variable
// reference smates expands: $NAME or ${NAME}. Shell forms it does not expand (a
// default as in ${NAME:-x} or ${NAME-x}, ${NAME:?}, $1, $$, ${}) would be read as an
// unset variable on every machine, leaving the suite's tests out for good.
func CheckVarRefs(v string) error {
	const forms = "write $NAME or ${NAME} (smates expands no defaults such as ${NAME:-x}, no $1 and no $$; for a default, set the variable where smates runs, or use a cmd requirement, whose shell expands it; a suite env value overrides the environment, so it is no default)"
	for i := 0; i < len(v); i++ {
		if v[i] != '$' {
			continue
		}
		rest := v[i+1:]
		if strings.HasPrefix(rest, "{") {
			end := strings.IndexByte(rest, '}')
			if end < 0 {
				return fmt.Errorf("%q has no closing }: %s", v[i:], forms)
			}
			if body := rest[1:end]; !envNameRe.MatchString(body) {
				return fmt.Errorf("${%s} is not a variable reference: %s", body, forms)
			}
			i += end + 1
			continue
		}
		n := 0
		for n < len(rest) && (rest[n] == '_' || rest[n] >= 'a' && rest[n] <= 'z' || rest[n] >= 'A' && rest[n] <= 'Z' || n > 0 && rest[n] >= '0' && rest[n] <= '9') {
			n++
		}
		if n == 0 {
			ref := "$"
			if rest != "" {
				ref += rest[:1]
			}
			return fmt.Errorf("%q is not a variable reference: %s", ref, forms)
		}
		i += n
	}
	return nil
}

// checkHostPort validates a "host:port" address; one with environment variables is
// checked once they are expanded, at run time.
func checkHostPort(addr string) error {
	if strings.Contains(addr, "$") {
		return nil
	}
	return CheckHostPort(addr)
}

// CheckHostPort validates a tcp requirement's address once its variables are expanded:
// a host and a port from 1 to 65535.
func CheckHostPort(addr string) error {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return errors.New("want host:port")
	}
	if host == "" {
		return errors.New("want host:port (the host is missing)")
	}
	if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
		return fmt.Errorf("bad port %q", port)
	}
	return nil
}

// Dependency is one entry of Suite.DependsOn: exactly one of Suite and Paths. A plain
// string in smates.yml is a suite name.
type Dependency struct {
	// Suite: the tests depend on that suite's code, i.e. the files under its root
	// except its test files, ignored and vendored files, and files of other suites
	// rooted below it.
	Suite string `yaml:"suite"`
	// Paths: repo-relative globs.
	Paths []string `yaml:"paths"`
	// Tests: globs relative to the suite root narrowing the tests that get the
	// dependency (default: all).
	Tests []string `yaml:"tests"`
	// FromServes marks the entry Suite.Dependencies derives from serves.
	FromServes bool `yaml:"-"`
}

// UnmarshalYAML accepts a suite name as a plain string.
func (d *Dependency) UnmarshalYAML(n *yaml.Node) error {
	switch n.Kind {
	case yaml.ScalarNode:
		return n.Decode(&d.Suite)
	case yaml.MappingNode:
	default:
		return fmt.Errorf("line %d: a depends_on or serves entry is a suite name or a mapping with suite or paths", n.Line)
	}
	// Decoding through a custom unmarshaler loses the decoder's KnownFields check.
	{
		for i := 0; i+1 < len(n.Content); i += 2 {
			switch k := n.Content[i]; k.Value {
			case "suite", "paths", "tests":
			default:
				return fmt.Errorf("line %d: field %s not found in depends_on entry (suite, paths, tests)", k.Line, k.Value)
			}
		}
	}
	type plain Dependency
	return n.Decode((*plain)(d))
}

// String describes the dependency in explanations ("depends_on suite api",
// "serves backend/**").
func (d Dependency) String() string {
	if d.Suite != "" {
		return "depends_on suite " + d.Suite
	}
	what := strings.Join(d.Paths, ", ")
	if len(d.Paths) > 2 {
		what = strings.Join(d.Paths[:2], ", ") + ", …"
	}
	if d.FromServes {
		return "serves " + what
	}
	return "depends_on " + what
}

// Dependencies returns depends_on with serves appended as a paths entry.
func (s *Suite) Dependencies() []Dependency {
	out := slices.Clone(s.DependsOn)
	if len(s.Serves) > 0 {
		out = append(out, Dependency{Paths: s.Serves, FromServes: true})
	}
	return out
}

// checkDependencies validates depends_on and serves, and strips a leading "./" from
// their globs (which would otherwise match nothing).
func checkDependencies(s *Suite, names map[string]bool) error {
	where := fmt.Sprintf("suite %q", s.Name)
	trimDotSlash(s.Serves)
	for i, d := range s.DependsOn {
		trimDotSlash(d.Paths)
		trimDotSlash(d.Tests)
		at := fmt.Sprintf("%s: depends_on[%d]", where, i)
		switch {
		case d.Suite != "" && len(d.Paths) > 0:
			return fmt.Errorf("%s: set either suite or paths, not both", at)
		case d.Suite == "" && len(d.Paths) == 0:
			return fmt.Errorf("%s: needs suite or paths", at)
		case d.Suite == s.Name:
			return fmt.Errorf("%s: a suite cannot depend on itself", at)
		case d.Suite != "" && !names[d.Suite]:
			return fmt.Errorf("%s: unknown suite %q", at, d.Suite)
		}
		if err := checkRepoGlobs(at+": paths", d.Paths); err != nil {
			return err
		}
		if err := checkGlobs(at+": tests", d.Tests); err != nil {
			return err
		}
	}
	return checkRepoGlobs(where+": serves", s.Serves)
}

// trimDotSlash removes leading "./" from globs in place.
func trimDotSlash(globs []string) {
	for i, g := range globs {
		for strings.HasPrefix(g, "./") {
			g = g[2:]
		}
		globs[i] = g
	}
}

func checkGlobs(where string, globs []string) error {
	for _, g := range globs {
		if strings.Contains(g, "\\") {
			return fmt.Errorf("%s: %q: use / as the path separator (a backslash escapes the next character in a glob)", where, g)
		}
		if g == "" || !doublestar.ValidatePattern(g) {
			return fmt.Errorf("%s: bad glob %q", where, g)
		}
	}
	return nil
}

// checkRepoGlobs validates repo-relative globs.
func checkRepoGlobs(where string, globs []string) error {
	for _, g := range globs {
		if strings.HasPrefix(g, "/") || g == ".." || strings.HasPrefix(g, "../") {
			return fmt.Errorf("%s: %q must be relative to the repository root", where, g)
		}
	}
	return checkGlobs(where, globs)
}

// AgentConfig configures the LLM agent used for orphan files.
type AgentConfig struct {
	// Enabled: "auto" (when credentials are present), "always" or "never".
	Enabled string `yaml:"enabled"`
	// Provider of the built-in backend: "auto" (the first of anthropic, openai and
	// openrouter whose API key is set), "anthropic", "openai" or "openrouter".
	Provider string `yaml:"provider"`
	// Model: default depends on the provider.
	Model    string        `yaml:"model"`
	MaxTurns int           `yaml:"max_turns"`
	Timeout  time.Duration `yaml:"timeout"`
	// Command runs an external agent instead of the built-in one; the prompt is
	// written to stdin and a JSON object with a "decision" field is read from stdout.
	Command string `yaml:"command"`
	// Effort: low | medium | high
	Effort string `yaml:"effort"`
	// Instructions are appended to the system prompt (project-specific hints).
	Instructions string `yaml:"instructions"`
	// ExplainFailures lets `smates run` ask the agent for the probable cause of failing
	// tests (default true; only with the built-in backend).
	ExplainFailures *bool `yaml:"explain_failures"`
	// ExplainMax bounds the failing tests explained per run (default 3).
	ExplainMax int `yaml:"explain_max"`
}

// FlakyConfig: smates always detects and reports flaky tests (a unit whose outcome
// flipped while its inputs stayed the same); a failure is never hidden by default.
type FlakyConfig struct {
	// Retry re-runs a suite run whose failures are all units already known to be flaky,
	// with the same units, up to this many times; when it then passes, the flip is
	// reported and the run does not fail. Default 0.
	Retry int `yaml:"retry"`
}

// DefaultIgnore are changes that never need tests unless some test reads them.
var DefaultIgnore = []string{
	"**/*.md", "**/*.mdx", "**/*.rst", "**/*.txt.license", "**/LICENSE*", "**/CHANGELOG*", "**/CODEOWNERS",
	"docs/**", "doc/**", ".github/ISSUE_TEMPLATE/**", ".github/PULL_REQUEST_TEMPLATE*", ".github/dependabot.yml",
	".vscode/**", ".idea/**", ".editorconfig", "**/.gitignore", "**/.gitattributes", ".devcontainer/**",
	"**/.prettierrc*", "**/.prettierignore", "**/.eslintrc*", "**/eslint.config.*", "**/.markdownlint*",
	"**/biome.json", "**/biome.jsonc", "**/.golangci.y*ml", "**/ruff.toml", "**/.pre-commit-config.yaml",
}

// DefaultEnvIgnore are environment variables whose reads are not tracked: their values
// differ between machines, between CI and a developer's shell, or between runs, while
// tests rarely depend on them. A state recorded on CI and pulled to a laptop would
// otherwise select every test that reads one of them. Names are matched with path.Match
// globs, case-sensitively.
var DefaultEnvIgnore = []string{
	// The user, the machine and the shell session.
	"PATH", "HOME", "USER", "USERNAME", "LOGNAME", "SHELL", "PWD", "OLDPWD", "SHLVL", "_", "HOSTNAME", "HOST",
	"MAIL", "TMPDIR", "TMP", "TEMP", "XDG_*", "SSH_*", "GPG_*", "DISPLAY", "WAYLAND_DISPLAY", "DBUS_*",
	"EDITOR", "VISUAL", "PAGER", "LESS*", "MANPATH", "INFOPATH", "__CF*", "COMMAND_MODE", "SECURITYSESSIONID",
	"USERPROFILE", "APPDATA", "LOCALAPPDATA", "HOMEDRIVE", "HOMEPATH", "COMPUTERNAME", "PATHEXT", "COMSPEC",
	"SYSTEMROOT", "SystemRoot", "WINDIR", "windir", "PROGRAMDATA", "ProgramData", "PROGRAMFILES*", "ProgramFiles*",
	"PROCESSOR_*", "NUMBER_OF_PROCESSORS", "OS",
	// Locale.
	"LANG", "LANGUAGE", "LC_*",
	// Terminals and colors.
	"TERM", "TERM_*", "COLORTERM", "COLUMNS", "LINES", "NO_COLOR", "FORCE_COLOR", "CLICOLOR*", "NODE_DISABLE_COLORS",
	"PY_COLORS", "TMUX*", "STY", "ITERM_*", "VSCODE_*", "WT_SESSION", "INSIDE_EMACS",
	// CI systems.
	"CI", "CONTINUOUS_INTEGRATION", "GITHUB_*", "RUNNER_*", "ACTIONS_*", "ImageOS", "ImageVersion", "GITLAB_*", "CI_*",
	"CIRCLE*", "TRAVIS*", "JENKINS_*", "BUILD_NUMBER", "BUILD_ID", "BUILD_URL", "BUILDKITE*", "TF_BUILD", "AGENT_*",
	"TEAMCITY_*", "APPVEYOR*", "DRONE*", "BITBUCKET_*", "CODEBUILD_*",
	// Proxies.
	"HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY", "ALL_PROXY", "http_proxy", "https_proxy", "no_proxy", "all_proxy",
	// Toolchain and package manager locations, runner workers.
	"npm_*", "NVM_*", "VOLTA_*", "FNM_*", "COREPACK_*", "JEST_WORKER_ID", "VITEST_POOL_ID", "VITEST_WORKER_ID",
	"PYTEST_*", "VIRTUAL_ENV", "VIRTUAL_ENV_PROMPT", "CONDA_*", "PYENV*", "UV", "UV_*", "PIP_*", "GOPATH", "GOROOT",
	"GOCACHE", "GOMODCACHE", "GOTMPDIR", "GOENV", "_PYTHON_*",
	// Set by smates itself to trace tests.
	"SMATES_*", "NODE_OPTIONS", "NODE_V8_COVERAGE", "BUN_OPTIONS", "PYTHONPATH", "PYTHONDONTWRITEBYTECODE", "GOFLAGS",
}

// EnvIgnored reports whether reads of the environment variable name are left out of
// the dependencies of the tests of suite s (nil: no suite): name matches env_ignore
// (top level or the suite's), or DefaultEnvIgnore unless env_track lists it.
func (c *Config) EnvIgnored(s *Suite, name string) bool {
	var ignore, track []string
	if c != nil {
		ignore, track = c.EnvIgnore, c.EnvTrack
	}
	if s != nil {
		ignore, track = append(append([]string{}, ignore...), s.EnvIgnore...), append(append([]string{}, track...), s.EnvTrack...)
	}
	return matchEnv(ignore, name) || matchEnv(DefaultEnvIgnore, name) && !matchEnv(track, name)
}

func matchEnv(patterns []string, name string) bool {
	for _, p := range patterns {
		if ok, _ := path.Match(p, name); ok {
			return true
		}
	}
	return false
}

func checkEnvPatterns(where string, lists ...[]string) error {
	for _, l := range lists {
		for _, p := range l {
			if _, err := path.Match(p, ""); err != nil || p == "" {
				return fmt.Errorf("%s: bad environment variable pattern %q", where, p)
			}
		}
	}
	return nil
}

// Load reads the config at the repo root. It returns (nil, nil) when there is none.
func Load(root string) (*Config, error) {
	for _, n := range FileNames {
		p := filepath.Join(root, n)
		data, err := os.ReadFile(p)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		c, err := Parse(data)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", n, err)
		}
		c.Path = n
		return c, nil
	}
	return nil, nil
}

// Parse decodes and validates a config document.
func Parse(data []byte) (*Config, error) {
	var c Config
	dec := yaml.NewDecoder(strings.NewReader(string(data)))
	dec.KnownFields(true)
	if err := dec.Decode(&c); err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	if err := c.Normalize(); err != nil {
		return nil, err
	}
	return &c, nil
}

// Orphan policies (Config.Orphans).
const (
	OrphansAgent  = "agent"  // ask the agent, fall back to all
	OrphansAll    = "all"    // run the suites the orphans may affect
	OrphansIgnore = "ignore" // do nothing
)

// Untested code policies (Config.Untested).
const (
	UntestedSkip   = "skip"
	UntestedOrphan = "orphan"
)

// Agent modes (AgentConfig.Enabled).
const (
	AgentAuto   = "auto"
	AgentAlways = "always"
	AgentNever  = "never"
)

// Normalize fills defaults and validates.
func (c *Config) Normalize() error {
	if c.Version == 0 {
		c.Version = 1
	}
	if c.Version != 1 {
		return fmt.Errorf("unsupported config version %d", c.Version)
	}
	if c.Orphans == "" {
		c.Orphans = OrphansAgent
	}
	switch c.Orphans {
	case OrphansAgent, OrphansAll, OrphansIgnore:
	default:
		return fmt.Errorf("orphans must be agent, all or ignore (got %q)", c.Orphans)
	}
	if c.Untested == "" {
		c.Untested = UntestedSkip
	}
	switch c.Untested {
	case UntestedSkip, UntestedOrphan:
	default:
		return fmt.Errorf("untested must be skip or orphan (got %q)", c.Untested)
	}
	if c.State == "" {
		c.State = ".smates/state.json.gz"
	}
	a := &c.Agent
	if a.Enabled == "" {
		a.Enabled = AgentAuto
	}
	switch a.Enabled {
	case AgentAuto, AgentAlways, AgentNever:
	default:
		return fmt.Errorf("agent.enabled must be auto, always or never (got %q)", a.Enabled)
	}
	if a.Provider == "" {
		a.Provider = "auto"
	}
	switch a.Provider {
	case "auto", "anthropic", "openai", "openrouter":
	default:
		return fmt.Errorf("agent.provider must be auto, anthropic, openai or openrouter (got %q)", a.Provider)
	}
	if a.MaxTurns == 0 {
		a.MaxTurns = 12
	}
	if a.Timeout == 0 {
		a.Timeout = 120 * time.Second
	}
	if a.Effort == "" {
		a.Effort = "low"
	}
	if a.ExplainMax == 0 {
		a.ExplainMax = 3
	}
	if a.ExplainMax < 0 {
		return fmt.Errorf("agent.explain_max must not be negative (got %d)", a.ExplainMax)
	}
	if c.Flaky.Retry < 0 || c.Flaky.Retry > 5 {
		return fmt.Errorf("flaky.retry must be between 0 and 5 (got %d)", c.Flaky.Retry)
	}
	if err := checkEnvPatterns("env_ignore/env_track", c.EnvIgnore, c.EnvTrack); err != nil {
		return err
	}
	names := map[string]bool{}
	for i := range c.Suites {
		s := &c.Suites[i]
		if s.Root == "" {
			s.Root = "."
		}
		s.Root = filepath.ToSlash(filepath.Clean(s.Root))
		if strings.HasPrefix(s.Root, "..") || filepath.IsAbs(s.Root) {
			return fmt.Errorf("suite %q: root must be inside the repository", s.Name)
		}
		if s.Runner == "" {
			return fmt.Errorf("suite %q: runner is required", s.Name)
		}
		if s.Name == "" {
			s.Name = s.Runner
			if s.Root != "." {
				s.Name = s.Root
			}
		}
		if names[s.Name] {
			return fmt.Errorf("duplicate suite name %q", s.Name)
		}
		names[s.Name] = true
		if s.Runner == "custom" && s.Command == "" {
			return fmt.Errorf("suite %q: custom runner needs a command", s.Name)
		}
		if err := checkEnvPatterns(fmt.Sprintf("suite %q", s.Name), s.EnvIgnore, s.EnvTrack); err != nil {
			return err
		}
		switch s.Granularity {
		case "", GranularityPackage:
		case GranularityFunc, "function":
			if s.Runner != "go" {
				return fmt.Errorf("suite %q: granularity %q is only supported by the go runner", s.Name, s.Granularity)
			}
			s.Granularity = GranularityFunc
		default:
			return fmt.Errorf("suite %q: granularity must be package or func (got %q)", s.Name, s.Granularity)
		}
		if s.RecordJobs < 0 {
			return fmt.Errorf("suite %q: record_jobs must not be negative (got %d)", s.Name, s.RecordJobs)
		}
		if s.MaxAge < 0 {
			return fmt.Errorf("suite %q: max_age must not be negative (got %s)", s.Name, s.MaxAge)
		}
		trimDotSlash(s.Volatile)
		if err := checkGlobs(fmt.Sprintf("suite %q: volatile", s.Name), s.Volatile); err != nil {
			return err
		}
		trimDotSlash(s.Untraced)
		if err := checkGlobs(fmt.Sprintf("suite %q: untraced", s.Name), s.Untraced); err != nil {
			return err
		}
		if err := checkRules(s); err != nil {
			return err
		}
		if err := checkRequires(s); err != nil {
			return err
		}
		if Bool(s.TrustBrowserPasses, false) && s.Runner != "vitest" {
			return fmt.Errorf("suite %q: trust_browser_passes is only supported by the vitest runner", s.Name)
		}
	}
	for i := range c.Suites {
		if err := checkDependencies(&c.Suites[i], names); err != nil {
			return err
		}
	}
	return nil
}

// Go unit granularities (Suite.Granularity).
const (
	GranularityPackage = "package" // one unit per package directory
	GranularityFunc    = "func"    // one unit per top-level test function
)

// Bool returns *b or def.
func Bool(b *bool, def bool) bool {
	if b == nil {
		return def
	}
	return *b
}

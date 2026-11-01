// Package config loads smates.yml.
package config

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"slices"
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
	// one process per file (bun always; jest/vitest with isolate). Default: 1 for bun
	// (bun test runs files one after another), the CPU count otherwise.
	RecordJobs int `yaml:"record_jobs"`
	// Static enables import-graph analysis (default true).
	Static *bool `yaml:"static"`
	// Junit: optional path (relative to Root) of a JUnit XML report the command writes;
	// used for per-file pass/fail when the runner cannot report it otherwise.
	Junit string `yaml:"junit"`
	// PythonRoots: extra import roots for Python static analysis (relative to Root).
	PythonRoots []string `yaml:"python_roots"`
	// EnvIgnore and EnvTrack add to the top-level lists for this suite only.
	EnvIgnore []string `yaml:"env_ignore"`
	EnvTrack  []string `yaml:"env_track"`
}

// Rule declares dependencies tracing cannot see.
type Rule struct {
	// Tests: globs relative to the suite root; Deps: repo-relative globs.
	Tests []string `yaml:"tests"`
	Deps  []string `yaml:"deps"`
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
	if n.Kind == yaml.ScalarNode {
		return n.Decode(&d.Suite)
	}
	// Decoding through a custom unmarshaler loses the decoder's KnownFields check.
	if n.Kind == yaml.MappingNode {
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
	}
	for i := range c.Suites {
		if err := checkDependencies(&c.Suites[i], names); err != nil {
			return err
		}
	}
	return nil
}

// Bool returns *b or def.
func Bool(b *bool, def bool) bool {
	if b == nil {
		return def
	}
	return *b
}

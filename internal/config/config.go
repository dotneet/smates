// Package config loads smates.yml.
package config

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

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
	Orphans string      `yaml:"orphans"`
	Agent   AgentConfig `yaml:"agent"`
	// State: path of the state file, relative to the repository root.
	State string `yaml:"state"`
	// FullRunBranches: branches on which `smates run` runs everything (default: the default branch).
	FullRunBranches []string `yaml:"full_run_branches"`

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
	Inputs []string          `yaml:"inputs"`
	Rules  []Rule            `yaml:"rules"`
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
}

// Rule declares dependencies tracing cannot see.
type Rule struct {
	// Tests: globs relative to the suite root; Deps: repo-relative globs.
	Tests []string `yaml:"tests"`
	Deps  []string `yaml:"deps"`
}

// AgentConfig configures the LLM agent used for orphan files.
type AgentConfig struct {
	// Enabled: "auto" (when credentials are present), "always" or "never".
	Enabled  string        `yaml:"enabled"`
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

// DefaultIgnore are changes that never need tests unless some test reads them.
var DefaultIgnore = []string{
	"**/*.md", "**/*.mdx", "**/*.rst", "**/*.txt.license", "**/LICENSE*", "**/CHANGELOG*", "**/CODEOWNERS",
	"docs/**", "doc/**", ".github/ISSUE_TEMPLATE/**", ".github/PULL_REQUEST_TEMPLATE*", ".github/dependabot.yml",
	".vscode/**", ".idea/**", ".editorconfig", "**/.gitignore", "**/.gitattributes", ".devcontainer/**",
	"**/.prettierrc*", "**/.prettierignore", "**/.eslintrc*", "**/eslint.config.*", "**/.markdownlint*",
	"**/biome.json", "**/biome.jsonc", "**/.golangci.y*ml", "**/ruff.toml", "**/.pre-commit-config.yaml",
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

// Normalize fills defaults and validates.
func (c *Config) Normalize() error {
	if c.Version == 0 {
		c.Version = 1
	}
	if c.Version != 1 {
		return fmt.Errorf("unsupported config version %d", c.Version)
	}
	if c.Orphans == "" {
		c.Orphans = "agent"
	}
	switch c.Orphans {
	case "agent", "all", "ignore":
	default:
		return fmt.Errorf("orphans must be agent, all or ignore (got %q)", c.Orphans)
	}
	if c.State == "" {
		c.State = ".smates/state.json.gz"
	}
	a := &c.Agent
	if a.Enabled == "" {
		a.Enabled = "auto"
	}
	switch a.Enabled {
	case "auto", "always", "never":
	default:
		return fmt.Errorf("agent.enabled must be auto, always or never (got %q)", a.Enabled)
	}
	if a.Model == "" {
		a.Model = "claude-haiku-5-5"
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

package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParseDefaults(t *testing.T) {
	for _, doc := range []string{"", "version: 1\n", "# only a comment\n"} {
		c, err := Parse([]byte(doc))
		if err != nil {
			t.Fatalf("%q: %v", doc, err)
		}
		if c.Version != 1 || c.Orphans != "agent" || c.State != ".smates/state.json.gz" {
			t.Errorf("%q: defaults %+v", doc, c)
		}
		a := c.Agent
		if a.Enabled != "auto" || a.Provider != "auto" || a.Model != "" || a.MaxTurns != 12 || a.Timeout != 120*time.Second || a.Effort != "low" {
			t.Errorf("%q: agent defaults %+v", doc, a)
		}
	}
}

func TestParseFull(t *testing.T) {
	c, err := Parse([]byte(`
version: 1
orphans: all
state: tmp/state.json
ignore: ["*.png"]
inputs: [".tool-versions"]
full_run_branches: [main, release]
flaky:
  retry: 2
agent:
  enabled: never
  provider: openrouter
  model: m
  max_turns: 3
  timeout: 5s
  command: claude -p
  effort: high
  instructions: hi
suites:
  - runner: go
  - runner: vitest
    root: ./web/
    tests: ["**/*.test.ts"]
    exclude: ["e2e/**"]
    command: pnpm vitest run
    inputs: ["/shared/**"]
    rules:
      - tests: ["db/**"]
        deps: ["migrations/**"]
    env: {A: "1"}
    isolate: true
    static: false
    junit: out.xml
    python_roots: [src]
  - name: custom-one
    runner: custom
    command: make test
`))
	if err != nil {
		t.Fatal(err)
	}
	if c.Orphans != "all" || c.State != "tmp/state.json" || c.Flaky.Retry != 2 || c.Agent.Timeout != 5*time.Second || c.Agent.MaxTurns != 3 || c.Agent.Enabled != "never" || c.Agent.Provider != "openrouter" || c.Agent.Model != "m" {
		t.Errorf("top-level: %+v", c)
	}
	if len(c.Suites) != 3 {
		t.Fatalf("suites: %+v", c.Suites)
	}
	if s := c.Suites[0]; s.Name != "go" || s.Root != "." {
		t.Errorf("default name/root: %+v", s)
	}
	s := c.Suites[1]
	if s.Root != "web" || s.Name != "web" {
		t.Errorf("root cleaned and used as name: %+v", s)
	}
	if !Bool(s.Isolate, false) || Bool(s.Static, true) || s.Env["A"] != "1" || len(s.Rules) != 1 || s.Rules[0].Deps[0] != "migrations/**" {
		t.Errorf("suite fields: %+v", s)
	}
	if c.Suites[2].Name != "custom-one" {
		t.Errorf("explicit name: %+v", c.Suites[2])
	}
}

func TestParseErrors(t *testing.T) {
	cases := []struct{ name, doc, want string }{
		{"unknown top-level field", "suites: []\nfoo: 1\n", "field foo not found"},
		{"unknown suite field", "suites:\n  - runner: go\n    command_line: x\n", "field command_line not found"},
		{"unknown agent field", "agent:\n  modle: x\n", "field modle not found"},
		{"bad version", "version: 2\n", "unsupported config version 2"},
		{"bad orphans", "orphans: some\n", "orphans must be"},
		{"bad agent enabled", "agent:\n  enabled: yes\n", "agent.enabled"},
		{"bad agent provider", "agent:\n  provider: gemini\n", "agent.provider"},
		{"missing runner", "suites:\n  - name: x\n", "runner is required"},
		{"duplicate names", "suites:\n  - runner: go\n  - runner: go\n", "duplicate suite name"},
		{"duplicate explicit names", "suites:\n  - {name: a, runner: go}\n  - {name: a, runner: pytest}\n", "duplicate suite name"},
		{"root outside", "suites:\n  - runner: go\n    root: ../x\n", "inside the repository"},
		{"root escapes after clean", "suites:\n  - runner: go\n    root: a/../../x\n", "inside the repository"},
		{"absolute root", "suites:\n  - runner: go\n    root: /abs\n", "inside the repository"},
		{"custom needs command", "suites:\n  - runner: custom\n", "needs a command"},
		{"bad timeout", "agent:\n  timeout: soon\n", ""},
		{"negative flaky retry", "flaky:\n  retry: -1\n", "flaky.retry"},
		{"too many flaky retries", "flaky:\n  retry: 9\n", "flaky.retry"},
		{"unknown flaky field", "flaky:\n  retries: 1\n", "field retries not found"},
		{"wrong type", "suites: 3\n", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := Parse([]byte(c.doc))
			if err == nil {
				t.Fatalf("no error")
			}
			if c.want != "" && !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err %q, want %q", err, c.want)
			}
		})
	}
}

func TestRootCleaning(t *testing.T) {
	cases := map[string]string{"": ".", ".": ".", "./": ".", "a/b/": "a/b", "./a//b": "a/b", "a/../b": "b"}
	for in, want := range cases {
		c, err := Parse([]byte("suites:\n  - runner: go\n    root: \"" + in + "\"\n"))
		if err != nil {
			t.Fatalf("%q: %v", in, err)
		}
		if got := c.Suites[0].Root; got != want {
			t.Errorf("root %q -> %q, want %q", in, got, want)
		}
	}
}

func TestLoad(t *testing.T) {
	dir := t.TempDir()
	c, err := Load(dir)
	if c != nil || err != nil {
		t.Fatalf("no config: %v %v", c, err)
	}
	os.WriteFile(filepath.Join(dir, ".smates.yaml"), []byte("orphans: ignore\n"), 0o644)
	c, err = Load(dir)
	if err != nil || c.Path != ".smates.yaml" || c.Orphans != "ignore" {
		t.Fatalf("got %+v %v", c, err)
	}
	// smates.yml wins over later names.
	os.WriteFile(filepath.Join(dir, "smates.yml"), []byte("orphans: all\n"), 0o644)
	c, err = Load(dir)
	if err != nil || c.Path != "smates.yml" || c.Orphans != "all" {
		t.Fatalf("got %+v %v", c, err)
	}
	os.WriteFile(filepath.Join(dir, "smates.yml"), []byte("bogus: 1\n"), 0o644)
	if _, err := Load(dir); err == nil || !strings.HasPrefix(err.Error(), "smates.yml:") {
		t.Fatalf("error should name the file: %v", err)
	}
}

func TestBool(t *testing.T) {
	tr, fa := true, false
	if !Bool(nil, true) || Bool(nil, false) || !Bool(&tr, false) || Bool(&fa, true) {
		t.Fatal("Bool")
	}
}

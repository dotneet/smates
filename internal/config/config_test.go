package config

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/dotneet/smates/internal/testutil"
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
		{"bad env pattern", "env_ignore: [\"A[\"]\n", "bad environment variable pattern"},
		{"empty suite env pattern", "suites:\n  - runner: go\n    env_track: [\"\"]\n", "bad environment variable pattern"},
		{"rule without tests or reaches", "suites:\n  - runner: go\n    rules: [{deps: [m/**]}]\n", "needs tests or reaches"},
		{"rule with empty lists", "suites:\n  - runner: go\n    rules: [{tests: [], reaches: [], deps: [m/**]}]\n", "needs tests or reaches"},
		{"rule without deps", "suites:\n  - runner: go\n    rules: [{tests: [\"**\"]}]\n", "needs deps"},
		{"rule absolute deps", "suites:\n  - runner: go\n    rules: [{tests: [\"**\"], deps: [/m/**]}]\n", "rules[0]: deps"},
		{"rule parent reaches", "suites:\n  - runner: go\n    rules: [{reaches: [../kit/**], deps: [m/**]}]\n", "rules[0]: reaches"},
		{"rule bad reaches glob", "suites:\n  - runner: go\n    rules: [{reaches: [\"kit/[\"], deps: [m/**]}]\n", "bad glob"},
		{"rule empty variable", "suites:\n  - runner: go\n    rules: [{reaches: [\"$\"], deps: [m/**]}]\n", "bad environment variable pattern"},
		{"rule bad variable pattern", "suites:\n  - runner: go\n    rules: [{reaches: [\"$DB_[\"], deps: [m/**]}]\n", "bad environment variable pattern"},
		{"rule bad tests glob", "suites:\n  - runner: go\n    rules: [{tests: [\"a\\\\b\"], deps: [m/**]}]\n", "path separator"},
		{"unknown rule field", "suites:\n  - runner: go\n    rules: [{test: [\"**\"], deps: [m/**]}]\n", "field test not found"},
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

func TestEnvIgnored(t *testing.T) {
	c, err := Parse([]byte("env_ignore: [\"BUILD_*\", DEPLOY_ID]\nenv_track: [LANG]\nsuites:\n  - name: web\n    runner: vitest\n    env_ignore: [VITE_DEBUG]\n    env_track: [\"LC_*\", CI]\n  - name: api\n    runner: go\n"))
	if err != nil {
		t.Fatal(err)
	}
	web, api := &c.Suites[0], &c.Suites[1]
	for _, tc := range []struct {
		s    *Suite
		name string
		want bool
	}{
		{nil, "TZ", false},
		{nil, "PATH", true},
		{nil, "GITHUB_SHA", true},
		{nil, "SMATES_TRACE_DIR", true},
		{nil, "LANG", false}, // env_track
		{api, "LC_ALL", true},
		{web, "LC_ALL", false}, // the suite's env_track
		{web, "CI", false},
		{api, "CI", true},
		{api, "BUILD_TARGET", true}, // env_ignore
		{api, "DEPLOY_ID", true},
		{web, "VITE_DEBUG", true},
		{api, "VITE_DEBUG", false},
		{nil, "DATABASE_URL", false},
	} {
		if got := c.EnvIgnored(tc.s, tc.name); got != tc.want {
			t.Errorf("EnvIgnored(%v, %s) = %v, want %v", tc.s != nil, tc.name, got, tc.want)
		}
	}
	// env_track does not override an explicit env_ignore.
	c2, _ := Parse([]byte("env_ignore: [\"*\"]\nenv_track: [LANG]\n"))
	if !c2.EnvIgnored(nil, "LANG") || !c2.EnvIgnored(nil, "TZ") {
		t.Error("env_ignore: [*] must turn tracking off")
	}
	var none *Config
	if none.EnvIgnored(nil, "TZ") || !none.EnvIgnored(nil, "HOME") {
		t.Error("nil config uses the defaults")
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

// Suite names derived from roots may hold any character a directory can (a scoped
// package, a comma): the state escapes them where it lists suites (state.MarkerSnap).
func TestDerivedSuiteNames(t *testing.T) {
	c, err := Parse([]byte("suites:\n  - {runner: vitest, root: packages/@scope/pkg}\n  - {runner: go, root: \"a,b\"}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if c.Suites[0].Name != "packages/@scope/pkg" || c.Suites[1].Name != "a,b" {
		t.Errorf("names %q %q", c.Suites[0].Name, c.Suites[1].Name)
	}
}

func TestLoad(t *testing.T) {
	dir := t.TempDir()
	c, err := Load(dir)
	if c != nil || err != nil {
		t.Fatalf("no config: %v %v", c, err)
	}
	testutil.Write(t, dir, ".smates.yaml", "orphans: ignore\n")
	c, err = Load(dir)
	if err != nil || c.Path != ".smates.yaml" || c.Orphans != "ignore" {
		t.Fatalf("got %+v %v", c, err)
	}
	// smates.yml wins over later names.
	testutil.Write(t, dir, "smates.yml", "orphans: all\n")
	c, err = Load(dir)
	if err != nil || c.Path != "smates.yml" || c.Orphans != "all" {
		t.Fatalf("got %+v %v", c, err)
	}
	testutil.Write(t, dir, "smates.yml", "bogus: 1\n")
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

func TestParseRules(t *testing.T) {
	c, err := Parse([]byte(`
suites:
  - runner: go
    rules:
      - reaches: ["./pkg/shared/testkit/**", "$APP_DB_*"]
        deps: ["./database/migrations/**"]
      - tests: ["./pkg/app/**"]
        reaches: ["pkg/shared/testkit/**"]
        deps: ["database/fixtures/**"]
`))
	if err != nil {
		t.Fatal(err)
	}
	s := c.Suites[0]
	r := s.Rules[0]
	if want := []string{"pkg/shared/testkit/**", "$APP_DB_*"}; !reflect.DeepEqual(r.Reaches, want) {
		t.Errorf("reaches %v, want %v", r.Reaches, want)
	}
	if want := []string{"database/migrations/**"}; !reflect.DeepEqual(r.Deps, want) {
		t.Errorf("deps %v, want %v", r.Deps, want)
	}
	files, env := r.ReachesEnv()
	if !reflect.DeepEqual(files, []string{"pkg/shared/testkit/**"}) || !reflect.DeepEqual(env, []string{"APP_DB_*"}) {
		t.Errorf("ReachesEnv: %v %v", files, env)
	}
	if want := []string{"pkg/app/**"}; !reflect.DeepEqual(s.Rules[1].Tests, want) {
		t.Errorf("tests %v, want %v", s.Rules[1].Tests, want)
	}
}

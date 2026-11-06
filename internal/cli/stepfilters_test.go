package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dotneet/smates/internal/config"
	"github.com/dotneet/smates/internal/gitx"
)

// TestStepPatchNarrowing: the smates step must not stand in for a step whose shell
// state it would lose (a cd or export on an earlier line of the run: block), for a
// pytest step that collects only part of a suite, or for a plain step when the suite's
// own command would leave tests out (-short, -run, -skip). Harmless lines and paths
// equal to a suite's root still patch.
func TestStepPatchNarrowing(t *testing.T) {
	suites := []config.Suite{
		{Name: "app", Runner: "go", Root: "pkg/app"},
		{Name: "short", Runner: "go", Root: "pkg/short", Command: "go test -short"},
		{Name: "filtered", Runner: "go", Root: "pkg/filtered", Command: "go test -run TestUnit"},
		{Name: "tagged", Runner: "go", Root: "pkg/tagged", Command: "go test -tags fast"},
		{Name: "envtags", Runner: "go", Root: "pkg/envtags", Env: map[string]string{"GOFLAGS": "-tags=fast"}},
		{Name: "envshort", Runner: "go", Root: "pkg/envshort", Env: map[string]string{"GOFLAGS": "-count=1 -short"}},
		{Name: "envdyn", Runner: "go", Root: "pkg/envdyn", Env: map[string]string{"GOFLAGS": "${EXTRA_GOFLAGS}"}},
		{Name: "envcount", Runner: "go", Root: "pkg/envcount", Env: map[string]string{"GOFLAGS": "-count=1"}},
		{Name: "gotestsum", Runner: "go", Root: "pkg/gotestsum", Command: "gotestsum -- -tags=fast"},
		{Name: "make", Runner: "go", Root: "pkg/make", Command: "make test"},
		{Name: "shc", Runner: "go", Root: "pkg/shc", Command: `sh -c "go test -tags fast"`},
		{Name: "py", Runner: "pytest", Root: "tests"},
		{Name: "svc", Runner: "pytest", Root: "svc"},
	}
	// env holds env: blocks of the workflow, the job and the step (YAML, "" = none).
	step := func(wd, run string, env [3]string) string {
		s := "on: push\n"
		if env[0] != "" {
			s += "env: " + env[0] + "\n"
		}
		s += "jobs:\n  t:\n"
		if env[1] != "" {
			s += "    env: " + env[1] + "\n"
		}
		s += "    steps:\n      - "
		if env[2] != "" {
			s += "env: " + env[2] + "\n        "
		}
		if wd != "" {
			s += "working-directory: " + wd + "\n        "
		}
		return s + "run: |\n" + "          " + strings.ReplaceAll(run, "\n", "\n          ") + "\n"
	}
	cases := []struct {
		name, wd, run string
		why           string    // substring of the refusal ("" = patched)
		names         string    // the suites patched in, when checked
		env           [3]string // env: of the workflow, the job and the step
	}{
		{"cd before the test line", "", "cd pkg/app\ngo test ./...", "changes the directory or environment", "", [3]string{}},
		{"export before the test line", "pkg/app", "export GOFLAGS=-tags=integration\ngo test ./...", "changes the directory or environment", "", [3]string{}},
		{"source before the test line", "svc", "source .venv/bin/activate\npytest", "changes the directory or environment", "", [3]string{}},
		{"dot-source before the test line", "svc", ". .venv/bin/activate\npytest", "changes the directory or environment", "", [3]string{}},
		{"bare assignment before the test line", "pkg/app", "APP_DB_PORT=5433\ngo test ./...", "changes the directory or environment", "", [3]string{}},
		{"cd inside a chain", "", "make deps && cd pkg/app\ngo test ./...", "changes the directory or environment", "", [3]string{}},
		{"harmless command before the test line", "pkg/app", "go mod download\ngo test ./...", "", "", [3]string{}},
		// Shell options for errors and tracing change neither directory nor environment.
		{"set -euo pipefail before the test line", "pkg/app", "set -euo pipefail\ngo test ./...", "", "", [3]string{}},
		{"set -x before the test line", "pkg/app", "set -x\ngo test ./...", "", "", [3]string{}},
		{"set +e before the test line", "pkg/app", "set +e\ngo test ./...", "", "", [3]string{}},
		{"set -o errexit -o pipefail before the test line", "pkg/app", "set -o errexit -o pipefail\ngo test ./...", "", "", [3]string{}},
		{"set -e in a chain before the test line", "pkg/app", "set -e && go mod download\ngo test ./...", "", "", [3]string{}},
		// Must refuse: these change what the commands after them see.
		{"set -a before the test line", "pkg/app", "set -a\ngo test ./...", "changes the directory or environment", "", [3]string{}},
		{"set -o allexport before the test line", "pkg/app", "set -eo allexport\ngo test ./...", "changes the directory or environment", "", [3]string{}},
		{"set -- before the test line", "pkg/app", "set -- -race\ngo test ./...", "changes the directory or environment", "", [3]string{}},
		{"set positional parameters before the test line", "pkg/app", "set a b\ngo test ./...", "changes the directory or environment", "", [3]string{}},
		{"set with an unknown option before the test line", "pkg/app", "set -k\ngo test ./...", "changes the directory or environment", "", [3]string{}},
		{"assignment for one command only", "pkg/app", "CGO_ENABLED=0 go build ./...\ngo test ./...", "", "", [3]string{}},
		{"pytest path equal to the suite root", "", "pytest tests", "", "py", [3]string{}},
		{"pytest path inside a suite", "", "pytest tests/unit", "it runs only the tests under tests/unit, inside suite", "", [3]string{}},
		{"pytest path of one suite of two", "", "pytest svc", "", "svc", [3]string{}},
		{"pytest . from the suite's directory", "svc", "pytest .", "", "", [3]string{}},
		{"suite command with -short", "pkg/short", "go test ./...", "passes -short", "", [3]string{}},
		{"suite command with -run", "pkg/filtered", "go test ./...", "passes -run", "", [3]string{}},
		{"plain suite command", "pkg/app", "go test ./...", "", "", [3]string{}},
		// Must refuse: the suite's -tags may leave out files the step compiles
		// (//go:build !fast).
		{"suite command with -tags, plain step", "pkg/tagged", "go test ./...", "passes -tags fast", "", [3]string{}},
		{"suite command with -tags, -count and -v step", "pkg/tagged", "go test -count=1 -v ./...", "passes -tags fast", "", [3]string{}},
		{"suite command with -tags, inline GOFLAGS step", "pkg/tagged", "GOFLAGS=-count=1 go test ./...", "passes -tags fast", "", [3]string{}},
		{"suite command with -tags, step with other tags", "pkg/tagged", "go test -tags slow ./...", "-tags slow, which the command", "", [3]string{}},
		{"suite command with -tags, step with the same tags", "pkg/tagged", "go test -tags fast ./...", "", "", [3]string{}},
		// The suite's env: GOFLAGS applies to its go test as its command does. Must
		// refuse what leaves out tests the step runs, or what smates cannot read.
		{"suite env GOFLAGS with -tags, plain step", "pkg/envtags", "go test ./...", "env GOFLAGS=-tags=fast) passes -tags fast", "", [3]string{}},
		{"suite env GOFLAGS with -tags, step with the same tags", "pkg/envtags", "go test -tags fast ./...", "", "", [3]string{}},
		{"suite env GOFLAGS with -short", "pkg/envshort", "go test ./...", "whose -short would make", "", [3]string{}},
		{"suite env GOFLAGS smates cannot read", "pkg/envdyn", "go test ./...", "whose value smates cannot read", "", [3]string{}},
		{"suite env GOFLAGS with -count only", "pkg/envcount", "go test ./...", "", "", [3]string{}},
		// Must refuse: a suite command that is not go test itself may pass flags
		// smates cannot see.
		{"suite command through gotestsum", "pkg/gotestsum", "go test ./...", "is not go test itself", "", [3]string{}},
		{"suite command through make", "pkg/make", "go test ./...", "is not go test itself", "", [3]string{}},
		{"suite command through sh -c", "pkg/shc", "go test ./...", "is not go test itself", "", [3]string{}},
		// An inline GOFLAGS replaces the env's for the step, but the smates step keeps
		// the env: and runs with its GOFLAGS. Must refuse unless both carry the same.
		{"inline GOFLAGS over job env GOFLAGS -tags", "pkg/app", "GOFLAGS=-count=1 go test ./...", "over GOFLAGS=-tags=integration of the env:", "", [3]string{"", "{GOFLAGS: -tags=integration}", ""}},
		{"inline GOFLAGS over step env GOFLAGS -tags", "pkg/app", "GOFLAGS=-count=1 go test ./...", "over GOFLAGS=-tags=integration of the env:", "", [3]string{"", "", "{GOFLAGS: -tags=integration}"}},
		{"inline GOFLAGS over workflow env GOFLAGS -tags", "pkg/app", "GOFLAGS=-count=1 go test ./...", "over GOFLAGS=-tags=integration of the env:", "", [3]string{"{GOFLAGS: -tags=integration}", "", ""}},
		{"inline GOFLAGS over env GOFLAGS -race", "pkg/app", "GOFLAGS=-count=1 go test ./...", "over GOFLAGS=-race of the env:", "", [3]string{"", "{GOFLAGS: -race}", ""}},
		{"inline GOFLAGS over env GOFLAGS -short", "pkg/app", "GOFLAGS=-count=1 go test ./...", "whose -short the smates step", "", [3]string{"", "{GOFLAGS: -short}", ""}},
		{"inline GOFLAGS over env GOFLAGS from an expression", "pkg/app", "GOFLAGS=-count=1 go test ./...", "smates cannot read", "", [3]string{"", "{GOFLAGS: '${{ vars.GOFLAGS }}'}", ""}},
		{"inline GOFLAGS under an env: expression", "pkg/app", "GOFLAGS=-count=1 go test ./...", "is an expression that may set GOFLAGS", "", [3]string{"", "${{ fromJSON(vars.CI_ENV) }}", ""}},
		{"inline GOFLAGS equal to env GOFLAGS", "pkg/app", "GOFLAGS='-count=1 -v' go test ./...", "", "", [3]string{"", "{GOFLAGS: -count=1}", ""}},
		{"inline GOFLAGS with env without GOFLAGS", "pkg/app", "GOFLAGS=-count=1 go test ./...", "", "", [3]string{"", "{TZ: UTC}", ""}},
		// Must refuse: the command line turns off a flag of the env's GOFLAGS that the
		// suite's command cannot turn off.
		{"-race=false over env GOFLAGS -race", "pkg/app", "go test -race=false ./...", "turns -race off", "", [3]string{"", "{GOFLAGS: -race}", ""}},
		{"-tags= over env GOFLAGS -tags", "pkg/app", "go test -tags= ./...", "clears the -tags of GOFLAGS", "", [3]string{"", "{GOFLAGS: -tags=integration}", ""}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			steps := parseWorkflowSteps("ci.yml", []byte(step(c.wd, c.run, c.env)))
			if len(steps) != 1 {
				t.Fatalf("steps %+v", steps)
			}
			names, why := stepPatch(steps[0], suites)
			if (c.why == "") != (why == "") || !strings.Contains(why, c.why) {
				t.Errorf("why %q, want %q", why, c.why)
			}
			if c.names != "" && strings.Join(names, " ") != c.names {
				t.Errorf("suites %v, want %s", names, c.names)
			}
		})
	}
}

// TestInitMakefileMessage: init says which Makefile flags the suite's command keeps,
// never that flags it wrote into command: are not applied.
func TestInitMakefileMessage(t *testing.T) {
	clearCI(t)
	noAgentCredentials(t)
	cases := []struct {
		name, makefile string
		want, not      string
	}{
		{"carried", "test:\n\tgo test -race -v ./...\n", "command in smates.yml keeps -race", "so those flags/variables are not applied"},
		{"nothing carried", "test:\n\tTZ=UTC go test -v ./...\n", "so those flags/variables are not applied", "keeps"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			g := newGitRepo(t)
			writeTree(t, g.dir, map[string]string{"go.mod": "module x\n\ngo 1.22\n", "x_test.go": "package x\n", "Makefile": c.makefile})
			g.git("add", "-A")
			g.commit("fixture")
			code, _, errs := smatesCmd(t, g.dir, "init", "--stdout")
			if code != 0 || !strings.Contains(errs, c.want) || strings.Contains(errs, c.not) {
				t.Errorf("code %d stderr:\n%s\nwant %q, not %q", code, errs, c.want, c.not)
			}
		})
	}
}

// TestInitGitHubExistingSmatesWorkflow: a workflow already running smates (on pull
// requests only) gets the seed patch, and no second workflow running the same tests.
func TestInitGitHubExistingSmatesWorkflow(t *testing.T) {
	clearCI(t)
	noAgentCredentials(t)
	cases := []struct {
		name    string
		args    []string
		written bool
	}{
		{"seed patch only", nil, false},
		{"forced", []string{"--force"}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			g := newGitRepo(t)
			writeTree(t, g.dir, map[string]string{
				".github/workflows/ci.yml": "on: pull_request\njobs:\n  test:\n    runs-on: ubuntu-latest\n    steps:\n      - uses: actions/checkout@v5\n      - uses: dotneet/smates@v1\n",
				"go.mod":                   "module example.com/x\n\ngo 1.22\n",
				"x_test.go":                "package x\n",
				"smates.yml":               "version: 1\nsuites:\n  - name: go\n    runner: go\n",
			})
			g.git("add", ".")
			g.commit("files")
			var out, errb bytes.Buffer
			args := append([]string{"init", "--github", "-C", g.dir}, c.args...)
			if code := Main(args, &out, &errb); code != 0 {
				t.Fatalf("init: %d %s", code, errb.String())
			}
			_, err := os.Stat(filepath.Join(g.dir, ".github/workflows/smates.yml"))
			if (err == nil) != c.written {
				t.Errorf("written %t, want %t\n%s", err == nil, c.written, errb.String())
			}
			if !strings.Contains(errb.String(), "+  schedule:") {
				t.Errorf("no seed patch:\n%s", errb.String())
			}
			if !c.written && !strings.Contains(errb.String(), "ci.yml already runs smates") {
				t.Errorf("no reason given:\n%s", errb.String())
			}
		})
	}
}

// TestDefaultBranchFromCI: in a CI checkout without origin/HEAD (nor main or master),
// the CI's default branch is the one the push trigger and the seed check name.
func TestDefaultBranchFromCI(t *testing.T) {
	clearCI(t)
	g := newGitRepo(t)
	g.git("checkout", "-q", "-b", "develop")
	writeTree(t, g.dir, map[string]string{"x": "x\n"})
	g.git("add", ".")
	g.commit("x")
	g.git("branch", "-q", "-D", "main")
	repo, err := gitx.Open(g.dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := defaultBranch(repo); got != "main" {
		t.Errorf("outside CI: %q (git's fallback), want main", got)
	}
	ev := filepath.Join(t.TempDir(), "event.json")
	os.WriteFile(ev, []byte(`{"repository":{"default_branch":"develop"}}`), 0o644)
	t.Setenv("GITHUB_ACTIONS", "true")
	t.Setenv("GITHUB_EVENT_PATH", ev)
	if got := defaultBranch(repo); got != "develop" {
		t.Errorf("in CI: %q, want develop", got)
	}
}

// TestDoctorWorkflowWithoutConfig: without smates.yml, a step whose go test flags init
// would carry into the suite's command gets the init fix, not advice to edit a
// smates.yml that does not exist; with one lacking the flags, the advice stays.
func TestDoctorWorkflowWithoutConfig(t *testing.T) {
	clearCI(t)
	noAgentCredentials(t)
	cases := []struct {
		name, config string
		fix          string // the fix command ("" = none)
		message      string
	}{
		{"no smates.yml", "", "smates init --github --patch smates.patch", "`smates init --github --patch smates.patch`"},
		{"smates.yml lacking the flag", "version: 1\nsuites:\n  - name: go\n    runner: go\n    root: .\n", "", "set `command: go test -race` for the suite"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			g := newGitRepo(t)
			files := map[string]string{
				".github/workflows/ci.yml": "on: pull_request\njobs:\n  t:\n    runs-on: ubuntu-latest\n    steps:\n      - run: go test -race ./...\n",
				"go.mod":                   "module example.com/x\n\ngo 1.22\n",
				"x_test.go":                "package x\n",
			}
			if c.config != "" {
				files["smates.yml"] = c.config
			}
			writeTree(t, g.dir, files)
			g.git("add", ".")
			g.commit("files")
			_, out, _ := smatesCmd(t, g.dir, "doctor", "--format", "json")
			rep := decodeJSON[doctorReport](t, out)
			found := false
			for _, ch := range rep.Checks {
				if ch.ID != "workflow-test-step" {
					continue
				}
				found = true
				got := ""
				if ch.Fix != nil {
					got = ch.Fix.Command
				}
				if got != c.fix || !strings.Contains(ch.Message, c.message) {
					t.Errorf("fix %q message %q, want %q %q", got, ch.Message, c.fix, c.message)
				}
			}
			if !found {
				t.Errorf("no workflow-test-step check:\n%s", out)
			}
		})
	}
}

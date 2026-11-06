package cli

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/dotneet/smates/internal/config"
	"gopkg.in/yaml.v3"
)

func TestShellWords(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"go test ./...", []string{"go", "test", "./..."}},
		{"go test -p 1 $(go list ./... | grep -v /e2e/)", []string{"go", "test", "-p", "1", "$(go list ./... | grep -v /e2e/)"}},
		{"go test `go list ./...` && echo ok", []string{"go", "test", "`go list ./...`", "&&", "echo", "ok"}},
		{`go test -run '^$' "./a b" ${PKGS}`, []string{"go", "test", "-run", `'^$'`, `"./a b"`, "${PKGS}"}},
		{`go test "$(echo "a b")" x`, []string{"go", "test", `"$(echo "a b")"`, "x"}},
		{"go test ./... 2>&1 | tee out.txt", []string{"go", "test", "./...", "2>&1", "|", "tee", "out.txt"}},
		{"go test ./... >out.txt; echo $?", []string{"go", "test", "./...", ">", "out.txt", ";", "echo", "$?"}},
		{"a || b & c", []string{"a", "||", "b", "&", "c"}},
		{"go test ./... # all of them", []string{"go", "test", "./..."}},
		{"go test $((1+2)) x", []string{"go", "test", "$((1+2))", "x"}},
		{`go test \"x\" y`, []string{"go", "test", `\"x\"`, "y"}},
	}
	for _, c := range cases {
		if got := shellWords(c.in); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%q: %q, want %q", c.in, got, c.want)
		}
	}
	lits := []struct {
		in   string
		want string
		ok   bool
	}{
		{"./...", "./...", true},
		{`'^$'`, "^$", true},
		{`"^Test$"`, "^Test$", true},
		{`"a b"`, "a b", true},
		{"$PKGS", "", false},
		{`"$PKGS"`, "", false},
		{"$(go list ./...)", "", false},
		{"`go list`", "", false},
		{"./pkg/*", "", false},
		{"'", "", false},
		{`"`, "", false},
	}
	for _, c := range lits {
		if got, ok := shellLiteral(c.in); got != c.want || ok != c.ok {
			t.Errorf("literal %q: %q %t, want %q %t", c.in, got, ok, c.want, c.ok)
		}
	}
}

// TestCheckTestCmd covers each kind of step the smates action must not replace, and
// the plain unit runs it can.
func TestCheckTestCmd(t *testing.T) {
	head := "on: push\njobs:\n  test:\n    steps:\n"
	cases := []struct {
		run   string
		skip  string // substring of the step's Skip ("" = patchable)
		other bool
		carry string
	}{
		// Plain unit runs.
		{run: "go test ./..."},
		{run: "go test -count=1 -v ./..."},
		{run: "go test ./... 2>&1"},
		// Inline assignments apply to the test command only: GOFLAGS is read as go test
		// flags (carried or refused), anything else must move to the step's env:.
		{run: "GOFLAGS=-tags=integration go test ./...", carry: "-tags integration"},
		{run: "GOFLAGS='-p=1 -race' go test ./...", carry: "-p 1 -race"},
		{run: "GOFLAGS=-p=4 go test -p 1 ./...", carry: "-p 1"},
		{run: "env GOFLAGS=-tags=db go test ./...", carry: "-tags db"},
		{run: "GOFLAGS=-mod=mod go test ./...", skip: "move GOFLAGS to the step's env:"},
		{run: "GOFLAGS=-run=TestA go test ./...", skip: "it passes -run", other: true},
		{run: "GOFLAGS=-short go test ./...", skip: "it passes -short"},
		{run: "GOFLAGS=$FLAGS go test ./...", skip: "its GOFLAGS value comes from the environment"},
		{run: "DB_PORT=3307 go test ./...", skip: "it sets DB_PORT for the test command"},
		{run: "env X=1 go test ./...", skip: "it sets X for the test command"},
		{run: "env -i go test ./...", skip: "through env -i"},
		{run: "CI=true pnpm test", skip: "it sets CI for the test command"},
		{run: "go test -short ./...", skip: "it passes -short"},
		{run: "go test -p 1 -tags integration,db -race -timeout 20m -count=1 ./...", carry: "-p 1 -tags db,integration -race -timeout 20m"},
		{run: "go test -p=2 -tags='a b' .", carry: "-p 2 -tags a,b"},
		{run: "npx vitest run --reporter dot"},
		{run: "pnpm test -- --coverage"},
		{run: "pytest -x"},
		// Packages other than ./... and .
		{run: "go test ./internal/x", skip: "it tests only ./internal/x"},
		{run: "go test ./common/... -count=1", skip: "it tests only ./common/..."},
		{run: "go test -p 1 $(go list ./... | grep -v /e2e/)", skip: "the packages come from $(go list ./... | grep -v /e2e/) at run time", carry: "-p 1"},
		{run: `go test -json -p 1 "${packages[@]}"`, skip: "its -json output feeds another tool", carry: "-p 1"},
		// Tests filtered or not run.
		{run: "go test -run '^$' ./...", skip: "it passes -run", other: true},
		{run: "go test -run TestDate -count=1 ./...", skip: "it passes -run", other: true},
		{run: "go test -race -run=TestX ./...", skip: "it passes -run", other: true},
		{run: "go test -list . ./...", skip: "it passes -list", other: true},
		{run: "go test -c -o bin/x.test ./x", skip: "it passes -c", other: true},
		{run: "go test -bench . ./...", skip: "it passes -bench", other: true},
		{run: "go test -skip TestSlow ./...", skip: "it passes -skip"},
		// Output another step reads.
		{run: "go test -json ./...", skip: "its -json output feeds another tool"},
		{run: "go test -coverprofile=cover.out ./...", skip: "-coverprofile, which writes a file"},
		{run: "go test -v ./... | tee out.txt", skip: "it pipes the test output into another command (tee out.txt)"},
		{run: "go test ./... > out.txt", skip: "writes the test output to out.txt"},
		{run: "go test ./... -args -update", skip: "it passes -args"},
		// Expressions and environment.
		{run: "go test ${{ matrix.pkg }}", skip: "changes with the matrix leg"},
		{run: "go test -tags ${{ inputs.tags }} ./...", skip: "built from a ${{ }} expression"},
		{run: `go test -tags "$TAGS" ./...`, skip: "its -tags value comes from the environment"},
		{run: "go test -p $N ./...", skip: "its -p value comes from the environment"},
		// Other runners.
		{run: "npx vitest run src/a.test.ts", skip: "it runs only the tests matching src/a.test.ts"},
		{run: "npx vitest run unit", skip: "it runs only the tests matching unit"},
		{run: "npx jest unit", skip: "it runs only the tests matching unit"},
		{run: "npx vitest run --reporter dot --config vitest.unit.ts"},
		{run: "npx vitest run ."},
		{run: "npm test --workspace=web", skip: "it passes --workspace"},
		{run: "npm test -w web", skip: "it passes -w"},
		{run: "pnpm test --filter web", skip: "it passes --filter"},
		{run: "pytest -n auto"},
		{run: `npx jest -t "adds"`, skip: "it passes -t"},
		{run: "npx jest --shard=1/4", skip: "it passes --shard"},
		{run: "pytest -k smoke", skip: "it passes -k"},
		{run: "pytest --junitxml=report.xml", skip: "it passes --junitxml"},
		{run: "pnpm test --findRelatedTests src/a.ts", skip: "it passes --findRelatedTests"},
		// Found (a duplicate of what smates runs), never replaced.
		{run: "pnpm exec turbo run test --filter=web", skip: "turbo runs the tests through its own task graph"},
		{run: "npx turbo run test", skip: "turbo runs the tests"},
		{run: "turbo test", skip: "turbo runs the tests"},
		{run: "turbo run test:e2e", skip: "is not the plain unit-test command", other: true},
		{run: "node --test scripts/", skip: "node --test runs the files it is given"},
		{run: "cd web && pnpm test", skip: "chained with other commands"},
	}
	for _, c := range cases {
		steps := parseWorkflowSteps("ci.yml", []byte(head+"      - run: "+yamlStr(c.run)+"\n"))
		if len(steps) != 1 {
			t.Errorf("%s: steps %+v", c.run, steps)
			continue
		}
		st := steps[0]
		if (c.skip == "") != (st.Skip == "") || !strings.Contains(st.Skip, c.skip) || st.Other != c.other {
			t.Errorf("%s: skip %q other %t, want %q %t", c.run, st.Skip, st.Other, c.skip, c.other)
		}
		if got := st.Carry.String(); got != c.carry {
			t.Errorf("%s: carry %q, want %q", c.run, got, c.carry)
		}
	}
}

// TestStepSuites: the suites a step covers by its runner and the directory it runs in
// (the step's working-directory, else the job's or the workflow's defaults.run).
func TestStepSuites(t *testing.T) {
	suites := []config.Suite{
		{Name: "app", Runner: "go", Root: "pkg/app"},
		{Name: "shared", Runner: "go", Root: "pkg/shared"},
		{Name: "tool", Runner: "go", Root: "pkg/app/tool"},
		{Name: "root", Runner: "go", Root: "."},
		{Name: "js", Runner: "vitest", Root: ".", Exclude: []string{"web/**"}},
		{Name: "web", Runner: "vitest", Root: "web"},
	}
	cases := []struct {
		name     string
		workflow string
		want     []string
		partial  string
		skip     string
	}{
		{name: "no working directory: every suite of the runner (must keep all)",
			workflow: "jobs:\n  t:\n    steps:\n      - run: go test ./...\n",
			want:     []string{"app", "shared", "tool", "root"}},
		{name: "workflow defaults.run",
			workflow: "defaults:\n  run:\n    working-directory: pkg/shared\njobs:\n  t:\n    steps:\n      - run: go test ./...\n",
			want:     []string{"shared"}},
		{name: "job defaults.run wins over the workflow's",
			workflow: "defaults:\n  run:\n    working-directory: pkg/shared\njobs:\n  t:\n    defaults:\n      run:\n        working-directory: pkg/app\n    steps:\n      - run: go test ./...\n",
			want:     []string{"app", "tool"}},
		{name: "step working-directory wins over defaults",
			workflow: "jobs:\n  t:\n    defaults:\n      run:\n        working-directory: pkg/app\n    steps:\n      - working-directory: pkg/shared\n        run: go test ./...\n",
			want:     []string{"shared"}},
		{name: "a module nested under the directory is kept (must keep)",
			workflow: "jobs:\n  t:\n    steps:\n      - working-directory: pkg/app\n        run: go test ./...\n",
			want:     []string{"app", "tool"}},
		{name: "a parent directory covers the suites below it (must keep)",
			workflow: "jobs:\n  t:\n    steps:\n      - working-directory: pkg\n        run: go test ./...\n",
			want:     []string{"app", "shared", "tool", "root"}, partial: "root"},
		{name: "normalized path",
			workflow: "jobs:\n  t:\n    steps:\n      - working-directory: ./pkg/shared/\n        run: go test ./...\n",
			want:     []string{"shared"}},
		{name: "workspace expression",
			workflow: "jobs:\n  t:\n    steps:\n      - working-directory: ${{ github.workspace }}/pkg/shared\n        run: go test ./...\n",
			want:     []string{"shared"}},
		{name: "inside a suite's root: part of it",
			workflow: "jobs:\n  t:\n    steps:\n      - working-directory: pkg/app/internal\n        run: go test ./...\n",
			want:     []string{"app"}, partial: "app"},
		{name: "inside the root suite: part of it",
			workflow: "jobs:\n  t:\n    steps:\n      - working-directory: cmd\n        run: go test ./...\n",
			want:     []string{"root"}, partial: "root"},
		{name: "a package root ends the outer suite",
			workflow: "jobs:\n  t:\n    steps:\n      - working-directory: web\n        run: pnpm test\n",
			want:     []string{"web"}},
		{name: "a directory the outer suite excludes",
			workflow: "jobs:\n  t:\n    steps:\n      - working-directory: web/sub\n        run: pnpm test\n",
			want:     []string{"web"}, partial: "web"},
		{name: "matrix working directory",
			workflow: "jobs:\n  t:\n    steps:\n      - working-directory: ${{ matrix.dir }}\n        run: go test ./...\n",
			want:     []string{"app", "shared", "tool", "root"}, skip: "changes with the matrix leg"},
		{name: "absolute working directory",
			workflow: "jobs:\n  t:\n    steps:\n      - working-directory: /tmp/x\n        run: go test ./...\n",
			want:     []string{"app", "shared", "tool", "root"}, skip: "not a directory of the repository"},
		{name: "working directory out of the repository",
			workflow: "jobs:\n  t:\n    steps:\n      - working-directory: ../other\n        run: go test ./...\n",
			want:     []string{"app", "shared", "tool", "root"}, skip: "not a directory of the repository"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			steps := parseWorkflowSteps("ci.yml", []byte("on: push\n"+c.workflow))
			if len(steps) != 1 {
				t.Fatalf("steps %+v", steps)
			}
			names, partial := stepSuites(steps[0], suites)
			if !reflect.DeepEqual(names, c.want) || partial != c.partial {
				t.Errorf("suites %v partial %q, want %v %q", names, partial, c.want, c.partial)
			}
			_, why := stepPatch(steps[0], suites)
			switch {
			case c.skip != "":
				if !strings.Contains(why, c.skip) {
					t.Errorf("why %q, want %q", why, c.skip)
				}
			case c.partial != "":
				if !strings.Contains(why, "runs only part of that suite") {
					t.Errorf("why %q", why)
				}
			case why != "":
				t.Errorf("not patched: %s", why)
			}
		})
	}
}

func TestGoCarry(t *testing.T) {
	cases := []struct {
		command string
		carry   goCarry
		want    string // "" = unchanged
		notGo   bool
	}{
		{"", goCarry{P: 1}, "go test -p 1", false},
		{"go test", goCarry{Race: true, Tags: []string{"db"}}, "go test -tags db -race", false},
		{"go test -race", goCarry{P: 1, Race: true}, "go test -p 1 -race", false},
		{"go test -p 4 -v", goCarry{P: 1}, "go test -p 1 -v", false},
		{"go test -p 1", goCarry{P: 4}, "", false},
		{"go test -tags a", goCarry{Tags: []string{"b"}}, "", false}, // the command keeps its own tags
		{"go test -timeout 5m", goCarry{Timeout: "20m"}, "go test -timeout 20m", false},
		{"go test -timeout 30m", goCarry{Timeout: "20m"}, "", false},
		{"go test -timeout 0", goCarry{Timeout: "20m"}, "", false},
		{"go test", goCarry{Timeout: "5m"}, "", false}, // shorter than the default
		{"go test -count=1 -args -x", goCarry{Race: true}, "go test -race -count=1 -args -x", false},
		{"gotestsum --", goCarry{P: 1}, "", true},
	}
	for _, c := range cases {
		got, changed, ok := withGoCarry(c.command, c.carry)
		if ok == c.notGo || changed != (c.want != "") || (changed && got != c.want) {
			t.Errorf("%q + %s: %q changed %t ok %t, want %q", c.command, c.carry, got, changed, ok, c.want)
		}
	}
	merges := []struct {
		a, b     goCarry
		want     string
		conflict bool
	}{
		{goCarry{P: 4}, goCarry{P: 1}, "-p 1", false},
		{goCarry{}, goCarry{P: 2}, "-p 2", false},
		{goCarry{Race: true}, goCarry{}, "-race", false},
		{goCarry{Timeout: "30m"}, goCarry{Timeout: "1h"}, "-timeout 1h", false},
		{goCarry{Timeout: "30m"}, goCarry{Timeout: "0"}, "-timeout 0", false},
		{goCarry{}, goCarry{Timeout: "5m"}, "", false},
		{goCarry{Tags: []string{"a"}}, goCarry{Tags: []string{"a"}}, "-tags a", false},
		// A union of tags could leave out files the other command ran (//go:build !a).
		{goCarry{Tags: []string{"a"}}, goCarry{}, "", true},
		{goCarry{Tags: []string{"a"}}, goCarry{Tags: []string{"b"}}, "", true},
	}
	for _, m := range merges {
		got, conflict := m.a.merge(m.b)
		if got.String() != m.want || conflict != m.conflict {
			t.Errorf("%s + %s: %q %t, want %q %t", m.a, m.b, got, conflict, m.want, m.conflict)
		}
	}
	unmet := []struct {
		need, have goCarry
		want       []string
	}{
		{goCarry{P: 1}, goCarry{}, []string{"-p 1"}},
		{goCarry{P: 1}, goCarry{P: 1}, nil},
		{goCarry{P: 2}, goCarry{P: 1}, nil},
		{goCarry{Race: true}, goCarry{}, []string{"-race"}},
		{goCarry{}, goCarry{Race: true}, nil},
		{goCarry{Timeout: "20m"}, goCarry{}, []string{"-timeout 20m"}},
		{goCarry{Timeout: "5m"}, goCarry{}, nil},
		{goCarry{Tags: []string{"a"}}, goCarry{}, []string{"-tags a"}},
		{goCarry{}, goCarry{Tags: []string{"a"}}, []string{"no -tags"}},
	}
	for _, u := range unmet {
		if got := u.need.unmet(u.have); !slices.Equal(got, u.want) {
			t.Errorf("%s over %s: %q, want %q", u.need, u.have, got, u.want)
		}
	}
}

// TestCarryGoFlags: the go test flags of the workflows and the Makefile go into the
// command of the suite they run, only for that suite.
func TestCarryGoFlags(t *testing.T) {
	suites := func() []config.Suite {
		return []config.Suite{{Name: "app", Runner: "go", Root: "pkg/app"}, {Name: "shared", Runner: "go", Root: "pkg/shared"}, {Name: "web", Runner: "vitest", Root: "web"}}
	}
	step := func(wd, run string) string {
		return "      - working-directory: " + wd + "\n        run: " + yamlStr(run) + "\n"
	}
	cases := []struct {
		name     string
		steps    string
		workflow string            // the whole workflow, in place of steps
		makefile map[string]string // dir -> Makefile
		want     map[string]string // suite -> command
		stderr   string
		noStderr string
	}{
		{name: "-p 1 only in the workflow", steps: step("pkg/app", "go test -p 1 ./..."),
			want: map[string]string{"app": "go test -p 1"}},
		{name: "per-module jobs: each suite gets its own flags",
			steps: step("pkg/app", "go test -p 1 ./...") + step("pkg/shared", "go test -race ./..."),
			want:  map[string]string{"app": "go test -p 1", "shared": "go test -race"}},
		{name: "-p 1 in the module's Makefile", makefile: map[string]string{"pkg/app": "test:\n\tgo test -p 1 $$(go list ./... | grep -v /slow/)\n"},
			want: map[string]string{"app": "go test -p 1"}},
		{name: "Makefile and workflow combine toward the safer run",
			steps:    step("pkg/app", "go test -p 4 -timeout 30m ./..."),
			makefile: map[string]string{"pkg/app": "test:\n\tgo test -p 1 -race ./...\n"},
			want:     map[string]string{"app": "go test -p 1 -race -timeout 30m"}},
		{name: "a root Makefile running another module's tests is not applied to the first suite",
			makefile: map[string]string{".": "test:\n\tgo test -p 1 ./...\n"},
			want:     map[string]string{}},
		{name: "tags that differ between steps are not carried (must keep the untagged files)",
			steps:  step("pkg/app", "go test ./...") + step("pkg/app", "go test -tags integration ./..."),
			want:   map[string]string{},
			stderr: "different -tags"},
		{name: "a step running only some tests is no source",
			steps: step("pkg/app", "go test -race -run '^$' ./..."),
			want:  map[string]string{}},
		{name: "a step without flags changes nothing", steps: step("pkg/app", "go test ./..."),
			want: map[string]string{}},
		{name: "tags of a plain step running the whole suite are carried",
			steps: step("pkg/app", "go test -tags integration ./..."),
			want:  map[string]string{"app": "go test -tags integration"}},
		// Must keep the untagged files: -tags only from runs of all of the suite's tests.
		{name: "tags of a step testing a few packages are not carried",
			steps:  step("pkg/app", "go test -tags integration -p 1 ./internal/db/..."),
			want:   map[string]string{"app": "go test -p 1"},
			stderr: "run only some of its tests with -tags"},
		{name: "a tagless test target and a tagged step disagree",
			steps:    step("pkg/app", "go test -tags integration ./..."),
			makefile: map[string]string{"pkg/app": "test:\n\tgo test -v ./...\n"},
			want:     map[string]string{},
			stderr:   "different -tags"},
		{name: "a test-integration target does not tag the suite",
			makefile: map[string]string{"pkg/app": "test:\n\tgo test ./...\n\ntest-integration:\n\tgo test -tags integration -p 1 ./...\n"},
			want:     map[string]string{"app": "go test -p 1"},
			stderr:   "run only some of its tests with -tags"},
		{name: "make test in one job, a tagged package step in another",
			steps:    "      - run: make test\n" + step("pkg/app", "go test -tags integration ./internal/db/..."),
			makefile: map[string]string{"pkg/app": "test:\n\tgo test ./...\n"},
			want:     map[string]string{}},
		{name: "a plain test target with tags is carried",
			makefile: map[string]string{"pkg/app": "test:\n\tgo test -tags db ./...\n"},
			want:     map[string]string{"app": "go test -tags db"}},
		// The GOFLAGS of a step's env: apply to its go test: its -tags vote.
		{name: "env GOFLAGS tags agree with the test target",
			steps:    envStep("pkg/app", "-tags=integration", "go test ./..."),
			makefile: map[string]string{"pkg/app": "test:\n\tgo test -tags integration ./...\n"},
			want:     map[string]string{"app": "go test -tags integration"}, noStderr: "different -tags"},
		{name: "env GOFLAGS tags alone are carried",
			steps: envStep("pkg/app", "-tags=integration -p=1", "go test ./..."),
			want:  map[string]string{"app": "go test -p 1 -tags integration"}},
		{name: "the command's own -tags win over env GOFLAGS",
			steps:    envStep("pkg/app", "-tags=integration", "go test -tags db ./..."),
			makefile: map[string]string{"pkg/app": "test:\n\tgo test -tags db ./...\n"},
			want:     map[string]string{"app": "go test -tags db"}, noStderr: "different -tags"},
		// Must keep apart: an inline GOFLAGS replaces the env's, so the step runs untagged.
		{name: "an inline GOFLAGS replaces env GOFLAGS",
			steps:    envStep("pkg/app", "-tags=integration", "GOFLAGS=-count=1 go test ./..."),
			makefile: map[string]string{"pkg/app": "test:\n\tgo test -tags integration ./...\n"},
			want:     map[string]string{}, stderr: "different -tags"},
		{name: "env GOFLAGS from an expression does not vote",
			steps:    envStep("pkg/app", "${{ vars.GOFLAGS }}", "go test ./..."),
			makefile: map[string]string{"pkg/app": "test:\n\tgo test -tags integration ./...\n"},
			want:     map[string]string{"app": "go test -tags integration"}, noStderr: "different -tags"},
		// An env: written as an expression may set GOFLAGS: the step does not vote, at
		// any level.
		{name: "an env: expression of the workflow does not vote",
			workflow: "on: push\nenv: ${{ fromJSON(vars.CI_ENV) }}\njobs:\n  t:\n    steps:\n" + step("pkg/app", "go test ./..."),
			makefile: map[string]string{"pkg/app": "test:\n\tgo test -tags integration ./...\n"},
			want:     map[string]string{"app": "go test -tags integration"}, noStderr: "different -tags"},
		{name: "an env: expression of the job does not vote",
			workflow: "on: push\njobs:\n  t:\n    env: ${{ fromJSON(vars.CI_ENV) }}\n    steps:\n" + step("pkg/app", "go test ./..."),
			makefile: map[string]string{"pkg/app": "test:\n\tgo test -tags integration ./...\n"},
			want:     map[string]string{"app": "go test -tags integration"}, noStderr: "different -tags"},
		{name: "an env: expression of the step does not vote",
			workflow: "on: push\njobs:\n  t:\n    steps:\n      - working-directory: pkg/app\n        env: ${{ fromJSON(vars.CI_ENV) }}\n        run: go test ./...\n",
			makefile: map[string]string{"pkg/app": "test:\n\tgo test -tags integration ./...\n"},
			want:     map[string]string{"app": "go test -tags integration"}, noStderr: "different -tags"},
		{name: "a GOFLAGS that is no plain value does not vote",
			workflow: "on: push\njobs:\n  t:\n    env:\n      GOFLAGS: [-tags=integration]\n    steps:\n" + step("pkg/app", "go test ./..."),
			makefile: map[string]string{"pkg/app": "test:\n\tgo test -tags integration ./...\n"},
			want:     map[string]string{"app": "go test -tags integration"}, noStderr: "different -tags"},
		// The command line's -race=false wins over the -race of GOFLAGS.
		{name: "-race=false on the command line over env GOFLAGS -race",
			steps: envStep("pkg/app", "-race", "go test -race=false ./..."),
			want:  map[string]string{}},
		{name: "env GOFLAGS -race alone is carried",
			steps: envStep("pkg/app", "-race", "go test ./..."),
			want:  map[string]string{"app": "go test -race"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			wf := c.workflow
			if wf == "" {
				wf = "on: push\njobs:\n  t:\n    steps:\n" + c.steps
			}
			files := map[string]string{".github/workflows/ci.yml": wf}
			for d, mk := range c.makefile {
				files[joinRel(d, "Makefile")] = mk
			}
			dir := treeDir(t, files)
			ss := suites()
			var errb bytes.Buffer
			carryGoFlags(ss, makefileHints(dir, ss), makeGoVotes(dir, ss), workflowTestSteps(dir), &errb)
			got := map[string]string{}
			for _, s := range ss {
				if s.Command != "" {
					got[s.Name] = s.Command
				}
			}
			if !reflect.DeepEqual(got, c.want) {
				t.Errorf("commands %v, want %v\n%s", got, c.want, errb.String())
			}
			if !strings.Contains(errb.String(), c.stderr) {
				t.Errorf("stderr lacks %q:\n%s", c.stderr, errb.String())
			}
			if c.noStderr != "" && strings.Contains(errb.String(), c.noStderr) {
				t.Errorf("stderr has %q:\n%s", c.noStderr, errb.String())
			}
		})
	}
}

// envStep is a workflow step running run in directory wd with GOFLAGS set in its env:.
func envStep(wd, goflags, run string) string {
	return "      - working-directory: " + wd + "\n        env:\n          GOFLAGS: " + yamlStr(goflags) + "\n        run: " + yamlStr(run) + "\n"
}

func TestMakefileShellWords(t *testing.T) {
	suites := []config.Suite{{Name: "app", Runner: "go", Root: "pkg/app"}, {Name: "root", Runner: "go", Root: "."}}
	cases := []struct {
		recipe string
		suite  string
		cmd    string
		picked string
		exact  bool
	}{
		{"go test -p 1 $$(go list ./... | grep -v /slow/)", "root", "go test -p 1", "$(go list ./... | grep -v /slow/)", true},
		{"go test -race `go list ./...`", "root", "go test -race", "`go list ./...`", true},
		{"cd pkg/app && go test -p 1 ./... | tee out.txt", "app", "go test -p 1", "", true},
		{"cd pkg/other && go test -p 1 ./...", "app", "go test -p 1", "", false},
		{"$(GO) test -tags 'a b' ./... 2>&1", "root", "$(GO) test -tags 'a b'", "", true},
	}
	for _, c := range cases {
		h, ok := parseRecipeLine(c.recipe, ".", suites, false)
		if !ok || h.Suite != c.suite || h.Command != c.cmd || h.Picked != c.picked || h.Exact != c.exact {
			t.Errorf("%q: %+v %t, want %s %q %q exact %t", c.recipe, h, ok, c.suite, c.cmd, c.picked, c.exact)
		}
	}
	doc := renderConfig([]config.Suite{{Name: "root", Runner: "go", Root: ".", Command: "go test -p 1"}},
		configHints([]makeHint{{Suite: "root", File: "Makefile", Target: "test", Line: "go test -p 1 -v $$(go list ./...)", Command: "go test -p 1 -v", Picked: "$(go list ./...)"}},
			map[string]envHint{"root": {From: `.github/workflows/ci.yml (job "t")`, Env: [][2]string{{"DB_PORT", "3307"}}}},
			map[string]string{"root": "-p 1"}))
	if _, err := config.Parse([]byte(doc)); err != nil {
		t.Fatalf("config does not parse: %v\n%s", err, doc)
	}
	for _, s := range []string{
		"    command: go test -p 1\n",
		"    # smates runs the runner itself; command: above keeps -p 1; to keep the other flags/variables too, replace it with:\n    # command: \"go test -p 1 -v\"\n",
		"    # It picks the packages with $(go list ./...): smates runs every package of the suite",
		"    # .github/workflows/ci.yml (job \"t\") sets env: for these tests (CI values: check them before uncommenting):\n    # env:\n    #   DB_PORT: \"3307\"\n",
	} {
		if !strings.Contains(doc, s) {
			t.Errorf("config lacks %q:\n%s", s, doc)
		}
	}
	if strings.Contains(doc, "env_ignore") {
		t.Errorf("env_ignore written:\n%s", doc)
	}
}

func TestWorkflowSeed(t *testing.T) {
	cases := []struct {
		on   string
		kind seedKind
		pr   bool
	}{
		{"on: pull_request\n", seedNo, true},
		{"on: [pull_request, workflow_dispatch]\n", seedNo, true},
		{"on:\n  pull_request_target:\n", seedNo, true},
		{"on: push\n", seedYes, false},
		{"on:\n  pull_request:\n  push:\n    branches: [main]\n", seedYes, true},
		{"on:\n  pull_request:\n  push:\n    branches: [develop]\n", seedNo, true},
		{"on:\n  pull_request:\n  push:\n    branches: ['**']\n", seedYes, true},
		{"on:\n  pull_request:\n  push:\n    branches: ['release/*']\n", seedNo, true},
		{"on:\n  pull_request:\n  push:\n    branches: ['**', '!main']\n", seedNo, true},
		{"on:\n  pull_request:\n  push:\n    branches-ignore: [main]\n", seedNo, true},
		{"on:\n  pull_request:\n  push:\n    branches-ignore: [develop]\n", seedYes, true},
		{"on:\n  pull_request:\n  push:\n    tags: ['v*']\n", seedNo, true},
		{"on:\n  pull_request:\n  push:\n    paths: ['pkg/**']\n", seedYes, true},
		{"on:\n  pull_request:\n  schedule:\n    - cron: '0 0 * * *'\n", seedYes, true},
		{"on:\n  pull_request:\n  workflow_call:\n", seedUnknown, true},
		{"on:\n  workflow_run:\n    workflows: [ci]\n", seedUnknown, false},
		{"jobs: {}\n", seedUnknown, false},
	}
	for _, c := range cases {
		var doc yaml.Node
		if err := yaml.Unmarshal([]byte(c.on), &doc); err != nil {
			t.Fatal(err)
		}
		if kind, pr, _ := workflowSeed(doc.Content[0], "main"); kind != c.kind || pr != c.pr {
			t.Errorf("%q: %d %t, want %d %t", c.on, kind, pr, c.kind, c.pr)
		}
	}
}

// TestSeedPatch: a workflow running smates only on pull requests gets push and a
// nightly schedule in its on:, whatever form the on: has.
func TestSeedPatch(t *testing.T) {
	job := "jobs:\n  test:\n    runs-on: ubuntu-latest\n    steps:\n      - uses: actions/checkout@v5\n      - uses: dotneet/smates@v1\n"
	cases := []struct {
		name     string
		workflow string
		want     []string
		notWant  []string
		note     string
		noPatch  bool
	}{
		{name: "mapping with path filters",
			workflow: "name: t\non:\n  pull_request:\n    paths:\n      - 'pkg/app/**'\n      - go.work\n\n" + job,
			want:     []string{"on:\n  pull_request:\n    paths:\n      - 'pkg/app/**'\n      - go.work\n  push:   # runs on the default branch record the state pull requests start from\n    branches: [main]\n    paths:\n      - 'pkg/app/**'\n      - go.work\n  schedule:   # nightly full run on the default branch (see the smates docs on --shadow)\n    - cron: '23 3 * * *'\n\njobs:"}},
		{name: "four-space indentation",
			workflow: "on:\n    pull_request:\n        branches: [main]\n" + job,
			want:     []string{"on:\n    pull_request:\n        branches: [main]\n    push:   # runs", "\n        branches: [main]\n    schedule:"}},
		{name: "a string", workflow: "on: pull_request\n" + job,
			want: []string{"on:\n  pull_request:\n  push:", "  schedule:"}},
		{name: "a list", workflow: "on: [pull_request, workflow_dispatch]\n" + job,
			want: []string{"on:\n  pull_request:\n  workflow_dispatch:\n  push:"}},
		{name: "a push to another branch: only the schedule",
			workflow: "on:\n  pull_request:\n  push:\n    branches: [develop]\n" + job,
			want:     []string{"    branches: [develop]\n  schedule:"}, notWant: []string{"branches: [main]"},
			note: "its push trigger never runs on main"},
		{name: "pull-request-only context", workflow: "on: pull_request\n" + job + "        with:\n          args: ${{ github.event.pull_request.number }}\n",
			want: []string{"  schedule:"}, note: "refers to pull-request-only context"},
		{name: "a flow mapping is left to the user", workflow: "on: {pull_request: {}}\n" + job,
			noPatch: true, note: "add `push: branches: [main]` and a nightly `schedule:` to its on: by hand"},
		{name: "flow-style pull_request filters", workflow: "on:\n  pull_request: {paths: ['pkg/app/**', go.work]}\n" + job,
			want: []string{"  pull_request: {paths: ['pkg/app/**', go.work]}\n  push:   # runs on the default branch record the state pull requests start from\n    branches: [main]\n    paths: [pkg/app/**, go.work]\n  schedule:"}},
		{name: "flow-style pull_request paths-ignore with branches", workflow: "on:\n  pull_request: {branches: [main], paths-ignore: ['docs/**']}\n" + job,
			want: []string{"    branches: [main]\n    paths-ignore: [docs/**]\n  schedule:"}},
		{name: "flow-style filters needing quotes", workflow: "on:\n  pull_request: {paths: ['**.md', '!x,y', \"a: b\"]}\n" + job,
			want: []string{"    paths: ["}},
		{name: "seeded by a schedule", workflow: "on:\n  pull_request:\n  schedule:\n    - cron: '0 1 * * *'\n" + job, noPatch: true},
		{name: "CRLF", workflow: strings.ReplaceAll("on:\n  pull_request:\n"+job, "\n", "\r\n"),
			want: []string{"  pull_request:\r\n  push:   # runs on the default branch record the state pull requests start from\r\n    branches: [main]\r\n"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			g := newGitRepo(t)
			writeTree(t, g.dir, map[string]string{".github/workflows/ci.yml": c.workflow})
			g.git("add", ".")
			g.commit("ci")
			var seed []string
			for _, gap := range smatesSeedGaps(g.dir, "main") {
				seed = append(seed, gap.File)
			}
			patch, notes := workflowPatch(g.dir, nil, nil, "main", seed...)
			joined := strings.Join(notes, "\n")
			if !strings.Contains(joined, c.note) {
				t.Errorf("notes %q, want %q", joined, c.note)
			}
			if c.noPatch {
				if patch != "" {
					t.Errorf("patched:\n%s", patch)
				}
				return
			}
			pf := filepath.Join(t.TempDir(), "p.patch")
			if err := os.WriteFile(pf, []byte(patch), 0o644); err != nil {
				t.Fatal(err)
			}
			g.git("apply", pf)
			data, _ := os.ReadFile(filepath.Join(g.dir, ".github/workflows/ci.yml"))
			for _, w := range c.want {
				if !strings.Contains(string(data), w) {
					t.Errorf("lacks %q:\n%s", w, data)
				}
			}
			for _, w := range c.notWant {
				if strings.Contains(string(data), w) {
					t.Errorf("has %q:\n%s", w, data)
				}
			}
			var doc yaml.Node
			if err := yaml.Unmarshal(data, &doc); err != nil {
				t.Fatalf("patched workflow does not parse: %v\n%s", err, data)
			}
			// A yaml.Node accepts duplicate keys; a map does not (nor does GitHub).
			var m map[string]any
			if err := yaml.Unmarshal(data, &m); err != nil {
				t.Fatalf("patched workflow has duplicate keys: %v\n%s", err, data)
			}
			if kind, pr, _ := workflowSeed(doc.Content[0], "main"); kind != seedYes || !pr {
				t.Errorf("patched workflow: %d %t\n%s", kind, pr, data)
			}
		})
	}
}

func TestSmatesSeedGaps(t *testing.T) {
	pr := func(key string) string {
		with := ""
		if key != "" {
			with = "        with:\n          cache-key: " + key + "\n"
		}
		return "on: pull_request\njobs:\n  t:\n    steps:\n      - uses: dotneet/smates@v1\n" + with
	}
	nightly := func(key string) string {
		return strings.Replace(pr(key), "on: pull_request", "on:\n  schedule:\n    - cron: '0 1 * * *'", 1)
	}
	cases := []struct {
		name  string
		files map[string]string
		want  []seedGap
	}{
		{"pull requests only", map[string]string{"ci.yml": pr("")}, []seedGap{{File: ".github/workflows/ci.yml", Keys: []string{""}}}},
		{"seeded by a nightly workflow with the same key", map[string]string{"ci.yml": pr("app"), "nightly.yml": nightly("app")}, nil},
		{"a nightly workflow with another key", map[string]string{"ci.yml": pr("app"), "nightly.yml": nightly("shared")}, []seedGap{{File: ".github/workflows/ci.yml", Keys: []string{"app"}}}},
		{"pushes to the default branch", map[string]string{"ci.yml": strings.Replace(pr(""), "on: pull_request", "on: [pull_request, push]", 1)}, nil},
		{"a reusable workflow: its callers decide", map[string]string{"ci.yml": strings.Replace(pr(""), "on: pull_request", "on: [pull_request, workflow_call]", 1)}, nil},
		{"own storage", map[string]string{"ci.yml": pr("") + "        with:\n          cache: 'false'\n"}, []seedGap{{File: ".github/workflows/ci.yml", Keys: []string{"(own storage)"}}}},
		{"a run: step", map[string]string{"ci.yml": "on: pull_request\njobs:\n  t:\n    steps:\n      - run: smates state pull && smates run\n"}, []seedGap{{File: ".github/workflows/ci.yml", Keys: []string{"(state pull)"}}}},
		{"no smates", map[string]string{"ci.yml": "on: pull_request\njobs:\n  t:\n    steps:\n      - run: go test ./...\n"}, nil},
	}
	for _, c := range cases {
		files := map[string]string{}
		for f, body := range c.files {
			files[".github/workflows/"+f] = body
		}
		if got := smatesSeedGaps(treeDir(t, files), "main"); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: %+v, want %+v", c.name, got, c.want)
		}
	}
}

func TestStateRefsFor(t *testing.T) {
	cases := []struct {
		branch, base, def string
		want              []string
	}{
		{"feature", "", "main", []string{"refs/smates/feature", "refs/smates/main"}},
		{"feature", "main", "main", []string{"refs/smates/feature", "refs/smates/main"}},
		{"feature", "release", "main", []string{"refs/smates/feature", "refs/smates/release", "refs/smates/main"}},
		{"release", "release", "main", []string{"refs/smates/release", "refs/smates/main"}},
		{"main", "", "main", []string{"refs/smates/main"}},
		{"", "", "main", []string{"refs/smates/main"}},
	}
	for _, c := range cases {
		got := stateRefsFor(c.branch, c.base, c.def)
		for i := range got {
			got[i] = strings.Replace(got[i], stateRefPrefix, "refs/smates/", 1)
		}
		if !slices.Equal(got, c.want) {
			t.Errorf("%q %q %q: %q, want %q", c.branch, c.base, c.def, got, c.want)
		}
	}
}

// TestInitGitHubPerModuleWorkflows: one workflow per Go module, each running in the
// module through defaults.run, only on pull requests. The patch runs each module's
// suite in its own job with its own cache-key, keeps -p 1 in smates.yml, leaves the
// targeted step alone and adds the seeding triggers.
func TestInitGitHubPerModuleWorkflows(t *testing.T) {
	clearCI(t)
	noAgentCredentials(t)
	module := func(m string) string {
		return "name: " + m + "\non:\n  pull_request:\n    paths: ['pkg/" + m + "/**']\ndefaults:\n  run:\n    working-directory: pkg/" + m + "\njobs:\n  test:\n    runs-on: ubuntu-latest\n    steps:\n      - uses: actions/checkout@v5\n      - name: unit\n        run: go test -p 1 ./...\n        env:\n          DB_PORT: \"3307\"\n"
	}
	g := newGitRepo(t)
	writeTree(t, g.dir, map[string]string{
		".github/workflows/app.yml":    module("app"),
		".github/workflows/shared.yml": module("shared") + "  dates:\n    runs-on: ubuntu-latest\n    steps:\n      - run: go test -run '^TestDate$' -count=1 ./calendar\n        working-directory: pkg/shared\n",
		"pkg/app/go.mod":               "module example.com/app\n\ngo 1.22\n",
		"pkg/app/a_test.go":            "package app\n",
		"pkg/shared/go.mod":            "module example.com/shared\n\ngo 1.22\n",
		"pkg/shared/s_test.go":         "package shared\n",
	})
	g.git("add", ".")
	g.commit("files")
	pf := filepath.Join(t.TempDir(), "smates.patch")
	var out, errb bytes.Buffer
	if code := Main([]string{"init", "--github", "--patch", pf, "-C", g.dir}, &out, &errb); code != 0 {
		t.Fatalf("init: %d %s", code, errb.String())
	}
	cfg, err := config.Load(g.dir)
	if err != nil || cfg == nil {
		t.Fatalf("config: %v", err)
	}
	for _, s := range cfg.Suites {
		if s.Command != "go test -p 1" {
			t.Errorf("suite %s command %q, want go test -p 1\n%s", s.Name, s.Command, errb.String())
		}
	}
	raw, _ := os.ReadFile(filepath.Join(g.dir, "smates.yml"))
	if !strings.Contains(string(raw), "#   DB_PORT: \"3307\"") || strings.Contains(string(raw), "env_ignore") {
		t.Errorf("smates.yml env hint:\n%s", raw)
	}
	if !strings.Contains(errb.String(), "it passes -run") {
		t.Errorf("targeted step not reported:\n%s", errb.String())
	}
	g.git("apply", pf)
	for _, m := range []string{"app", "shared"} {
		data, _ := os.ReadFile(filepath.Join(g.dir, ".github/workflows/"+m+".yml"))
		for _, w := range []string{
			"          args: --suite " + suiteNamed(cfg.Suites, "pkg/"+m) + "\n          cache-key: go-pkg-" + m + "\n",
			"        env:\n          DB_PORT: \"3307\"\n",
			"  push:   # runs on the default branch record the state pull requests start from\n    branches: [main]\n    paths: ['pkg/" + m + "/**']\n",
			"  schedule:",
		} {
			if !strings.Contains(string(data), w) {
				t.Errorf("%s.yml lacks %q:\n%s", m, w, data)
			}
		}
		if strings.Contains(string(data), "working-directory: pkg/"+m+"\n          ") || strings.Contains(string(data), "    working-directory: pkg") && strings.Contains(string(data), "with:\n          working-directory") {
			t.Errorf("%s.yml: action working-directory set:\n%s", m, data)
		}
	}
	data, _ := os.ReadFile(filepath.Join(g.dir, ".github/workflows/shared.yml"))
	if !strings.Contains(string(data), "go test -run '^TestDate$' -count=1 ./calendar") {
		t.Errorf("targeted step replaced:\n%s", data)
	}
	cmd := exec.Command("git", "diff", "--stat")
	cmd.Dir = g.dir
	if b, err := cmd.CombinedOutput(); err != nil || len(b) == 0 {
		t.Errorf("nothing applied: %v %s", err, b)
	}
}

func suiteNamed(suites []config.Suite, root string) string {
	for _, s := range suites {
		if s.Root == root {
			return s.Name
		}
	}
	return ""
}

// TestDoctorWorkflowNoSeed: doctor warns about a workflow running smates only on pull
// requests.
func TestDoctorWorkflowNoSeed(t *testing.T) {
	g := newGitRepo(t)
	writeTree(t, g.dir, map[string]string{
		".github/workflows/ci.yml": "on: pull_request\njobs:\n  t:\n    steps:\n      - uses: dotneet/smates@v1\n        with:\n          cache-key: unit\n",
		"go.mod":                   "module example.com/x\n\ngo 1.22\n",
		"x_test.go":                "package x\n",
		"smates.yml":               "version: 1\nsuites:\n  - name: unit\n    runner: go\n    root: .\n",
	})
	g.git("add", ".")
	g.commit("files")
	var out, errb bytes.Buffer
	Main([]string{"doctor", "-C", g.dir}, &out, &errb)
	if !strings.Contains(out.String(), ".github/workflows/ci.yml runs smates only on pull requests, and no workflow runs it on main (cache-key unit)") {
		t.Errorf("no workflow-no-seed warning:\n%s", out.String())
	}
	writeTree(t, g.dir, map[string]string{".github/workflows/nightly.yml": "on:\n  schedule:\n    - cron: '0 1 * * *'\njobs:\n  t:\n    steps:\n      - uses: dotneet/smates@v1\n        with:\n          cache-key: unit\n          args: --shadow\n"})
	out.Reset()
	Main([]string{"doctor", "-C", g.dir}, &out, &errb)
	if strings.Contains(out.String(), "runs smates only on pull requests") {
		t.Errorf("warned although a nightly workflow seeds the state:\n%s", out.String())
	}
}

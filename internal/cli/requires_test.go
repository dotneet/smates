package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/dotneet/smates/internal/config"
	"github.com/dotneet/smates/internal/runner"
	"github.com/dotneet/smates/internal/selector"
	"github.com/dotneet/smates/internal/state"
)

// --max-time cuts each batch by recorded durations, in the order the units start.
func TestPlanMaxTime(t *testing.T) {
	sec := int64(1000)
	recorded := &selector.Suite{Name: "go", Suite: &runner.Suite{Suite: config.Suite{Name: "go"}}, Tests: []*selector.Test{
		{ID: "pkg/c", Selected: true, Reason: selector.ReasonChanged, Duration: 40 * sec},
		{ID: "pkg/a", Selected: true, Reason: selector.ReasonChanged, Duration: 20 * sec},
		{ID: "pkg/d", Selected: true, Reason: selector.ReasonChanged, Duration: 10 * sec},
		{ID: "pkg/b", Selected: true, Reason: selector.ReasonChanged, Duration: 30 * sec},
		{ID: "pkg/huge", Selected: true, Reason: selector.ReasonChanged, Duration: 300 * sec},
		{ID: "pkg/new", Selected: true, Reason: selector.ReasonNew}, // estimated at the mean
		{ID: "pkg/failed", Selected: true, Reason: selector.ReasonFailed, Duration: 5 * sec},
		{ID: "pkg/idle", Reason: selector.ReasonUnaffected, Duration: 4 * sec},
	}}
	cold := &selector.Suite{Name: "web", Suite: &runner.Suite{Suite: config.Suite{Name: "web"}}, Tests: []*selector.Test{
		{ID: "a.test.ts", Selected: true, Reason: selector.ReasonNew},
		{ID: "b.test.ts", Selected: true, Reason: selector.ReasonNew},
	}}
	res := &selector.Result{Mode: selector.ModeState, Suites: []*selector.Suite{recorded, cold}}
	entries := map[string]*state.Entry{}
	for _, tt := range recorded.Tests {
		if tt.ID != "pkg/new" {
			entries[tt.ID] = &state.Entry{Status: state.Pass}
		}
	}
	entries["pkg/failed"].Status = state.Fail
	st := &state.State{Suites: map[string]*state.Suite{"go": {Tests: entries}}}
	prioritize(res, st, nil)

	type step struct {
		suite   string
		units   []string
		est     time.Duration
		unknown bool
	}
	// The mean of go's recorded durations: (40+20+10+30+300+5+4)/7 s.
	mean := (409 * time.Second) / 7
	for _, tc := range []struct {
		name     string
		maxTime  time.Duration
		failFast bool
		want     []step
	}{
		{"no limit", 0, false, []step{
			{"go", []string{"pkg/failed", "pkg/d", "pkg/a", "pkg/b", "pkg/c", "pkg/huge", "pkg/new"}, 0, false},
			{"web", []string{"a.test.ts", "b.test.ts"}, 0, false},
		}},
		// A quarter of 4m per batch; a unit longer than that runs alone; a suite without
		// recorded durations stays in one batch of unknown duration.
		{"4m", 4 * time.Minute, false, []step{
			{"go", []string{"pkg/failed", "pkg/d", "pkg/a"}, 35 * time.Second, false},
			{"go", []string{"pkg/b"}, 30 * time.Second, false},
			{"go", []string{"pkg/c"}, 40 * time.Second, false},
			{"go", []string{"pkg/huge"}, 300 * time.Second, false},
			{"go", []string{"pkg/new"}, mean, false},
			{"web", []string{"a.test.ts", "b.test.ts"}, 0, true},
		}},
		{"20m: everything in one batch per suite", 20 * time.Minute, false, []step{
			{"go", []string{"pkg/failed", "pkg/d", "pkg/a", "pkg/b", "pkg/c"}, 105 * time.Second, false},
			{"go", []string{"pkg/huge"}, 300 * time.Second, false},
			{"go", []string{"pkg/new"}, mean, false},
			{"web", []string{"a.test.ts", "b.test.ts"}, 0, true},
		}},
		{"with --fail-fast, priorities first", 4 * time.Minute, true, []step{
			{"go", []string{"pkg/failed"}, 5 * time.Second, false},
			{"go", []string{"pkg/d", "pkg/a", "pkg/b"}, 60 * time.Second, false},
			{"go", []string{"pkg/c"}, 40 * time.Second, false},
			{"go", []string{"pkg/huge"}, 300 * time.Second, false},
			{"go", []string{"pkg/new"}, mean, false},
			{"web", []string{"a.test.ts", "b.test.ts"}, 0, true},
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := &cycle{p: &project{state: st}, f: &runFlags{maxTime: tc.maxTime, failFast: tc.failFast}, sel: &selection{res: res}}
			var got []step
			for _, b := range c.plan() {
				if b.full {
					t.Errorf("%s: a partial selection is not a full run", b.sr.Name)
				}
				got = append(got, step{b.sr.Name, b.units, b.est, b.unknown})
			}
			if tc.maxTime == 0 {
				for i := range got {
					got[i].est, got[i].unknown = 0, false
				}
			}
			if !slices.EqualFunc(got, tc.want, func(a, b step) bool {
				return a.suite == b.suite && slices.Equal(a.units, b.units) && a.est == b.est && a.unknown == b.unknown
			}) {
				t.Errorf("plan\n got %v\nwant %v", got, tc.want)
			}
		})
	}
}

// A full run cut by --max-time passes explicit units to every batch; one that fits in a
// single batch stays a full run.
func TestPlanMaxTimeFullRun(t *testing.T) {
	sr := &selector.Suite{Name: "go", All: true, Suite: &runner.Suite{Suite: config.Suite{Name: "go"}}, Tests: []*selector.Test{
		{ID: "pkg/a", Selected: true, Reason: selector.ReasonAll, Duration: 60000},
		{ID: "pkg/b", Selected: true, Reason: selector.ReasonAll, Duration: 60000},
	}}
	res := &selector.Result{Mode: selector.ModeAll, Suites: []*selector.Suite{sr}}
	st := &state.State{Suites: map[string]*state.Suite{"go": {Tests: map[string]*state.Entry{"pkg/a": {}, "pkg/b": {}}}}}
	prioritize(res, st, nil)
	for _, tc := range []struct {
		maxTime time.Duration
		batches int
		full    bool
	}{{4 * time.Minute, 2, false}, {time.Hour, 1, true}} {
		c := &cycle{p: &project{state: st}, f: &runFlags{maxTime: tc.maxTime}, sel: &selection{res: res}, runAll: true}
		plan := c.plan()
		if len(plan) != tc.batches || slices.ContainsFunc(plan, func(b *batch) bool { return b.full != tc.full }) {
			t.Errorf("--max-time %s: %d batches (full %v), want %d (full %v)", tc.maxTime, len(plan), plan[0].full, tc.batches, tc.full)
		}
	}
}

func TestOverTime(t *testing.T) {
	for _, tc := range []struct {
		name    string
		maxTime time.Duration
		started bool
		elapsed time.Duration
		est     time.Duration
		unknown bool
		want    bool
	}{
		{"no limit", 0, true, time.Hour, time.Hour, false, false},
		{"the first batch always starts", time.Minute, false, 0, time.Hour, false, false},
		{"the first batch of unknown duration starts", time.Minute, false, 0, 0, true, false},
		{"fits", time.Minute, true, 20 * time.Second, 30 * time.Second, false, false},
		{"would end after the limit", time.Minute, true, 40 * time.Second, 30 * time.Second, false, true},
		{"the limit has passed", time.Minute, true, 2 * time.Minute, 0, false, true},
		{"unknown duration within the first half", time.Hour, true, 29 * time.Minute, 0, true, false},
		{"unknown duration after half the time", time.Hour, true, 30 * time.Minute, 0, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := &cycle{f: &runFlags{maxTime: tc.maxTime}, start: time.Now().Add(-tc.elapsed), started: map[string][]string{}}
			if tc.started {
				c.started["go"] = []string{"pkg/a"}
			}
			if got := c.overTime(&batch{est: tc.est, unknown: tc.unknown}); got != tc.want {
				t.Errorf("overTime = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestParseRunFlagsMaxTime(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want string // error substring; "" = valid
	}{
		{[]string{"--max-time", "8m"}, ""},
		{[]string{"--max-time", "0"}, ""},
		{[]string{"--max-time", "8m", "--fail-fast", "--shard", "1/2"}, ""},
		{[]string{"--max-time", "-1s"}, "must not be negative"},
		{[]string{"--max-time", "soon"}, "invalid value"},
		{[]string{"--max-time", "8m", "--shadow"}, "--max-time"},
	} {
		_, err := parseRunFlags(tc.args, &bytes.Buffer{})
		var ue usageError
		switch {
		case tc.want == "" && err != nil:
			t.Errorf("%v: %v", tc.args, err)
		case tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want) || !errors.As(err, &ue)):
			t.Errorf("%v: %v, want a usage error with %q", tc.args, err, tc.want)
		}
	}
}

// requiresFixture is outputFixture with the suites good and bad, recorded by a full
// run, and then the given requires entries (YAML flow sequence items) added to good.
func requiresFixture(t *testing.T, requires string) (dir string, write func(rel, content string), run func(args ...string) (int, string, string)) {
	t.Helper()
	dir, write, run = outputFixture(t)
	cmd := `sh -c 'rc=0; [ $# -gt 0 ] || set -- %s/*_check.py; for f; do python3 "$f" || rc=1; done; exit $rc' sh`
	config := func(requires string) string {
		return "version: 1\norphans: ignore\nsuites:\n" +
			"  - name: good\n    runner: custom\n    tests: [\"good/*_check.py\"]\n    command: " + strings.ReplaceAll(cmd, "%s", "good") + "\n" +
			"    requires: [" + requires + "]\n" +
			"  - name: bad\n    runner: custom\n    tests: [\"bad/*_check.py\"]\n    command: " + strings.ReplaceAll(cmd, "%s", "bad") + "\n"
	}
	write("smates.yml", config(""))
	write("good/e_check.py", "import sys\nsys.path.insert(0, 'src')\nimport common\n")
	if code, _, errs := run("run", "--all"); code != 0 {
		t.Fatalf("baseline: %d\n%s", code, errs)
	}
	write("smates.yml", config(requires))
	return dir, write, run
}

type runReport struct {
	Results       map[string]map[string]string `json:"results"`
	NotRun        map[string][]string          `json:"not_run"`
	ExitCode      int                          `json:"exit_code"`
	MaxTime       bool                         `json:"max_time_reached"`
	UnmetRequires map[string][]string          `json:"unmet_requires"`
}

func readReport(t *testing.T, path string) runReport {
	t.Helper()
	var rep runReport
	data, err := os.ReadFile(path)
	if err == nil {
		err = json.Unmarshal(data, &rep)
	}
	if err != nil {
		t.Fatalf("report: %v", err)
	}
	return rep
}

// Units whose requirements are not met do not run, are never recorded (as passed or
// otherwise) and are selected again next time; a check that cannot tell counts as unmet.
func TestRunRequires(t *testing.T) {
	t.Setenv("SMATES_REQ_DB", "")
	cases := []struct {
		name     string
		requires string
		failBad  bool     // a test of the other suite fails too
		code     int      // exit code
		deferred []string // good's units left out
		log      string
	}{
		{"env unset", "{env: SMATES_REQ_DB}", false, ExitEnv, []string{"good/a_check.py", "good/e_check.py"}, "requires env SMATES_REQ_DB ($SMATES_REQ_DB is not set)"},
		{"env unset, other tests fail", "{env: SMATES_REQ_DB}", true, ExitFailed, []string{"good/a_check.py", "good/e_check.py"}, "requirements not met"},
		{"env set", "{env: PATH}", false, 0, nil, ""},
		{"file missing", "{file: .env.test}", false, ExitEnv, []string{"good/a_check.py", "good/e_check.py"}, "no file matches"},
		{"file present", "{file: src/common.py}", false, 0, nil, ""},
		{"cmd fails", "{cmd: 'exit 3'}", false, ExitEnv, []string{"good/a_check.py", "good/e_check.py"}, "exit status 3"},
		{"cmd not found", "{cmd: smates-no-such-check-x9}", false, ExitEnv, []string{"good/a_check.py", "good/e_check.py"}, "exit status 127"},
		{"cmd times out", "{cmd: 'sleep 30', timeout: 200ms}", false, ExitEnv, []string{"good/a_check.py", "good/e_check.py"}, "did not finish within 200ms"},
		{"cmd passes", "{cmd: 'test -f good/a_check.py'}", false, 0, nil, ""},
		{"tcp closed", "{tcp: '127.0.0.1:1', timeout: 200ms}", false, ExitEnv, []string{"good/a_check.py", "good/e_check.py"}, "nothing accepts connections on 127.0.0.1:1"},
		// Only the units the tests globs select are left out.
		{"narrowed to some tests", "{env: SMATES_REQ_DB, tests: ['good/e_*.py']}", false, ExitEnv, []string{"good/e_check.py"}, "1 test unit(s) not run"},
		{"narrowed to no selected test", "{env: SMATES_REQ_DB, tests: ['good/none_*.py']}", false, 0, nil, ""},
		{"one of two unmet", "{env: PATH}, {env: SMATES_REQ_DB}", false, ExitEnv, []string{"good/a_check.py", "good/e_check.py"}, "requires env SMATES_REQ_DB"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir, write, run := requiresFixture(t, tc.requires)
			write("src/common.py", "X = 1  # changed\n")
			if tc.failBad {
				write("bad/c_check.py", "assert False\n")
			}
			report := filepath.Join(dir, "r.json")
			code, _, errs := run("run", "--report", report)
			if code != tc.code || !strings.Contains(errs, tc.log) {
				t.Fatalf("exit %d, want %d (log %q)\n%s", code, tc.code, tc.log, errs)
			}
			rep := readReport(t, report)
			if got := rep.NotRun["good"]; !slices.Equal(sorted(got), tc.deferred) {
				t.Errorf("not run %v, want %v", got, tc.deferred)
			}
			for _, u := range tc.deferred {
				if _, ok := rep.Results["good"][u]; ok {
					t.Errorf("%s has a result: %v", u, rep.Results["good"])
				}
			}
			if (len(tc.deferred) > 0) != (len(rep.UnmetRequires["good"]) > 0) {
				t.Errorf("unmet_requires %v", rep.UnmetRequires)
			}
			// Must keep: the units left out are selected again; what ran is not.
			code, out, errs := run("select", "--format", "files")
			want := slices.Clone(tc.deferred)
			if tc.failBad {
				// The custom runner reports no per-file status: the failing batch's
				// units are all selected again.
				want = append(want, "bad/b_check.py", "bad/c_check.py")
			}
			if got := strings.Fields(out); code != 0 || !slices.Equal(sorted(got), sorted(want)) {
				t.Errorf("selected next time %v, want %v\n%s", got, want, errs)
			}
		})
	}
}

func sorted(s []string) []string {
	s = slices.Clone(s)
	slices.Sort(s)
	return s
}

// Unmet requirements leave the snapshot's view of the suite unchanged: an orphan its
// policy runs the suite for is seen again until the suite has run.
func TestRunRequiresKeepsOrphansUnseen(t *testing.T) {
	t.Setenv("SMATES_REQ_DB", "")
	dir, write, run := requiresFixture(t, "{env: SMATES_REQ_DB}")
	write("smates.yml", strings.Replace(readFile(t, filepath.Join(dir, "smates.yml")), "orphans: ignore", "orphans: all", 1))
	write("Dockerfile", "FROM scratch\n")
	if code, _, errs := run("run", "--no-agent"); code != ExitEnv {
		t.Fatalf("run: exit %d\n%s", code, errs)
	}
	code, out, errs := run("select", "--no-agent", "--format", "files")
	if code != 0 || !strings.Contains(out, "good/a_check.py") || !strings.Contains(out, "good/e_check.py") {
		t.Errorf("the orphan must still select good: %d %q\n%s", code, out, errs)
	}
	t.Setenv("SMATES_REQ_DB", "postgres://localhost/test")
	if code, _, errs := run("run", "--no-agent"); code != 0 {
		t.Fatalf("run with the requirement met: exit %d\n%s", code, errs)
	}
	if code, out, errs := run("select", "--no-agent", "--format", "files"); code != 0 || out != "" {
		t.Errorf("after the run: %d %q\n%s", code, out, errs)
	}
}

// --max-time stops starting batches; what did not start stays selected, nothing is
// recorded for it, and the run exits 0 when what ran passed.
func TestRunMaxTime(t *testing.T) {
	dir, write, run := outputFixture(t)
	write("smates.yml", customSuites("ignore", "", "good", "bad"))
	if code, _, errs := run("run", "--all"); code != 0 {
		t.Fatalf("baseline: %d\n%s", code, errs)
	}
	write("src/common.py", "X = 1  # changed\n") // selects good/a_check.py and bad/b_check.py
	report := filepath.Join(dir, "r.json")
	md := filepath.Join(dir, "summary.md")
	code, _, errs := run("run", "--max-time", "1ns", "--report", report, "--markdown", md)
	if code != 0 || !strings.Contains(errs, "--max-time 1ns: starting no further test batch (1 selected unit(s) left") ||
		!strings.Contains(errs, "--max-time left 1 selected unit(s) not started") {
		t.Fatalf("exit %d\n%s", code, errs)
	}
	rep := readReport(t, report)
	if !rep.MaxTime || len(rep.Results) != 1 || countUnits(rep.NotRun) != 1 || len(rep.UnmetRequires) != 0 {
		t.Errorf("report %+v", rep)
	}
	if s, _ := os.ReadFile(md); !strings.Contains(string(s), "**Time limit:** 1 selected test unit not started within --max-time 1ns") {
		t.Errorf("summary:\n%s", s)
	}
	var left string
	for s, u := range rep.NotRun {
		left = s + "/" + strings.TrimPrefix(u[0], s+"/")
	}
	// Must keep: the unit left out is selected again; the one that ran is not.
	code, out, errs := run("select", "--format", "files")
	if code != 0 || strings.TrimSpace(out) != left {
		t.Errorf("selected next time %q, want %q\n%s", out, left, errs)
	}
	if code, _, errs := run("run"); code != 0 || !strings.Contains(errs, "ran 1 of") {
		t.Errorf("next run: %d\n%s", code, errs)
	}
	if code, out, _ := run("select", "--format", "files"); code != 0 || out != "" {
		t.Errorf("after both runs: %d %q", code, out)
	}
}

// A failing batch within --max-time still fails the run, and --output failures says
// what the limit left out.
func TestRunMaxTimeFailures(t *testing.T) {
	_, write, run := outputFixture(t)
	write("smates.yml", customSuites("ignore", "", "bad", "good"))
	if code, _, errs := run("run", "--all"); code != 0 {
		t.Fatalf("baseline: %d\n%s", code, errs)
	}
	write("src/common.py", "X = 2\n") // bad/b_check.py fails
	code, _, errs := run("run", "--max-time", "1ns", "--output", "failures")
	if code != ExitFailed || !strings.Contains(errs, "(--max-time left 1 selected test unit(s) not started)") {
		t.Errorf("exit %d\n%s", code, errs)
	}
	write("src/common.py", "X = 1  # changed\n")
	code, _, errs = run("run", "--max-time", "1ns", "--output", "failures")
	if code != 0 || !strings.Contains(errs, "smates: PASSED") || !strings.Contains(errs, "--max-time left") {
		t.Errorf("passing: exit %d\n%s", code, errs)
	}
}

// The coding agent hooks do not block on units left for later: one line on stderr,
// exit 0, and the units stay selected. Failing tests still block, and another
// environment error is still reported.
func TestAgentStopHookDefers(t *testing.T) {
	t.Setenv("SMATES_REQ_DB", "")
	for _, a := range []agentHook{claudeHook, codexHook} {
		t.Run(a.name, func(t *testing.T) {
			dir, write, run := requiresFixture(t, "{env: SMATES_REQ_DB}")
			sub := strings.TrimPrefix(a.marker, "smates hook ")
			stop := func(args ...string) (int, string, string) {
				t.Helper()
				in, _ := json.Marshal(map[string]any{"hook_event_name": "Stop", "stop_hook_active": false, "cwd": dir})
				var out, errb bytes.Buffer
				code, err := cmdHook(context.Background(), append([]string{sub, "--", "--no-agent"}, args...), bytes.NewReader(in), &out, &errb)
				if err != nil {
					t.Fatalf("%s: %v", sub, err)
				}
				return code, out.String(), errb.String()
			}
			write("src/common.py", "X = 1  # changed\n")
			code, out, errs := stop()
			if code != 0 || out != "" || strings.Count(errs, "\n") != 1 ||
				!strings.Contains(errs, "smates: selected test units left for a later run (they stay selected): 2 of good, which needs env SMATES_REQ_DB") {
				t.Fatalf("unmet requires: %d %q %q", code, out, errs)
			}
			if code, sel, _ := run("select", "--format", "files"); code != 0 || !strings.Contains(sel, "good/a_check.py") {
				t.Errorf("must keep: %q", sel)
			}
			// --max-time among the run flags wins over the hook's default.
			write("src/common.py", "X = 1  # changed again\n")
			t.Setenv("SMATES_REQ_DB", "x")
			code, out, errs = stop("--max-time", "1ns")
			if code != 0 || out != "" || !strings.Contains(errs, "by --max-time 1ns") {
				t.Errorf("--max-time: %d %q %q", code, out, errs)
			}
			t.Setenv("SMATES_REQ_DB", "")
			// Failing tests still block, with the units left out in the reason.
			write("src/common.py", "X = 2\n")
			code, out, _ = stop()
			var dec struct{ Decision, Reason string }
			if code != 0 || json.Unmarshal([]byte(out), &dec) != nil || dec.Decision != "block" || !strings.Contains(dec.Reason, "requires env SMATES_REQ_DB") {
				t.Errorf("failing: %d %q", code, out)
			}
			// A runner that cannot start is still an error shown to the user.
			write("src/common.py", "X = 1\n")
			write("pt/test_x.py", "def test_x():\n    pass\n")
			write("smates.yml", readFile(t, filepath.Join(dir, "smates.yml"))+
				"  - name: pt\n    runner: pytest\n    tests: [\"pt/test_*.py\"]\n    command: sh -c 'smates-no-such-pytest-x8' sh\n")
			code, out, errs = stop()
			if code != 1 || out != "" || !strings.Contains(errs, a.name+" Stop hook): pt: the runner exited with code 127") ||
				!strings.Contains(errs, "requirements not met") {
				t.Errorf("runner error: %d %q %q", code, out, errs)
			}
		})
	}
}

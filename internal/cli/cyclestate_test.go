package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/dotneet/smates/internal/state"
)

// pySuite is a custom suite of outputFixture's kind: python3 runs each *_check.py of
// directory name. extra is appended to its YAML entry (requires).
func pySuite(name, extra string) string {
	cmd := `sh -c 'rc=0; [ $# -gt 0 ] || set -- ` + name + `/*_check.py; for f; do python3 "$f" || rc=1; done; exit $rc' sh`
	return "  - name: " + name + "\n    runner: custom\n    tests: [\"" + name + "/*_check.py\"]\n    command: " + cmd + "\n" + extra
}

// captureCycle makes the next cycles that run batches hand themselves to the returned
// slot once they ended.
func captureCycle(t *testing.T) **cycle {
	t.Helper()
	var got *cycle
	cycleDone = func(c *cycle) { got = c }
	t.Cleanup(func() { cycleDone = nil })
	return &got
}

// cycleReport is what TestCycleOutcomesAgree reads from --report.
type cycleReport struct {
	Results   map[string]map[string]string `json:"results"`
	NotRun    map[string][]string          `json:"not_run"`
	StoppedBy string                       `json:"stopped_by"`
	ExitCode  int                          `json:"exit_code"`
}

func readCycleReport(t *testing.T, path string) cycleReport {
	t.Helper()
	var rep cycleReport
	data, err := os.ReadFile(path)
	if err == nil {
		err = json.Unmarshal(data, &rep)
	}
	if err != nil {
		t.Fatalf("report: %v", err)
	}
	return rep
}

// TestCycleOutcomesAgree: across the main flag combinations, every unit a run planned
// ends in exactly one final state (ran, not run, not started, unmet), and the exit
// code, --report, the GitHub outputs and the run record agree with those states.
func TestCycleOutcomesAgree(t *testing.T) {
	const (
		pt     = "  - name: pt\n    runner: pytest\n    tests: [\"pt/test_*.py\"]\n    command: sh -c 'smates-no-such-pytest-x6' sh\n"
		broken = "  - name: broken\n    runner: custom\n    tests: [\"broken/*_check.py\"]\n    command: \"sh -c 'exit 0\"\n"
	)
	unmet := "    requires: [{file: missing-file-for-the-test}]\n"
	for _, tc := range []struct {
		name   string
		suites string
		args   []string
		stop   stopReason
		// states the run must end with at least one unit in.
		want []unitState
	}{
		{name: "full run", suites: pySuite("good", "") + pySuite("bad", ""), args: []string{"--all"}, want: []unitState{unitRan}},
		{name: "fail-fast", suites: pySuite("bad", "") + pySuite("good", ""), args: []string{"--all", "--fail-fast"}, stop: stopFailFast, want: []unitState{unitRan, unitNotRun}},
		{name: "max-time", suites: pySuite("bad", "") + pySuite("good", ""), args: []string{"--all", "--max-time", "1ms"}, stop: stopMaxTime, want: []unitState{unitRan, unitNotRun}},
		{name: "shard 1/2", suites: pySuite("good", "") + pySuite("bad", ""), args: []string{"--all", "--shard", "1/2"}},
		{name: "shard 2/2", suites: pySuite("good", "") + pySuite("bad", ""), args: []string{"--all", "--shard", "2/2"}},
		{name: "runner exits 127", suites: pySuite("good", "") + pt, args: []string{"--all"}, want: []unitState{unitRan, unitUnstarted}},
		{name: "requires unmet", suites: pySuite("good", unmet) + pySuite("bad", ""), args: []string{"--all"}, want: []unitState{unitRan, unitUnmet}},
		{name: "configuration error", suites: pySuite("good", "") + broken + pySuite("bad", ""), args: []string{"--all"}, stop: stopConfig,
			want: []unitState{unitRan, unitUnstarted, unitNotRun}},
		{name: "failures output", suites: pySuite("bad", "") + pySuite("good", ""), args: []string{"--all", "--fail-fast", "--output", "failures"}, stop: stopFailFast},
		{name: "runner arguments", suites: pySuite("good", "") + pySuite("bad", ""), args: []string{"--all", "--", "good/a_check.py"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir, write, run := outputFixture(t)
			write("smates.yml", "version: 1\norphans: ignore\nsuites:\n"+tc.suites)
			write("pt/test_x.py", "def test_x():\n    pass\n")
			write("broken/x_check.py", "print('x')\n")
			write("bad/b_check.py", "assert False\n")
			outputs := filepath.Join(t.TempDir(), "github_output")
			t.Setenv("GITHUB_OUTPUT", outputs)
			report := filepath.Join(t.TempDir(), "r.json")
			got := captureCycle(t)
			code, _, errs := run(append([]string{"run", "--report", report}, tc.args...)...)
			c := *got
			if c == nil {
				t.Fatalf("no cycle ran: %d\n%s", code, errs)
			}
			// Every planned unit ended in one final state.
			counts := map[unitState]int{}
			for suite, units := range c.units.order {
				for _, u := range units {
					ur := c.units.lookup(suite, u)
					if !ur.state.final() {
						t.Errorf("%s: %s ended in state %d", suite, u, ur.state)
					}
					counts[ur.state]++
				}
			}
			if n := counts[unitRan] + counts[unitNotRun] + counts[unitUnstarted] + counts[unitUnmet]; n < c.total {
				t.Errorf("%d unit(s) in a final state, %d planned", n, c.total)
			}
			for _, st := range tc.want {
				if counts[st] == 0 {
					t.Errorf("no unit in state %d: %v\n%s", st, counts, errs)
				}
			}
			if c.stop != tc.stop {
				t.Errorf("stopped by %q, want %q", c.stop, tc.stop)
			}
			// The exit code, --report and the GitHub outputs agree.
			rep := readCycleReport(t, report)
			if rep.ExitCode != code || c.exit != code {
				t.Errorf("exit %d, report %d, cycle %d\n%s", code, rep.ExitCode, c.exit, errs)
			}
			data, _ := os.ReadFile(outputs)
			if want := "exit-code=" + strconv.Itoa(code); !strings.Contains(string(data), want+"\n") {
				t.Errorf("GitHub outputs lack %s:\n%s", want, data)
			}
			if rep.StoppedBy != tc.stop.String() {
				t.Errorf("report stopped_by %q, want %q", rep.StoppedBy, tc.stop.String())
			}
			notRun := 0
			for suite, units := range rep.NotRun {
				for _, u := range units {
					notRun++
					if ur := c.units.lookup(suite, u); ur == nil || !ur.state.notStarted() {
						t.Errorf("report: %s %s not run, state %+v", suite, u, ur)
					}
				}
			}
			if notRun != c.notStartedUnits() {
				t.Errorf("report: %d not run, cycle %d", notRun, c.notStartedUnits())
			}
			for suite, units := range rep.Results {
				for u, status := range units {
					if c.units.status(suite, u) != status {
						t.Errorf("report: %s %s %s, cycle %q", suite, u, status, c.units.status(suite, u))
					}
				}
			}
			if counts[unitUnstarted]+counts[unitUnmet] > 0 && code == 0 {
				t.Errorf("units not started, exit 0:\n%s", errs)
			}
			if c.stop == stopConfig && strings.Contains(errs, "--fail-fast") {
				t.Errorf("a configuration error is reported as --fail-fast:\n%s", errs)
			}
			// The run record agrees.
			if c.record {
				st, err := state.Load(filepath.Join(dir, ".smates", "state.json.gz"))
				if err != nil || len(st.Runs) == 0 {
					t.Fatalf("state: %v", err)
				}
				r := st.Runs[len(st.Runs)-1]
				if r.ExitCode != code || r.NotRun != c.notStartedUnits() || r.Failed != c.units.countStatus(state.Fail) {
					t.Errorf("run record %+v; exit %d, not run %d", r, code, c.notStartedUnits())
				}
			}
		})
	}
}

// TestFullRunRecordsOnlyWhatRan: a full run replaces a suite's entries and advances the
// snapshot (Files, Commit, Created) only when every test ran unfiltered. Must keep:
// with runner arguments (they may filter tests), the entries of units they did not run
// survive and the snapshot stays; a runner exiting 127 before any test reported leaves
// the snapshot's Commit and Created as they were; a unit that ran but left no
// observation keeps its entry.
func TestFullRunRecordsOnlyWhatRan(t *testing.T) {
	stateOf := func(t *testing.T, dir string) *state.State {
		t.Helper()
		st, err := state.Load(filepath.Join(dir, ".smates", "state.json.gz"))
		if err != nil {
			t.Fatal(err)
		}
		return st
	}
	baseline := func(t *testing.T, suites string) (string, func(string, string), func(...string) (int, string, string), *state.State) {
		t.Helper()
		dir, write, run := outputFixture(t)
		write("smates.yml", "version: 1\norphans: ignore\nsuites:\n"+suites)
		write("bad/b_check.py", "print('ok')\n")
		if code, _, errs := run("run", "--all"); code != 0 {
			t.Fatalf("baseline: %d\n%s", code, errs)
		}
		st := stateOf(t, dir)
		if st.Commit == "" || st.Suites["good"].Tests["good/a_check.py"] == nil {
			t.Fatalf("baseline state: %+v", st)
		}
		return dir, write, run, st
	}
	commitAll := func(t *testing.T, dir string) {
		t.Helper()
		g := &gitRepo{t: t, dir: dir}
		g.git("add", "-A")
		g.commit("change")
	}

	t.Run("runner arguments", func(t *testing.T) {
		dir, write, run, before := baseline(t, pySuite("good", "")+pySuite("bad", ""))
		write("good/z_check.py", "print('z')\n")
		commitAll(t, dir)
		if code, _, errs := run("run", "--all", "--", "bad/c_check.py"); code != 0 {
			t.Fatalf("run: %d\n%s", code, errs)
		}
		st := stateOf(t, dir)
		if st.Commit != before.Commit || !st.Created.Equal(before.Created) {
			t.Errorf("the snapshot advanced: %s %s, was %s %s", st.Commit, st.Created, before.Commit, before.Created)
		}
		for _, u := range []string{"good/a_check.py", "bad/b_check.py"} {
			suite := strings.Split(u, "/")[0]
			if st.Suites[suite].Tests[u] == nil {
				t.Errorf("the entry of %s was dropped", u)
			}
		}
	})

	t.Run("runner exits 127", func(t *testing.T) {
		pt := "  - name: pt\n    runner: pytest\n    tests: [\"pt/test_*.py\"]\n    command: sh -c 'smates-no-such-pytest-x6' sh\n"
		dir, write, run, before := baseline(t, pySuite("good", "")+pySuite("bad", ""))
		write("smates.yml", "version: 1\norphans: ignore\nsuites:\n"+pySuite("good", "")+pySuite("bad", "")+pt)
		write("pt/test_x.py", "def test_x():\n    pass\n")
		commitAll(t, dir)
		if code, _, errs := run("run", "--all"); code != ExitEnv {
			t.Fatalf("run: exit %d, want %d\n%s", code, ExitEnv, errs)
		}
		st := stateOf(t, dir)
		if st.Commit != before.Commit || !st.Created.Equal(before.Created) {
			t.Errorf("Commit and Created advanced although pt never started: %s %s, was %s %s", st.Commit, st.Created, before.Commit, before.Created)
		}
	})

	t.Run("unit without an observation", func(t *testing.T) {
		dir, write, run, before := baseline(t, pySuite("good", "")+pySuite("bad", ""))
		old := before.Suites["good"].Tests["good/a_check.py"]
		// The suite's command now passes over a_check.py: the batch passes and leaves
		// nothing about it.
		skip := "  - name: good\n    runner: custom\n    tests: [\"good/*_check.py\"]\n" +
			"    command: sh -c 'for f in good/*_check.py; do case $f in *a_check.py) ;; *) python3 \"$f\" || exit 1;; esac; done' sh\n"
		write("good/x_check.py", "print('x')\n")
		write("smates.yml", "version: 1\norphans: ignore\nsuites:\n"+skip+pySuite("bad", ""))
		commitAll(t, dir)
		if code, _, errs := run("run", "--all"); code != 0 {
			t.Fatalf("run: %d\n%s", code, errs)
		}
		st := stateOf(t, dir)
		e := st.Suites["good"].Tests["good/a_check.py"]
		if e == nil {
			t.Fatal("the entry of a unit that left no observation was dropped")
		}
		if e.Status != old.Status || !e.At.Equal(old.At) {
			t.Errorf("entry %+v, want the old one %+v", e, old)
		}
		if st.Suites["good"].Tests["good/x_check.py"] == nil {
			t.Error("the unit that ran is not recorded")
		}
		if st.Commit == before.Commit {
			t.Errorf("a full run in which every test ran must advance the snapshot")
		}
	})
}

// TestConfigErrorStopReason: a run stopped by a suite command that cannot be parsed
// says so, not that --fail-fast stopped it.
func TestConfigErrorStopReason(t *testing.T) {
	_, write, run := outputFixture(t)
	broken := "  - name: broken\n    runner: custom\n    tests: [\"broken/*_check.py\"]\n    command: \"sh -c 'exit 0\"\n"
	write("smates.yml", "version: 1\norphans: ignore\nsuites:\n"+pySuite("good", "")+broken+pySuite("bad", ""))
	write("broken/x_check.py", "print('x')\n")
	for _, output := range []string{"full", "failures"} {
		md := filepath.Join(t.TempDir(), "s.md")
		code, _, errs := run("run", "--all", "--output", output, "--markdown", md)
		if code != ExitConfig {
			t.Fatalf("%s: exit %d, want %d\n%s", output, code, ExitConfig, errs)
		}
		summary, _ := os.ReadFile(md)
		if strings.Contains(errs, "--fail-fast") || strings.Contains(string(summary), "Fail fast") {
			t.Errorf("%s: reported as --fail-fast:\n%s\n%s", output, errs, summary)
		}
		if !strings.Contains(errs, "a configuration error") || !strings.Contains(string(summary), "configuration error") {
			t.Errorf("%s: the configuration error is not named as the reason:\n%s\n%s", output, errs, summary)
		}
	}
}

// TestFailureHintsHasherApart: the failure hints of a failing batch hash what they
// compare on a hasher of their own, so what Record hashes after the last batch does not
// depend on whether an earlier batch failed.
func TestFailureHintsHasherApart(t *testing.T) {
	_, write, run := outputFixture(t)
	write("smates.yml", "version: 1\norphans: ignore\nsuites:\n"+pySuite("bad", "")+pySuite("good", ""))
	write("bad/b_check.py", "assert False\n")
	got := captureCycle(t)
	if code, _, errs := run("run", "--all"); code != ExitFailed {
		t.Fatalf("exit %d\n%s", code, errs)
	}
	c := *got
	if c == nil || c.failures == nil || c.failures.hasher == nil || c.failures.hasher == c.hasher {
		t.Fatal("failure hints share the recording hasher")
	}
	// Hashing through the hints' hasher fills none of the recording hasher's caches: a
	// file outside the snapshot is read again by the recording hasher.
	ignored := filepath.Join(c.p.repo.Root, "probe.local")
	if err := os.WriteFile(ignored, []byte("one"), 0o644); err != nil {
		t.Fatal(err)
	}
	first := c.failures.hasher.Hash("probe.local")
	if err := os.WriteFile(ignored, []byte("two"), 0o644); err != nil {
		t.Fatal(err)
	}
	if second := c.hasher.Hash("probe.local"); second == first || second == "" {
		t.Errorf("the recording hasher reused the hints' read: %q %q", first, second)
	}
}

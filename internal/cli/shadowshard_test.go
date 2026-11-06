package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"testing"

	"github.com/dotneet/smates/internal/runner"
	"github.com/dotneet/smates/internal/selector"
	"github.com/dotneet/smates/internal/state"
)

func TestShadowShardFlags(t *testing.T) {
	f, err := parseRunFlags([]string{"--shadow", "--shard", "2/3"}, &bytes.Buffer{})
	if err != nil || !f.shadow || f.shardI != 2 || f.shardN != 3 {
		t.Fatalf("--shadow --shard: %+v %v", f, err)
	}
}

// TestShadowShardUnits: a sharded shadow run runs every unit its shard owns, selected or
// not, so that the shards together run every test exactly once; a sharded run without
// --shadow still runs only its share of the selection.
func TestShadowShardUnits(t *testing.T) {
	for _, n := range []int{2, 3, 5} {
		for _, shadow := range []bool{true, false} {
			runs := map[string]int{}
			for i := 1; i <= n; i++ {
				res := shardResult()
				applyShard(res, i, n)
				c := &cycle{f: &runFlags{shadow: shadow, shardI: i, shardN: n}, sel: &selection{res: res}}
				for _, sr := range res.Suites {
					units, full := c.units(sr)
					if full {
						t.Errorf("n=%d shadow=%t: shard %d runs %s as a whole suite", n, shadow, i, sr.Name)
					}
					for _, u := range units {
						runs[sr.Name+":"+u]++
						if tt := testByID(sr, u); tt.Shard != i {
							t.Errorf("n=%d shadow=%t: shard %d runs %s, owned by %d", n, shadow, i, u, tt.Shard)
						}
					}
				}
			}
			for _, s := range shardResult().Suites {
				for _, tt := range s.Tests {
					want := 1
					if !shadow && !tt.Selected {
						want = 0
					}
					if got := runs[s.Name+":"+tt.ID]; got != want {
						t.Errorf("n=%d shadow=%t: %s ran %d time(s), want %d", n, shadow, tt.ID, got, want)
					}
				}
			}
		}
	}
}

func testByID(sr *selector.Suite, id string) *selector.Test {
	for _, t := range sr.Tests {
		if t.ID == id {
			return t
		}
	}
	return nil
}

// TestCheckShadowSharded: a sharded shadow run reports as escapes only the failing
// units its own shard owns that the whole run did not select.
func TestCheckShadowSharded(t *testing.T) {
	type unit struct {
		id          string
		selected    bool // before sharding
		owner       int
		status      string // "" when the runner said nothing about it
		inferred    bool
		buildFailed bool
	}
	fn := func(name string) string { return runner.FuncUnit("pkg", name) }
	build := runner.FuncUnit("pkg", runner.BuildCheck)
	cases := []struct {
		name     string
		shard    [2]int
		units    []unit
		escapes  []string
		possible []string
	}{
		{
			name:    "owned unselected failure is an escape",
			shard:   [2]int{1, 2},
			units:   []unit{{id: "a", owner: 1, status: state.Fail}},
			escapes: []string{"a"},
		},
		{
			name:  "owned selected failure is no escape",
			shard: [2]int{1, 2},
			units: []unit{{id: "a", selected: true, owner: 1, status: state.Fail}},
		},
		{
			name:     "owned inferred failure is a possible escape",
			shard:    [2]int{1, 2},
			units:    []unit{{id: "a", owner: 1, status: state.Fail, inferred: true}},
			possible: []string{"a"},
		},
		{
			// Must keep: applyShard deselected it here, but the run selected it.
			name:  "failure of a unit another shard owns and the run selected",
			shard: [2]int{1, 2},
			units: []unit{{id: "b", selected: true, owner: 2, status: state.Fail}},
		},
		{
			// Must keep: its owner runs it and reports it; counting it here too would
			// count it twice.
			name:  "failure of an unselected unit another shard owns",
			shard: [2]int{1, 2},
			units: []unit{{id: "b", owner: 2, status: state.Fail}},
		},
		{
			name:  "owned unit that passed",
			shard: [2]int{2, 2},
			units: []unit{{id: "a", owner: 2, status: state.Pass}},
		},
		{
			name:  "function of a package whose selected check failed here",
			shard: [2]int{1, 2},
			units: []unit{{id: build, selected: true, owner: 1, status: state.Fail}, {id: fn("TestA"), owner: 1, status: state.Fail}},
		},
		{
			// It ran and failed on its own: the package built here.
			name:    "function failing an assertion, its package's selected check in another shard",
			shard:   [2]int{1, 2},
			units:   []unit{{id: build, selected: true, owner: 2}, {id: fn("TestA"), owner: 1, status: state.Fail}},
			escapes: []string{fn("TestA")},
		},
		{
			// The package did not build here either, which the check's shard reports.
			name:     "function of a package that did not build, its selected check in another shard",
			shard:    [2]int{1, 2},
			units:    []unit{{id: build, selected: true, owner: 2}, {id: fn("TestA"), owner: 1, status: state.Fail, buildFailed: true}},
			possible: []string{fn("TestA")},
		},
		{
			name:    "function of a package whose unselected check another shard owns",
			shard:   [2]int{1, 2},
			units:   []unit{{id: build, owner: 2}, {id: fn("TestA"), owner: 1, status: state.Fail}},
			escapes: []string{fn("TestA")},
		},
		{
			name:    "unsharded shadow run",
			units:   []unit{{id: "a", status: state.Fail}, {id: "b", selected: true, status: state.Fail}},
			escapes: []string{"a"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			sr := &selector.Suite{Name: "s"}
			obs := map[string]*runner.Observation{}
			for _, u := range c.units {
				sr.Tests = append(sr.Tests, &selector.Test{ID: u.id, Selected: u.selected, Shard: u.owner})
				if u.status != "" {
					obs[u.id] = &runner.Observation{Status: u.status, Inferred: u.inferred, BuildFailed: u.buildFailed}
				}
			}
			res := &selector.Result{Suites: []*selector.Suite{sr}}
			cy := &cycle{f: &runFlags{shadow: true, shardI: c.shard[0], shardN: c.shard[1]}, sel: &selection{res: res}}
			if c.shard[1] > 1 {
				cy.preShard = selectedUnits(res)
				for _, tt := range sr.Tests { // as applyShard leaves it
					if tt.Shard != c.shard[0] {
						tt.Selected = false
					}
				}
			}
			cy.checkShadow(sr, obs, map[string]bool{})
			prefix := func(ids []string) []string {
				var out []string
				for _, id := range ids {
					out = append(out, "s: "+id)
				}
				return out
			}
			if !reflect.DeepEqual(cy.escapes, prefix(c.escapes)) || !reflect.DeepEqual(cy.possible, prefix(c.possible)) {
				t.Errorf("escapes %v possible %v, want %v %v", cy.escapes, cy.possible, c.escapes, c.possible)
			}
		})
	}
}

// TestShadowShardEndToEnd: `run --shadow --shard I/N` runs every test of its shard, and
// across the shards each failing test the selection missed is reported exactly once,
// by the shard owning it; a selected failing test is reported by none.
func TestShadowShardEndToEnd(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses sh")
	}
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not on PATH")
	}
	clearCI(t)
	t.Setenv("GITHUB_OUTPUT", "")
	t.Setenv("GITHUB_STEP_SUMMARY", "")
	noAgentCredentials(t)
	g := newGitRepo(t)
	write := func(rel, content string) {
		p := filepath.Join(g.dir, filepath.FromSlash(rel))
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("smates.yml", `version: 1
orphans: ignore
suites:
  - name: py
    runner: custom
    tests: ["tests/*_check.py"]
    junit: junit.xml
    command: sh -c 'rc=0; echo "<testsuite>" > junit.xml; for f; do if python3 "$f"; then echo "<testcase file=\"$f\" name=\"t\"/>"; else rc=1; echo "<testcase file=\"$f\" name=\"t\"><failure message=\"boom\"/></testcase>"; fi >> junit.xml; done; echo "</testsuite>" >> junit.xml; exit $rc' sh
`)
	write(".gitignore", ".smates/\njunit.xml\n")
	var all []string
	for i := range 8 {
		id := fmt.Sprintf("tests/u%d_check.py", i)
		all = append(all, id)
		write(id, "assert True\n")
	}
	// u1 depends on an environment variable, which tracing cannot see.
	write("tests/u1_check.py", "import os\nassert os.environ.get(\"SMATES_TEST_MODE\") != \"broken\"\n")
	g.git("add", "-A")
	g.commit("fixture")
	run := func(args ...string) (int, string) {
		var out, errb bytes.Buffer
		code := Main(append(args[:1:1], append([]string{"-C", g.dir, "--no-agent"}, args[1:]...)...), &out, &errb)
		return code, errb.String()
	}
	if code, errs := run("run", "--all"); code != 0 {
		t.Fatalf("baseline: %d\n%s", code, errs)
	}
	// u5 changes and fails: selected, so no escape anywhere.
	write("tests/u5_check.py", "assert False\n")
	t.Setenv("SMATES_TEST_MODE", "broken")

	const n = 3
	ran := map[string]int{}
	var escapes, possible []string
	for i := 1; i <= n; i++ {
		report := filepath.Join(t.TempDir(), "report.json")
		code, errs := run("run", "--shadow", "--no-record", "--shard", fmt.Sprintf("%d/%d", i, n), "--report", report)
		data, err := os.ReadFile(report)
		if err != nil {
			t.Fatalf("shard %d: exit %d, no report: %v\n%s", i, code, err, errs)
		}
		var rep struct {
			Escapes  []string                     `json:"escapes"`
			Possible []string                     `json:"possible_escapes"`
			Results  map[string]map[string]string `json:"results"`
		}
		if err := json.Unmarshal(data, &rep); err != nil {
			t.Fatal(err)
		}
		for u, status := range rep.Results["py"] {
			ran[u]++
			if failing := u == "tests/u1_check.py" || u == "tests/u5_check.py"; failing != (status == state.Fail) {
				t.Errorf("shard %d: %s %s", i, u, status)
			}
		}
		escapes = append(escapes, rep.Escapes...)
		possible = append(possible, rep.Possible...)
	}
	for _, id := range all {
		if ran[id] != 1 {
			t.Errorf("%s ran in %d shard(s), want 1 (%v)", id, ran[id], ran)
		}
	}
	if !reflect.DeepEqual(escapes, []string{"py: tests/u1_check.py"}) || len(possible) != 0 {
		t.Errorf("escapes across shards %v, possible %v; want only u1, once", escapes, possible)
	}
}

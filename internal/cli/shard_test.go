package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/dotneet/smates/internal/selector"
)

func TestShardCount(t *testing.T) {
	type unit struct {
		sel bool
		ms  int64
	}
	suite := func(name string, units ...unit) *selector.Suite {
		s := &selector.Suite{Name: name}
		for i, u := range units {
			s.Tests = append(s.Tests, &selector.Test{ID: fmt.Sprintf("%s/t%d", name, i), Selected: u.sel, Duration: u.ms})
		}
		return s
	}
	minute := int64(time.Minute / time.Millisecond)
	cases := []struct {
		name   string
		suites []*selector.Suite
		max    int
		target time.Duration
		want   int
	}{
		{"nothing selected", []*selector.Suite{suite("a", unit{false, 5 * minute}, unit{false, 0})}, 8, 10 * time.Minute, 0},
		{"no suites", nil, 8, 10 * time.Minute, 0},
		{"under one target", []*selector.Suite{suite("a", unit{true, 3 * minute}, unit{true, 2 * minute})}, 8, 10 * time.Minute, 1},
		{"exactly one target", []*selector.Suite{suite("a", unit{true, 10 * minute})}, 8, 10 * time.Minute, 1},
		{"just over one target", []*selector.Suite{suite("a", unit{true, 10*minute + 1})}, 8, 10 * time.Minute, 2},
		{"two and a half targets", []*selector.Suite{suite("a", unit{true, 15 * minute}, unit{true, 10 * minute}, unit{false, 60 * minute})}, 8, 10 * time.Minute, 3},
		{"capped at max", []*selector.Suite{suite("a", unit{true, 500 * minute})}, 4, 10 * time.Minute, 4},
		{"max one", []*selector.Suite{suite("a", unit{true, 500 * minute})}, 1, 10 * time.Minute, 1},
		{"across suites", []*selector.Suite{suite("a", unit{true, 6 * minute}), suite("b", unit{true, 6 * minute})}, 8, 10 * time.Minute, 2},
		{
			// Unrecorded units count as the suite's median (here 10 min, also from
			// unselected units): 2 × 10 min.
			name:   "unrecorded units weigh the median",
			suites: []*selector.Suite{suite("a", unit{true, 0}, unit{true, 0}, unit{false, 10 * minute}, unit{false, 10 * minute}, unit{false, 1 * minute})},
			max:    8, target: 10 * time.Minute, want: 2,
		},
		{
			// Must keep: nothing tells how long a suite's selected units take, so the
			// count is the most allowed, never sized as if they took no time.
			name:   "suite without any recorded duration",
			suites: []*selector.Suite{suite("a", unit{true, 1 * minute}), suite("b", unit{true, 0}, unit{false, 0})},
			max:    6, target: 10 * time.Minute, want: 6,
		},
		{
			// A suite without durations that selected nothing does not force the maximum.
			name:   "unrecorded suite with nothing selected",
			suites: []*selector.Suite{suite("a", unit{true, 1 * minute}), suite("b", unit{false, 0})},
			max:    6, target: 10 * time.Minute, want: 1,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			res := &selector.Result{Suites: c.suites}
			if got := shardCount(res, c.max, c.target); got != c.want {
				t.Errorf("shardCount = %d, want %d", got, c.want)
			}
		})
	}
}

func TestShardOutputs(t *testing.T) {
	cases := []struct {
		n      int
		matrix string
	}{
		{0, `{"shard":[]}`},
		{1, `{"shard":["1/1"]}`},
		{3, `{"shard":["1/3","2/3","3/3"]}`},
	}
	for _, c := range cases {
		out := outputMap(t, shardOutputs(c.n))
		if out["shards"] != fmt.Sprint(c.n) || out["shard-matrix"] != c.matrix {
			t.Errorf("n=%d: %v", c.n, out)
		}
		// Every matrix value is a valid --shard of the same count.
		var m map[string][]string
		if err := json.Unmarshal([]byte(out["shard-matrix"]), &m); err != nil {
			t.Fatal(err)
		}
		for _, s := range m["shard"] {
			if _, n, err := parseShard(s); err != nil || n != c.n {
				t.Errorf("n=%d: matrix value %q: %d %v", c.n, s, n, err)
			}
		}
	}
}

// TestSelectShardFlagErrors: invalid --max-shards / --shard-time combinations are usage
// errors (exit 2), found before anything is loaded.
func TestSelectShardFlagErrors(t *testing.T) {
	cases := [][]string{
		{"--max-shards", "0"},
		{"--max-shards", "-1"},
		{"--max-shards", "257"},
		{"--max-shards", "4", "--shard", "1/2"},
		{"--shard-time", "5m"},
		{"--max-shards", "4", "--shard-time", "0s"},
		{"--max-shards", "4", "--shard-time", "500ms"},
		{"--max-shards", "x"},
	}
	for _, args := range cases {
		var out, errb bytes.Buffer
		dir := t.TempDir() // not a repository: validation must come first
		if code := Main(append([]string{"select", "-C", dir}, args...), &out, &errb); code != ExitUsage {
			t.Errorf("%v: exit %d, want %d\n%s", args, code, ExitUsage, errb.String())
		}
	}
}

// TestSelectMaxShardsEndToEnd: `select --max-shards` writes shards and shard-matrix,
// 0 when nothing is selected, and sizes for a full run where `smates run` runs every
// test (a push build of the default branch on CI).
func TestSelectMaxShardsEndToEnd(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses sh")
	}
	clearCI(t)
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
  - name: sh
    runner: custom
    tests: ["tests/*_check.sh"]
    command: sh -c 'for f; do sh "$f" || exit 1; done' sh
`)
	write(".gitignore", ".smates/\n")
	for i := range 4 {
		write(fmt.Sprintf("tests/u%d_check.sh", i), "exit 0\n")
	}
	g.git("add", "-A")
	g.commit("fixture")
	run := func(args ...string) (int, string) {
		var out, errb bytes.Buffer
		code := Main(append(args[:1:1], append([]string{"-C", g.dir, "--no-agent"}, args[1:]...)...), &out, &errb)
		return code, errb.String()
	}
	t.Setenv("GITHUB_OUTPUT", "")
	if code, errs := run("run", "--all"); code != 0 {
		t.Fatalf("baseline: %d\n%s", code, errs)
	}
	outputs := func(env map[string]string) map[string]string {
		t.Helper()
		for k, v := range env {
			t.Setenv(k, v)
		}
		path := filepath.Join(t.TempDir(), "out")
		t.Setenv("GITHUB_OUTPUT", path)
		if code, errs := run("select", "--format", "github", "--max-shards", "4"); code != 0 {
			t.Fatalf("select: %d\n%s", code, errs)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		return outputMap(t, strings.Split(strings.TrimSpace(string(data)), "\n"))
	}

	// Locally, nothing changed: no shard.
	if out := outputs(nil); out["shards"] != "0" || out["shard-matrix"] != `{"shard":[]}` || out["skip"] != "true" {
		t.Errorf("nothing changed: %v", out)
	}
	// A pull request build selects as usual.
	pr := map[string]string{"GITHUB_ACTIONS": "true", "GITHUB_EVENT_NAME": "pull_request", "GITHUB_HEAD_REF": "topic", "GITHUB_REF_NAME": "1/merge"}
	if out := outputs(pr); out["shards"] != "0" {
		t.Errorf("pull request, nothing changed: %v", out)
	}
	// Must keep: a push build of the default branch runs every test, so the shards are
	// sized for all of them, not for an empty selection.
	push := map[string]string{"GITHUB_ACTIONS": "true", "GITHUB_EVENT_NAME": "push", "GITHUB_HEAD_REF": "", "GITHUB_REF_NAME": "main"}
	if out := outputs(push); out["shards"] == "0" || out["skip"] != "false" || out["selected"] != out["total"] {
		t.Errorf("default branch: %v", out)
	}
	// A changed test is selected: at least one shard.
	write("tests/u2_check.sh", "exit 0 # changed\n")
	if out := outputs(pr); out["shards"] == "0" || out["selected"] != "1" {
		t.Errorf("one change: %v", out)
	}
}

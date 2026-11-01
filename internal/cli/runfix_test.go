package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/dotneet/smates/internal/config"
)

// fixRepo is a committed git repository for the tests below.
type fixRepo struct {
	t *testing.T
	g *gitRepo
}

func newFixRepo(t *testing.T, files map[string]string) *fixRepo {
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
	r := &fixRepo{t: t, g: newGitRepo(t)}
	for p, c := range files {
		r.write(p, c)
	}
	r.g.git("add", "-A")
	r.g.commit("init")
	return r
}

func (r *fixRepo) write(rel, content string) {
	r.t.Helper()
	p := filepath.Join(r.g.dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		r.t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		r.t.Fatal(err)
	}
}

// run runs smates args[0] -C <repo> args[1:]: the exit code, stdout and stderr.
func (r *fixRepo) run(args ...string) (int, string, string) {
	var out, errb bytes.Buffer
	code := Main(append(args[:1:1], append([]string{"-C", r.g.dir}, args[1:]...)...), &out, &errb)
	return code, out.String(), errb.String()
}

// countLines counts the lines of a file (0 when it does not exist).
func countLines(t *testing.T, path string) int {
	t.Helper()
	data, _ := os.ReadFile(path)
	return strings.Count(string(data), "\n")
}

// A suite whose runner cannot start (not installed) does not abort the run: the other
// suites run and are recorded, the summary is printed, and the exit code is 1 when
// tests failed, 4 otherwise. Its units are not run and stay selected.
func TestRunnerStartErrorKeepsRunning(t *testing.T) {
	r := newFixRepo(t, map[string]string{
		"smates.yml": `version: 1
orphans: ignore
suites:
  - name: a
    runner: custom
    root: a
    tests: ["*_check.py"]
    command: sh -c 'rc=0; for f; do python3 "$f" || rc=1; done; exit $rc' sh
  - name: b
    runner: custom
    root: b
    tests: ["*_check.py"]
    command: definitely-not-installed-xyz
`,
		".gitignore":     ".smates/\n",
		"a/one_check.py": "raise SystemExit(1)\n",
		"b/two_check.py": "print(2)\n",
	})
	code, _, errs := r.run("run", "--all", "--no-agent")
	if code != ExitFailed {
		t.Errorf("exit %d, want 1 (tests of suite a failed)\n%s", code, errs)
	}
	if _, err := os.Stat(filepath.Join(r.g.dir, ".smates", "state.json.gz")); err != nil {
		t.Errorf("state not saved: %v", err)
	}
	if !strings.Contains(errs, "definitely-not-installed-xyz") || !strings.Contains(errs, "a runner that could not start left 1 selected unit(s) not started") {
		t.Errorf("runner error or not-run units not reported:\n%s", errs)
	}

	// Without failing tests the run exits 4, after recording suite a and writing the
	// report, which lists b's unit as not run and gives exit code 4 as well. Failures
	// mode says the unit did not start, not that the run passed or failed.
	r.write("a/one_check.py", "print(1)\n")
	r.g.git("add", "-A")
	r.g.commit("fix a")
	report := filepath.Join(t.TempDir(), "report.json")
	code, _, errs = r.run("run", "--all", "--no-agent", "--output", "failures", "--report", report)
	if code != ExitEnv {
		t.Errorf("exit %d, want 4\n%s", code, errs)
	}
	if !strings.Contains(errs, "smates: NOT STARTED: a runner that could not start left 1 selected test unit(s) not started (they stay selected)") ||
		strings.Contains(errs, "PASSED") || strings.Contains(errs, "FAILED") {
		t.Errorf("failures mode summary:\n%s", errs)
	}
	var rep struct {
		NotRun   map[string][]string `json:"not_run"`
		ExitCode int                 `json:"exit_code"`
	}
	data, err := os.ReadFile(report)
	if err != nil {
		t.Fatalf("no report: %v\n%s", err, errs)
	}
	if err := json.Unmarshal(data, &rep); err != nil {
		t.Fatal(err)
	}
	if want := map[string][]string{"b": {"b/two_check.py"}}; !reflect.DeepEqual(rep.NotRun, want) {
		t.Errorf("not_run %v, want %v", rep.NotRun, want)
	}
	if rep.ExitCode != ExitEnv {
		t.Errorf("report exit_code %d, want 4 (as smates exits)", rep.ExitCode)
	}
	// In CI (locally an unrecorded test is judged against the base revision instead).
	t.Setenv("CI", "true")
	code, out, errs := r.run("select", "--no-agent", "--format", "files")
	if code != 0 || strings.TrimSpace(out) != "b/two_check.py" {
		t.Errorf("after the run, select: %d %q (want only b's unit; a was recorded)\n%s", code, out, errs)
	}
}

// The Claude Code Stop hook runs after every turn. An orphan the agent judges
// irrelevant leaves nothing to run: the agent's answer is still cached and the orphan
// recorded as handled, so later turns do not ask again. --dry-run caches the answer
// too.
func TestRunNothingToRunSavesAgentDecision(t *testing.T) {
	calls := filepath.Join(t.TempDir(), "calls")
	t.Setenv("FIX_CALLS", calls)
	r := newFixRepo(t, map[string]string{
		"smates.yml": `version: 1
orphans: agent
agent:
  command: 'echo x >> "$FIX_CALLS"; echo "{\"decision\":\"none\",\"no_impact\":[\"deploy/run.sh\"]}"'
suites:
  - name: s
    runner: custom
    tests: ["t/*_check.py"]
    command: sh -c 'for f; do python3 "$f" || exit 1; done' sh
`,
		".gitignore":    ".smates/\n",
		"t/a_check.py":  "print(1)\n",
		"deploy/run.sh": "echo v1\n",
	})
	if code, _, errs := r.run("run", "--all"); code != 0 {
		t.Fatalf("baseline %d %s", code, errs)
	}
	r.write("deploy/run.sh", "echo v2\n")
	in := `{"cwd":"` + r.g.dir + `"}`
	for range 3 {
		var out, errb bytes.Buffer
		code, err := cmdHook(context.Background(), []string{"claude-stop"}, strings.NewReader(in), &out, &errb)
		if code != 0 || err != nil {
			t.Fatalf("hook: %d %v %s", code, err, errb.String())
		}
	}
	if n := countLines(t, calls); n != 1 {
		t.Errorf("agent called %d times over 3 Stop hooks for the same change, want 1", n)
	}

	r.write("deploy/run.sh", "echo v3\n")
	for range 2 {
		if code, _, errs := r.run("run", "--dry-run"); code != 0 {
			t.Fatalf("dry run %d %s", code, errs)
		}
	}
	if n := countLines(t, calls); n != 2 {
		t.Errorf("agent called %d times in all after two dry runs of a new change, want 2", n)
	}
}

// A dry run never marks orphans as handled, even when it selects nothing for them
// (here --orphans=skip): the next run under the configured policy still runs the
// affected suite.
func TestDryRunKeepsOrphansUnseen(t *testing.T) {
	r := newFixRepo(t, map[string]string{
		"smates.yml": `version: 1
orphans: all
suites:
  - name: s
    runner: custom
    tests: ["t/*_check.py"]
    command: sh -c 'for f; do python3 "$f" || exit 1; done' sh
`,
		".gitignore":    ".smates/\n",
		"t/a_check.py":  "print(1)\n",
		"deploy/run.sh": "echo v1\n",
	})
	if code, _, errs := r.run("run", "--all"); code != 0 {
		t.Fatalf("baseline %d %s", code, errs)
	}
	r.write("deploy/run.sh", "echo v2\n")
	if code, out, errs := r.run("run", "--dry-run", "--orphans", "skip"); code != 0 || strings.TrimSpace(out) != "" {
		t.Fatalf("dry run: %d %q\n%s", code, out, errs)
	}
	code, _, errs := r.run("run")
	if code != 0 || !strings.Contains(errs, "s: running 1 test unit(s)") {
		t.Errorf("after a dry run with --orphans=skip, the orphan policy did not run suite s: %d\n%s", code, errs)
	}
}

func shardOf(suite, id string, n int) int {
	h := fnv.New32a()
	h.Write([]byte(suite + "\x00" + id))
	return int(h.Sum32()%uint32(n)) + 1
}

// The shards of a sharded run do not ask the agent on their own: an LLM's answers vary
// between calls, and a test picked only by shard 1's agent but owned by shard 2 would
// run nowhere. Without a cached decision they fall back to the orphan policy; a
// decision an unsharded run cached (a shared `smates select` step) is used by every
// shard.
func TestShardedRunAgentDecision(t *testing.T) {
	var a, b string
	for i := 0; a == "" || b == ""; i++ {
		id := fmt.Sprintf("t/x%d_check.py", i)
		if shardOf("s", id, 2) == 1 && a == "" {
			a = id
		} else if shardOf("s", id, 2) == 2 && b == "" {
			b = id
		}
	}
	tmp := t.TempDir()
	log, calls := filepath.Join(tmp, "ran.log"), filepath.Join(tmp, "calls")
	t.Setenv("FIX_LOG", log)
	t.Setenv("FIX_CALLS", calls)
	r := newFixRepo(t, map[string]string{
		"smates.yml": `version: 1
orphans: agent
agent:
  command: 'echo x >> "$FIX_CALLS"; printf "{\"decision\":\"tests\",\"tests\":[{\"suite\":\"s\",\"test\":\"%s\"}]}" "$FIX_PICK"'
suites:
  - name: s
    runner: custom
    tests: ["t/*_check.py"]
    command: sh -c 'for f; do echo "$f" >> "$FIX_LOG"; python3 "$f" || exit 1; done' sh
`,
		".gitignore":    ".smates/\n",
		a:               "print(1)\n",
		b:               "print(2)\n",
		"deploy/run.sh": "echo v1\n",
	})
	if code, _, errs := r.run("run", "--all"); code != 0 {
		t.Fatalf("baseline %d %s", code, errs)
	}
	data, err := os.ReadFile(filepath.Join(r.g.dir, ".smates", "state.json.gz"))
	if err != nil {
		t.Fatal(err)
	}
	states := func() (string, string) {
		s1, s2 := filepath.Join(t.TempDir(), "s1.json.gz"), filepath.Join(t.TempDir(), "s2.json.gz")
		for _, s := range []string{s1, s2} {
			if err := os.WriteFile(s, data, 0o644); err != nil {
				t.Fatal(err)
			}
		}
		return s1, s2
	}
	ran := func() string {
		out, _ := os.ReadFile(log)
		os.Remove(log)
		return string(out)
	}

	// No cached decision: each shard runs its share of the affected suite.
	ran()
	r.write("deploy/run.sh", "echo v2\n")
	s1, s2 := states()
	t.Setenv("FIX_PICK", b)
	c1, _, e1 := r.run("run", "--state", s1, "--shard", "1/2")
	t.Setenv("FIX_PICK", a)
	c2, _, e2 := r.run("run", "--state", s2, "--shard", "2/2")
	if c1 != 0 || c2 != 0 {
		t.Fatalf("shards: %d %d\n%s\n%s", c1, c2, e1, e2)
	}
	if n := countLines(t, calls); n != 0 {
		t.Errorf("sharded runs asked the agent %d time(s)", n)
	}
	if got := ran(); !strings.Contains(got, a) || !strings.Contains(got, b) {
		t.Errorf("the shards ran %q, want both %s and %s (the orphan policy)\n%s\n%s", got, a, b, e1, e2)
	}
	if !strings.Contains(e1, "sharded run: the agent is not asked") {
		t.Errorf("no note on the agent in a sharded run:\n%s", e1)
	}

	// A decision cached by an unsharded `smates select` on the state both shards
	// restore: the shard that owns the picked test runs it.
	r.write("deploy/run.sh", "echo v3\n")
	s1, _ = states()
	t.Setenv("FIX_PICK", b)
	if code, _, errs := r.run("select", "--state", s1); code != 0 {
		t.Fatalf("select %d %s", code, errs)
	}
	shared, err := os.ReadFile(s1)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(s2, shared, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FIX_PICK", a) // a fresh call would answer differently
	c1, _, e1 = r.run("run", "--state", s1, "--shard", "1/2")
	c2, _, e2 = r.run("run", "--state", s2, "--shard", "2/2")
	if c1 != 0 || c2 != 0 {
		t.Fatalf("shards: %d %d\n%s\n%s", c1, c2, e1, e2)
	}
	if n := countLines(t, calls); n != 1 {
		t.Errorf("agent called %d time(s), want 1 (by select)", n)
	}
	if got := ran(); strings.TrimSpace(got) != b {
		t.Errorf("the shards ran %q, want only %s (the cached decision)\n%s\n%s", got, b, e1, e2)
	}
}

// renderConfig writes a smates.yml that parses back to the same suites, whatever the
// strings hold (line breaks, YAML indicators, tabs).
func TestRenderConfigRoundTrip(t *testing.T) {
	long := strings.Repeat("--flag-with-a-long-name ", 8)
	cases := []config.Suite{
		{Name: "a", Runner: "jest", Root: ".", Command: "npx jest\n--ci", Env: map[string]string{"X": "a\nb", "Y": "", "Z": "  lead", "W": "#x", "V": "a: b", "U": "null", "T": "~", "S": "0x10"}},
		{Name: "b: c", Runner: "jest", Root: "pkg #1", Tests: []string{"**/*.{js,ts}", "*"}, Exclude: []string{"!x", "[a]"}},
		{Name: "c", Runner: "jest", Root: ".", Command: "react-scripts test --watchAll=false\t--x"},
		{Name: "d", Runner: "jest", Root: ".", Command: long + "\n" + long + "\r\n"},
	}
	for _, c := range cases {
		doc := renderConfig([]config.Suite{c}, nil)
		cfg, err := config.Parse([]byte(doc))
		if err != nil {
			t.Errorf("%s: parse: %v\n%s", c.Name, err, doc)
			continue
		}
		got := cfg.Suites[0]
		if got.Command != c.Command || len(c.Env) > 0 && !reflect.DeepEqual(got.Env, c.Env) || got.Name != c.Name || got.Root != c.Root || !reflect.DeepEqual(got.Tests, c.Tests) || !reflect.DeepEqual(got.Exclude, c.Exclude) {
			t.Errorf("mismatch:\n got %#v\nwant %#v\n%s", got, c, doc)
		}
	}
	if got := yamlStr("a\nb"); got != `"a\nb"` {
		t.Errorf("yamlStr(a\\nb) = %s", got)
	}
}

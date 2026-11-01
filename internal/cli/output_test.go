package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/dotneet/smates/internal/config"
	"github.com/dotneet/smates/internal/runner"
	"github.com/dotneet/smates/internal/selector"
	"github.com/dotneet/smates/internal/state"
)

// outputFixture is a repository with two suites of Python "tests" run by a custom
// runner: good/ (always passes, prints GOOD-NOISE) and bad/ (fails when
// src/common.py has X != 1, prints BAD-NOISE). Both import src/common.py.
func outputFixture(t *testing.T) (dir string, write func(rel, content string), run func(args ...string) (int, string, string)) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("uses sh")
	}
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not on PATH")
	}
	clearCI(t)
	t.Setenv("GITHUB_OUTPUT", "")
	t.Setenv("GITHUB_STEP_SUMMARY", "")
	t.Setenv("SMATES_OUTPUT", "")
	t.Setenv("PYTHONDONTWRITEBYTECODE", "1")
	noAgentCredentials(t)
	g := newGitRepo(t)
	write = func(rel, content string) {
		p := filepath.Join(g.dir, filepath.FromSlash(rel))
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	cmd := `sh -c 'rc=0; [ $# -gt 0 ] || set -- %s/*_check.py; for f; do python3 "$f" || rc=1; done; exit $rc' sh`
	write("smates.yml", `version: 1
orphans: ignore
suites:
  - name: good
    runner: custom
    tests: ["good/*_check.py"]
    command: `+strings.ReplaceAll(cmd, "%s", "good")+`
  - name: bad
    runner: custom
    tests: ["bad/*_check.py"]
    command: `+strings.ReplaceAll(cmd, "%s", "bad")+`
`)
	write(".gitignore", ".smates/\n")
	write("src/common.py", "X = 1\n")
	write("good/a_check.py", "import sys\nsys.path.insert(0, 'src')\nimport common\nprint('GOOD-NOISE ' * 3)\n")
	write("bad/b_check.py", "import sys\nsys.path.insert(0, 'src')\nimport common\nprint('BAD-NOISE')\nassert common.X == 1, 'X is %r' % common.X\n")
	write("bad/c_check.py", "print('C-NOISE')\n")
	g.git("add", "-A")
	g.commit("fixture")
	run = func(args ...string) (int, string, string) {
		var out, errb bytes.Buffer
		code := Main(append(args[:1:1], append([]string{"-C", g.dir, "--no-agent"}, args[1:]...)...), &out, &errb)
		return code, out.String(), errb.String()
	}
	if code, _, errs := run("run", "--all"); code != 0 {
		t.Fatalf("baseline: %d\n%s", code, errs)
	}
	return g.dir, write, run
}

func TestOutputFailuresMode(t *testing.T) {
	dir, write, run := outputFixture(t)

	// The default (full) streams every batch's output.
	write("src/common.py", "X = 2\n")
	code, out, errs := run("run")
	if code != ExitFailed {
		t.Fatalf("full: exit %d\n%s", code, errs)
	}
	if !strings.Contains(out, "GOOD-NOISE") || !strings.Contains(out, "BAD-NOISE") || !strings.Contains(errs, "running") {
		t.Errorf("full output:\n%s\n--- stderr\n%s", out, errs)
	}

	// failures: only the failing batch's output, then the summary. (Another change, so
	// that both suites run again.)
	write("src/common.py", "X = 3\n")
	code, out, errs = run("run", "--output", "failures")
	if code != ExitFailed {
		t.Fatalf("failures: exit %d\n%s", code, errs)
	}
	if strings.Contains(out, "GOOD-NOISE") || !strings.Contains(out, "BAD-NOISE") || !strings.Contains(out, "X is 3") || !strings.Contains(out, "── smates: FAIL bad") {
		t.Errorf("failures output:\n%s", out)
	}
	if strings.Contains(errs, "running") || strings.Contains(errs, "failure hints") {
		t.Errorf("progress lines in failures mode:\n%s", errs)
	}
	if !strings.Contains(errs, "smates: FAILED 1 of 2 test unit(s)") || !strings.Contains(errs, "  bad: bad/b_check.py") ||
		!strings.Contains(errs, "changed inputs: src/common.py (modified)") {
		t.Errorf("summary:\n%s", errs)
	}
	_, rerun, ok := strings.Cut(errs, "smates: rerun them: ")
	if !ok {
		t.Fatalf("no rerun command:\n%s", errs)
	}
	rerun = strings.TrimSpace(rerun)
	want := "smates run -C " + dir + " --output failures --no-agent --only bad:bad/b_check.py"
	if rerun != want {
		t.Errorf("rerun command\n got %s\nwant %s", rerun, want)
	}

	// The rerun command runs exactly those tests.
	var o, e bytes.Buffer
	code = Main(strings.Fields(rerun)[1:], &o, &e)
	if code != ExitFailed || !strings.Contains(e.String(), "selected 1 of 2 test unit(s)") || strings.Contains(o.String(), "GOOD-NOISE") {
		t.Errorf("rerun: exit %d\n%s\n%s", code, o.String(), e.String())
	}

	// A passing run is one line after the selection.
	write("src/common.py", "X = 1\n")
	code, out, errs = run("run", "--output", "failures", "--only", "bad:bad/b_check.py")
	if code != 0 || out != "" || !strings.Contains(errs, "smates: PASSED 1 of 1 selected test unit(s)") {
		t.Errorf("passing --only: exit %d\n%s\n%s", code, out, errs)
	}
	// --only recorded b as passed: it is no longer selected; a (changed since its last
	// run) still is.
	code, out, _ = run("select", "--format", "files")
	if code != 0 || out != "good/a_check.py\n" {
		t.Errorf("selection after --only: %q", out)
	}

	// SMATES_OUTPUT=agent selects the failures mode.
	t.Setenv("SMATES_OUTPUT", "agent")
	code, out, errs = run("run")
	if code != 0 || strings.Contains(out, "NOISE") || !strings.Contains(errs, "smates: PASSED") {
		t.Errorf("SMATES_OUTPUT=agent: exit %d\n%s\n%s", code, out, errs)
	}
}

func TestReportToStdout(t *testing.T) {
	_, write, run := outputFixture(t)
	write("src/common.py", "X = 2\n")
	code, out, errs := run("run", "--report", "-")
	if code != ExitFailed {
		t.Fatalf("exit %d\n%s", code, errs)
	}
	var rep struct {
		Results  map[string]map[string]string `json:"results"`
		ExitCode int                          `json:"exit_code"`
	}
	if err := json.Unmarshal([]byte(out), &rep); err != nil {
		t.Fatalf("stdout is not only the report: %v\n%s", err, out)
	}
	if rep.Results["bad"]["bad/b_check.py"] != "fail" || rep.Results["good"]["good/a_check.py"] != "pass" || rep.ExitCode == 0 {
		t.Errorf("report: %+v", rep)
	}
	if !strings.Contains(errs, "GOOD-NOISE") || !strings.Contains(errs, "BAD-NOISE") {
		t.Errorf("runner output must go to stderr:\n%s", errs)
	}
}

func TestExitCodes(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses sh")
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
	write("t/a_check.sh", "exit 0\n")
	write(".gitignore", ".smates/\n")
	suite := func(command string) string {
		return "version: 1\norphans: ignore\nsuites:\n  - name: sh\n    runner: custom\n    tests: [\"t/*_check.sh\"]\n    command: " + command + "\n"
	}
	run := func(args ...string) (int, string) {
		var out, errb bytes.Buffer
		code := Main(append(args[:1:1], append([]string{"-C", g.dir, "--no-agent", "--no-record"}, args[1:]...)...), &out, &errb)
		return code, errb.String()
	}
	cases := []struct {
		name   string
		config string
		args   []string
		want   int
	}{
		{"pass", suite("sh -c 'exit 0'"), []string{"run", "--all"}, ExitOK},
		{"tests fail with the runner's code 7", suite("sh -c 'exit 7'"), []string{"run", "--all"}, ExitFailed},
		{"usage: bad --output", suite("sh -c 'exit 0'"), []string{"run", "--output", "bogus"}, ExitUsage},
		{"usage: bad --only", suite("sh -c 'exit 0'"), []string{"run", "--only", "nocolon"}, ExitUsage},
		{"usage: --only with --all", suite("sh -c 'exit 0'"), []string{"run", "--all", "--only", "sh:t/a_check.sh"}, ExitUsage},
		{"usage: --only with --add", suite("sh -c 'exit 0'"), []string{"run", "--only", "sh:t/a_check.sh", "--add", "sh:t/a_check.sh"}, ExitUsage},
		{"usage: --only with --orphans", suite("sh -c 'exit 0'"), []string{"run", "--only", "sh:t/a_check.sh", "--orphans=skip"}, ExitUsage},
		{"config: unknown field", suite("sh -c 'exit 0'") + "bogus: 1\n", []string{"run", "--all"}, ExitConfig},
		{"config: unknown runner", "version: 1\nsuites:\n  - name: x\n    runner: nope\n", []string{"run", "--all"}, ExitConfig},
		{"config: unknown --suite", suite("sh -c 'exit 0'"), []string{"run", "--suite", "nope"}, ExitConfig},
		{"config: unknown --only suite", suite("sh -c 'exit 0'"), []string{"run", "--only", "nope:t/a_check.sh"}, ExitConfig},
		{"config: unknown --only unit", suite("sh -c 'exit 0'"), []string{"run", "--only", "sh:t/zz_check.sh"}, ExitConfig},
		{"config: unparsable command", suite(`"sh -c 'exit 0"`), []string{"run", "--all"}, ExitConfig},
		{"env: runner not installed", suite("smates-no-such-tool-x1"), []string{"run", "--all"}, ExitEnv},
		{"env: npx tool not installed", suite("npx smates-no-such-tool-x2"), []string{"run", "--all"}, ExitEnv},
		// Untraced tests of a custom runner (and --no-record) report nothing even when they
		// ran: 126/127 cannot be told from a test failure (see TestExit126And127).
		{"tests: launcher exits 127 without results", suite("sh -c 'smates-no-such-tool-x3'"), []string{"run", "--all"}, ExitFailed},
		{"tests: launcher exits 126 without results", suite("sh -c 'exit 126'"), []string{"run", "--all", "--output", "failures"}, ExitFailed},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			write("smates.yml", tc.config)
			if code, errs := run(tc.args...); code != tc.want {
				t.Errorf("exit %d, want %d\n%s", code, tc.want, errs)
			}
		})
	}

	// Not a git repository.
	var out, errb bytes.Buffer
	if code := Main([]string{"run", "-C", t.TempDir()}, &out, &errb); code != ExitEnv {
		t.Errorf("not a repository: exit %d\n%s", code, errb.String())
	}

	// smates config follows the same table.
	cfgRun := func(dir string, args ...string) (int, string) {
		var out, errb bytes.Buffer
		code := Main(append([]string{"config", args[0], "-C", dir}, args[1:]...), &out, &errb)
		return code, errb.String()
	}
	write("smates.yml", suite("sh -c 'exit 0'"))
	if code, errs := cfgRun(g.dir, "set", "orphans", "all"); code != ExitOK {
		t.Errorf("config set: exit %d\n%s", code, errs)
	}
	if code, errs := cfgRun(g.dir, "set", "orphans", "sometimes"); code != ExitConfig {
		t.Errorf("config set invalid value: exit %d\n%s", code, errs)
	}
	write("smates.yml", suite("sh -c 'exit 0'")+"bogus: 1\n")
	if code, errs := cfgRun(g.dir, "set", "orphans", "all"); code != ExitConfig {
		t.Errorf("config set on an invalid config: exit %d\n%s", code, errs)
	}
	os.Remove(filepath.Join(g.dir, "smates.yml"))
	if code, errs := cfgRun(g.dir, "add-ignore", "docs/**"); code != ExitConfig {
		t.Errorf("config without smates.yml: exit %d\n%s", code, errs)
	}
	if code, errs := cfgRun(t.TempDir(), "add-ignore", "docs/**"); code != ExitEnv {
		t.Errorf("config outside a repository: exit %d\n%s", code, errs)
	}
}

func TestParseOnly(t *testing.T) {
	got, err := parseOnly([]string{"web:src/a.test.ts", "go:./internal/cli/", "go:.", "go:./", "py:a:b.py"})
	if err != nil {
		t.Fatal(err)
	}
	want := []onlySpec{{"web", "src/a.test.ts"}, {"go", "internal/cli"}, {"go", "."}, {"go", "."}, {"py", "a:b.py"}}
	if len(got) != len(want) {
		t.Fatalf("%v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("%d: %v, want %v", i, got[i], want[i])
		}
	}
	for _, bad := range []string{"web", ":x", "web:", "web: "} {
		if _, err := parseOnly([]string{bad}); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestParseOutputMode(t *testing.T) {
	var log bytes.Buffer
	for _, tc := range []struct{ flag, env, want string }{
		{"", "", outputFull}, {"", "agent", outputFailures}, {"", "failures", outputFailures}, {"", "FULL", outputFull},
		{"full", "agent", outputFull}, {"agent", "", outputFailures}, {"", "bogus", outputFull},
	} {
		t.Setenv("SMATES_OUTPUT", tc.env)
		if got, err := parseOutputMode(tc.flag, &log); err != nil || got != tc.want {
			t.Errorf("flag %q env %q: %q %v", tc.flag, tc.env, got, err)
		}
	}
	if !strings.Contains(log.String(), `ignoring SMATES_OUTPUT="bogus"`) {
		t.Errorf("no warning: %s", log.String())
	}
	if _, err := parseOutputMode("bogus", &log); err == nil {
		t.Error("invalid --output accepted")
	}
}

func TestTruncateOutput(t *testing.T) {
	var lines []string
	for i := 0; i < 500; i++ {
		lines = append(lines, "line")
	}
	lines[0], lines[499] = "first", "last"
	got := truncateLines(strings.Join(lines, "\n"), 50, 10)
	if n := strings.Count(got, "\n") + 1; n != 51 || !strings.HasPrefix(got, "first\n") || !strings.HasSuffix(got, "\nlast") ||
		!strings.Contains(got, "… 450 line(s) omitted") {
		t.Errorf("%d lines:\n%s", n, got)
	}
	if got := truncateLines("a\nb\n", 50, 10); got != "a\nb" {
		t.Errorf("short: %q", got)
	}

	var b outputBuffer
	b.Write([]byte("START"))
	chunk := bytes.Repeat([]byte("x"), 1<<20)
	for i := 0; i < 5; i++ {
		b.Write(chunk)
	}
	b.Write([]byte("END"))
	text := b.text()
	if len(text) > maxBatchHead+maxBatchTail+100 || !strings.HasPrefix(text, "START") || !strings.HasSuffix(text, "END") || !strings.Contains(text, "bytes of output omitted") {
		t.Errorf("buffer: %d bytes, %q … %q", len(text), text[:10], text[len(text)-10:])
	}
}

func TestPrintFailedBatch(t *testing.T) {
	var w bytes.Buffer
	c := &cycle{stdout: &w}
	sr := &selector.Suite{Name: "web", Suite: &runner.Suite{Suite: config.Suite{Runner: "vitest"}}}
	b := &batch{sr: sr, units: []string{"a.test.ts", "b.test.ts", "c.test.ts"}}
	buf := &outputBuffer{}
	buf.Write([]byte("whole batch output\n"))

	// Every failing test has its own output: only that is printed.
	obs := map[string]*runner.Observation{
		"a.test.ts": {Status: state.Pass},
		"b.test.ts": {Status: state.Fail, Failure: "\x1b[31mexpected 1 to be 2\x1b[0m"},
		"c.test.ts": {Status: state.Fail, Failure: "boom"},
	}
	c.printFailedBatch(sr, b, buf, obs)
	want := "── smates: FAIL web: b.test.ts\nexpected 1 to be 2\n── smates: FAIL web: c.test.ts\nboom\n"
	if w.String() != want {
		t.Errorf("per test:\n%s", w.String())
	}

	// A failure without its own output (or an inferred one): the batch output.
	w.Reset()
	obs["c.test.ts"].Failure = ""
	c.printFailedBatch(sr, b, buf, obs)
	if want := "── smates: FAIL web (3 test unit(s) with vitest); its output:\nwhole batch output\n"; w.String() != want {
		t.Errorf("batch:\n%s", w.String())
	}
}

// TestExit126And127: a recorded run whose runner (any but custom) exits 126/127
// although no test reported a result is an environment problem; a custom runner, or a
// run with --no-record, may report nothing when its tests ran: a test failure.
func TestExit126And127(t *testing.T) {
	_, write, run := outputFixture(t) // records traced results for good/a_check.py
	cfg := func(runner, tests, command string) string {
		return "version: 1\norphans: ignore\nsuites:\n  - name: s\n    runner: " + runner + "\n    tests: [\"" + tests + "\"]\n    command: " + command + "\n"
	}

	// Custom runner: the tests run and report results, then the command exits 127.
	write("smates.yml", cfg("custom", "good/*_check.py", `sh -c 'for f; do python3 "$f"; done; exit 127' sh`))
	if code, _, errs := run("run", "--all"); code != ExitFailed {
		t.Errorf("results then 127: exit %d, want %d\n%s", code, ExitFailed, errs)
	}
	if code, _, errs := run("run", "--all", "--no-record"); code != ExitFailed {
		t.Errorf("--no-record, tests ran then 127: exit %d, want %d\n%s", code, ExitFailed, errs)
	}
	// Custom runner, tool missing: nothing tells it from a failing test.
	write("smates.yml", cfg("custom", "good/*_check.py", "sh -c 'smates-no-such-tool-x5'"))
	if code, _, errs := run("run", "--all"); code != ExitFailed {
		t.Errorf("custom, missing tool: exit %d, want %d\n%s", code, ExitFailed, errs)
	}

	// pytest launched through a shell that cannot find it: no test ran.
	write("pt/test_x.py", "def test_x():\n    pass\n")
	write("smates.yml", cfg("pytest", "pt/test_*.py", "sh -c 'smates-no-such-pytest-x6' sh"))
	code, out, errs := run("run", "--all", "--output", "failures")
	if code != ExitEnv || !strings.Contains(out, "smates-no-such-pytest-x6") ||
		!strings.Contains(errs, "exited with code 127 before any test reported a result") || !strings.Contains(errs, "smates: FAILED") {
		t.Errorf("pytest, missing tool: exit %d\n%s\n--- stderr\n%s", code, out, errs)
	}
	if code, _, errs := run("run", "--all", "--no-record"); code != ExitFailed {
		t.Errorf("pytest, missing tool, --no-record: exit %d, want %d\n%s", code, ExitFailed, errs)
	}
}

func TestNotStarted(t *testing.T) {
	obs := map[string]*runner.Observation{"a": {Status: state.Fail}}
	for _, tc := range []struct {
		runner string
		record bool
		r      *runner.RunResult
		want   bool
	}{
		{"pytest", true, &runner.RunResult{ExitCode: 127}, true},
		{"vitest", true, &runner.RunResult{ExitCode: 126}, true},
		{"pytest", true, &runner.RunResult{ExitCode: 127, Observations: obs}, false},
		{"pytest", false, &runner.RunResult{ExitCode: 127}, false},
		{"custom", true, &runner.RunResult{ExitCode: 127}, false},
		{"pytest", true, &runner.RunResult{ExitCode: 1}, false},
		{"pytest", true, &runner.RunResult{ExitCode: 0}, false},
	} {
		if got := notStarted(tc.runner, tc.record, tc.r); got != tc.want {
			t.Errorf("notStarted(%s, record=%t, exit %d, %d obs) = %t", tc.runner, tc.record, tc.r.ExitCode, len(tc.r.Observations), got)
		}
	}
}

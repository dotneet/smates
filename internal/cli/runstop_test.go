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

	"github.com/dotneet/smates/internal/state"
)

// customSuites is a smates.yml with custom suites over outputFixture's tests, in the
// given order (names are directories); pre runs in the shell before the tests.
func customSuites(orphans, pre string, names ...string) string {
	cfg := "version: 1\norphans: " + orphans + "\nsuites:\n"
	for _, n := range names {
		cfg += "  - name: " + n + "\n    runner: custom\n    tests: [\"" + n + "/*_check.py\"]\n" +
			"    command: sh -c '" + pre + "rc=0; [ $# -gt 0 ] || set -- " + n + `/*_check.py; for f; do python3 "$f" || rc=1; done; exit $rc' sh` + "\n"
	}
	return cfg
}

// A watch cycle applies the orphan policy only to the files it was started for: an
// older orphan must stay unseen, so that a later run still runs its suites.
func TestWatchCycleKeepsUnhandledOrphans(t *testing.T) {
	dir, write, run := outputFixture(t)
	write("smates.yml", customSuites("all", "", "bad", "good"))
	if code, _, errs := run("run", "--all"); code != 0 {
		t.Fatalf("baseline: %d\n%s", code, errs)
	}
	cycle := func(scope map[string]bool) (int, string) {
		t.Helper()
		var errb bytes.Buffer
		f, err := parseRunFlags([]string{"-C", dir, "--no-agent", "--fail-fast", "--select"}, &errb)
		if err != nil {
			t.Fatal(err)
		}
		code, err := runCycle(context.Background(), f, &errb, &errb, &runHooks{orphans: scope})
		if err != nil {
			t.Fatalf("cycle: %v\n%s", err, errb.String())
		}
		return code, errb.String()
	}

	// An orphan (Dockerfile) and a failing test: --fail-fast stops after bad's test.
	write("Dockerfile", "FROM scratch\n")
	write("bad/b_check.py", "assert False\n")
	if code, errs := cycle(nil); code == 0 || !strings.Contains(errs, "--fail-fast left") {
		t.Fatalf("first cycle: %d\n%s", code, errs)
	}
	// The fix starts a cycle for bad/b_check.py only: the Dockerfile is not handled.
	write("bad/b_check.py", "print('ok')\n")
	if code, errs := cycle(map[string]bool{"bad/b_check.py": true}); code != 0 {
		t.Fatalf("second cycle: %d\n%s", code, errs)
	}
	code, out, errs := run("select", "--format", "files")
	if code != 0 || !strings.Contains(out, "good/a_check.py") || !strings.Contains(out, "bad/c_check.py") {
		t.Errorf("the Dockerfile's suites must still be selected: %d\n%s\n%s", code, out, errs)
	}

	// smates run applies the policy and records the orphan as handled.
	if code, _, errs := run("run"); code != 0 {
		t.Fatalf("run: %d\n%s", code, errs)
	}
	if code, out, errs := run("select", "--format", "files"); code != 0 || out != "" {
		t.Errorf("after run: %d %q\n%s", code, out, errs)
	}
}

// An environment error in one suite does not hide the test failures of another: exit 1.
func TestEnvErrorWithFailingTests(t *testing.T) {
	dir, write, run := outputFixture(t)
	write("pt/test_x.py", "def test_x():\n    pass\n")
	write("smates.yml", customSuites("ignore", "", "bad")+
		"  - name: pt\n    runner: pytest\n    tests: [\"pt/test_*.py\"]\n    command: sh -c 'smates-no-such-pytest-x6' sh\n")
	write("src/common.py", "X = 2\n")
	report := filepath.Join(dir, "r.json")
	code, _, errs := run("run", "--all", "--report", report)
	if code != ExitFailed || !strings.Contains(errs, "exited with code 127 before any test reported a result") {
		t.Errorf("exit %d, want %d\n%s", code, ExitFailed, errs)
	}
	var rep struct {
		Results  map[string]map[string]string `json:"results"`
		ExitCode int                          `json:"exit_code"`
	}
	data, err := os.ReadFile(report)
	if err == nil {
		err = json.Unmarshal(data, &rep)
	}
	if err != nil || rep.Results["bad"]["bad/b_check.py"] != state.Fail || rep.ExitCode == 0 {
		t.Errorf("report: %v %+v", err, rep)
	}

	// Without a failing test the environment error decides.
	write("src/common.py", "X = 1\n")
	if code, _, errs := run("run", "--all"); code != ExitEnv {
		t.Errorf("only the env error: exit %d, want %d\n%s", code, ExitEnv, errs)
	}
}

func TestDryRun(t *testing.T) {
	_, _, run := outputFixture(t)
	code, out, errs := run("run", "--all", "--dry-run")
	if code != 0 || !strings.Contains(out, `(cd . && sh -c 'rc=0; [ $# -gt 0 ] || set -- good/*_check.py; for f; do python3 "$f" || rc=1; done; exit $rc' sh good/a_check.py)`) {
		t.Errorf("commands must be quoted for the shell: %d\n%s\n%s", code, out, errs)
	}
	if code, _, errs := run("run", "--dry-run", "--report", "r.json"); code != ExitUsage {
		t.Errorf("--dry-run --report: exit %d, want %d\n%s", code, ExitUsage, errs)
	}
}

func TestWatchWithoutSuites(t *testing.T) {
	clearCI(t)
	g := newGitRepo(t)
	os.WriteFile(filepath.Join(g.dir, "README"), []byte("x\n"), 0o644)
	g.git("add", "-A")
	g.commit("init")
	for _, cmd := range []string{"run", "watch"} {
		var out, errb bytes.Buffer
		if code := Main([]string{cmd, "-C", g.dir}, &out, &errb); code != ExitConfig {
			t.Errorf("%s without suites: exit %d, want %d\n%s", cmd, code, ExitConfig, errb.String())
		}
	}
}

// Interrupting a run keeps what finished: it is recorded and reported, the batch cut
// short and those after it are not run (and not recorded as passed), and the exit code
// is 1, not an environment error.
func TestInterruptedRun(t *testing.T) {
	dir, write, run := outputFixture(t)
	marks := t.TempDir()
	// Once armed, the first suite to run passes, the second one hangs until it is killed
	// and the third never starts.
	pre := "if [ -e " + marks + "/armed ] && [ -e " + marks + "/started ]; then touch " + marks + "/hanging; exec sleep 30; fi; touch " + marks + "/started; "
	write("more/d_check.py", "import sys\nsys.path.insert(0, 'src')\nimport common\n")
	write("smates.yml", customSuites("ignore", pre, "good", "bad", "more"))
	if code, _, errs := run("run", "--all"); code != 0 {
		t.Fatalf("baseline: %d\n%s", code, errs)
	}
	os.Remove(filepath.Join(marks, "started"))
	os.WriteFile(filepath.Join(marks, "armed"), nil, 0o644)
	write("src/common.py", "X = 1  # changed\n") // selects a, b and d

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		for deadline := time.Now().Add(20 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
			if _, err := os.Stat(filepath.Join(marks, "hanging")); err == nil {
				break
			}
		}
		cancel()
	}()
	report := filepath.Join(dir, "r.json")
	var out, errb bytes.Buffer
	start := time.Now()
	code, err := cmdRun(ctx, []string{"-C", dir, "--no-agent", "--report", report}, &out, &errb)
	var ee envError
	if code != ExitFailed || err == nil || errors.As(err, &ee) || time.Since(start) > 15*time.Second {
		t.Fatalf("interrupted run: exit %d, %v\n%s", code, err, errb.String())
	}
	var rep struct {
		Results     map[string]map[string]string `json:"results"`
		NotRun      map[string][]string          `json:"not_run"`
		ExitCode    int                          `json:"exit_code"`
		Interrupted bool                         `json:"interrupted"`
	}
	data, err := os.ReadFile(report)
	if err == nil {
		err = json.Unmarshal(data, &rep)
	}
	if err != nil || len(rep.Results) != 1 || len(rep.NotRun) != 2 || !rep.Interrupted || rep.ExitCode == 0 {
		t.Fatalf("report: %v %+v\n%s", err, rep, errb.String())
	}
	units := map[string]string{"good": "good/a_check.py", "bad": "bad/b_check.py", "more": "more/d_check.py"}
	var ran string
	var cut []string
	for s := range rep.Results {
		ran = s
	}
	for s, u := range rep.NotRun {
		if !slices.Equal(u, []string{units[s]}) {
			t.Errorf("not run in %s: %v", s, u)
		}
		cut = append(cut, units[s])
	}
	if rep.Results[ran][units[ran]] != state.Pass {
		t.Errorf("report: %+v", rep)
	}
	// What finished is recorded; what was cut short is selected again.
	code, sel, errs := run("select", "--format", "files")
	slices.Sort(cut)
	if got := strings.Fields(sel); code != 0 || (!slices.Equal(got, cut) && !slices.Equal(got, []string{cut[1], cut[0]})) {
		t.Errorf("after the interrupt: %d %q\n%s", code, sel, errs)
	}
}

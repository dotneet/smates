package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/dotneet/smates/internal/state"
)

// Only testsFailedError exits 1: every other error, classified or not, exits 2 to 4.
func TestExitCodeFor(t *testing.T) {
	plain := errors.New("boom")
	cases := []struct {
		name string
		err  error
		want int
	}{
		{"plain error", plain, ExitEnv},
		{"wrapped plain error", fmt.Errorf("writing: %w", plain), ExitEnv},
		{"file system error", &os.PathError{Op: "open", Path: "x", Err: os.ErrPermission}, ExitEnv},
		{"io error", io.ErrUnexpectedEOF, ExitEnv},
		{"state locked", fmt.Errorf("saving state: %w", state.ErrLocked), ExitEnv},
		{"usage", usageError{plain}, ExitUsage},
		{"wrapped usage", fmt.Errorf("x: %w", usageError{plain}), ExitUsage},
		{"config", configError{plain}, ExitConfig},
		{"wrapped config", fmt.Errorf("x: %w", configError{plain}), ExitConfig},
		{"env", envError{plain}, ExitEnv},
		{"requires", envError{requiresError{plain}}, ExitEnv},
		{"tests failed", testsFailedError{plain}, ExitFailed},
		{"wrapped tests failed", fmt.Errorf("x: %w", testsFailedError{plain}), ExitFailed},
		// The outer classification wins.
		{"env around tests failed", envError{testsFailedError{plain}}, ExitEnv},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := exitCodeFor(c.err); got != c.want {
				t.Errorf("exitCodeFor(%v) = %d, want %d", c.err, got, c.want)
			}
			// orEnvError keeps a classification and never turns an error into exit 1.
			if got := exitCodeFor(orEnvError(c.err)); got != c.want {
				t.Errorf("exitCodeFor(orEnvError(%v)) = %d, want %d", c.err, got, c.want)
			}
		})
	}
	if orEnvError(nil) != nil {
		t.Error("orEnvError(nil) is not nil")
	}
}

// resolveExit decides the end of a run from its pieces; the code it reports (summary,
// --report) agrees with the code smates exits with, and no run without a failing or
// interrupted test exits 1.
func TestResolveExit(t *testing.T) {
	envE := envError{errors.New("runner could not start")}
	unmet := envError{requiresError{errors.New("requirements not met")}}
	save := envError{errors.New("saving state")}
	report := envError{errors.New("writing the report")}
	cfg := configError{errors.New("cannot parse command")}
	cases := []struct {
		name     string
		end      runEnd
		exit     int     // the process exit code
		reported int     // reportedExit
		notes    []error // logged at the end, in order
	}{
		{"pass", runEnd{}, ExitOK, 0, nil},
		{"tests failed", runEnd{worst: 1, testsFailed: true}, ExitFailed, 1, nil},
		{"runner's own code", runEnd{worst: 2, testsFailed: true}, ExitFailed, 2, nil},
		{"interrupted, nothing failed", runEnd{interrupted: true}, ExitFailed, 1, nil},
		{"interrupted with errors", runEnd{worst: 1, interrupted: true, envErr: envE, unmetErr: unmet, saveErr: save, reportErr: report}, ExitFailed, 1, []error{envE, unmet, save, report}},
		{"runner could not start", runEnd{envErr: envE}, ExitEnv, ExitEnv, nil},
		{"126/127 before results", runEnd{worst: 127, envErr: envE}, ExitEnv, ExitEnv, nil},
		{"runner could not start, tests failed", runEnd{worst: 1, testsFailed: true, envErr: envE}, ExitFailed, 1, []error{envE}},
		{"runner could not start, save failed", runEnd{envErr: envE, saveErr: save}, ExitEnv, ExitEnv, []error{save}},
		{"unmet requires", runEnd{unmetErr: unmet}, ExitEnv, ExitEnv, nil},
		{"unmet requires, save failed", runEnd{unmetErr: unmet, saveErr: save}, ExitEnv, ExitEnv, []error{unmet}},
		{"unmet requires, runner could not start", runEnd{unmetErr: unmet, envErr: envE}, ExitEnv, ExitEnv, []error{unmet}},
		{"unmet requires, tests failed", runEnd{worst: 1, testsFailed: true, unmetErr: unmet}, ExitFailed, 1, []error{unmet}},
		{"save failed", runEnd{saveErr: save}, ExitEnv, ExitEnv, nil},
		{"save failed, tests failed", runEnd{worst: 1, testsFailed: true, saveErr: save}, ExitFailed, 1, []error{save}},
		{"report failed", runEnd{reportErr: report}, ExitEnv, ExitEnv, nil},
		{"report failed, tests failed", runEnd{worst: 1, testsFailed: true, reportErr: report}, ExitFailed, 1, []error{report}},
		{"report and save failed", runEnd{saveErr: save, reportErr: report}, ExitEnv, ExitEnv, []error{report}},
		{"report failed, runner could not start", runEnd{envErr: envE, reportErr: report}, ExitEnv, ExitEnv, []error{report}},
		{"report failed, unmet requires", runEnd{unmetErr: unmet, reportErr: report}, ExitEnv, ExitEnv, []error{unmet}},
		{"config error", runEnd{configErr: cfg}, ExitConfig, ExitConfig, nil},
		{"config error after failing tests", runEnd{worst: 1, testsFailed: true, configErr: cfg}, ExitConfig, ExitConfig, nil},
		{"config error with other errors", runEnd{configErr: cfg, envErr: envE, unmetErr: unmet, saveErr: save, reportErr: report}, ExitConfig, ExitConfig, []error{envE, unmet, save, report}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			code, err, notes := resolveExit(c.end)
			exit := testsExitCode(code)
			if err != nil {
				exit = exitCodeFor(err)
			}
			if exit != c.exit {
				t.Errorf("exit %d (code %d, err %v), want %d", exit, code, err, c.exit)
			}
			if got := reportedExit(c.end); got != c.reported {
				t.Errorf("reportedExit %d, want %d", got, c.reported)
			}
			if !slices.Equal(notes, c.notes) {
				t.Errorf("notes %v, want %v", notes, c.notes)
			}
			if exit == ExitFailed && !c.end.testsFailed && !c.end.interrupted {
				t.Errorf("exit 1 without a failing test")
			}
			// The summary and --report give the runner's own code where smates exits 0
			// or 1, else smates' code.
			if r := reportedExit(c.end); exit >= ExitUsage && r != exit || exit < ExitUsage && testsExitCode(r) != exit {
				t.Errorf("reported %d disagrees with exit %d", r, exit)
			}
		})
	}
}

// exitFixture is a repository with a passing custom suite and a recorded state.
func exitFixture(t *testing.T) *gitRepo {
	t.Helper()
	clearCI(t)
	noAgentCredentials(t)
	t.Setenv("GITHUB_OUTPUT", "")
	t.Setenv("GITHUB_STEP_SUMMARY", "")
	g := newGitRepo(t)
	writeTree(t, g.dir, map[string]string{
		".gitignore":   ".smates/\n",
		"t/a_check.sh": "exit 0\n",
		"smates.yml":   "version: 1\norphans: ignore\nsuites:\n  - name: sh\n    runner: custom\n    tests: [\"t/*_check.sh\"]\n    command: sh -c 'for f; do sh \"$f\" || exit 1; done' sh\n",
	})
	g.git("add", "-A")
	g.commit("fixture")
	if code, _, errs := smatesCmd(t, g.dir, "run", "--all", "--no-agent"); code != 0 {
		t.Fatalf("baseline run: %d\n%s", code, errs)
	}
	return g
}

// The error paths of every command exit with the code of what went wrong, never 1
// ("tests failed"), which would send the user, or an agent, to fix code that is fine.
func TestNonTestErrorsNeverExit1(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses sh and file modes")
	}
	notRepo := t.TempDir()
	g := exitFixture(t)
	stateFile := filepath.Join(g.dir, ".smates", "state.json.gz")
	missingDir := filepath.Join(t.TempDir(), "missing", "r.json")
	cases := []struct {
		name  string
		args  []string
		setup func(t *testing.T)
		want  int
	}{
		// Not a git repository: an environment problem for every command.
		{"run outside a repository", []string{"run", "-C", notRepo}, nil, ExitEnv},
		{"select outside a repository", []string{"select", "-C", notRepo}, nil, ExitEnv},
		{"watch outside a repository", []string{"watch", "-C", notRepo}, nil, ExitEnv},
		{"why outside a repository", []string{"why", "-C", notRepo, "x"}, nil, ExitEnv},
		{"deps outside a repository", []string{"deps", "-C", notRepo, "x"}, nil, ExitEnv},
		{"flaky outside a repository", []string{"flaky", "-C", notRepo}, nil, ExitEnv},
		{"stats outside a repository", []string{"stats", "-C", notRepo}, nil, ExitEnv},
		{"orphans outside a repository", []string{"orphans", "-C", notRepo}, nil, ExitEnv},
		{"list outside a repository", []string{"list", "-C", notRepo}, nil, ExitEnv},
		{"state show outside a repository", []string{"state", "show", "-C", notRepo}, nil, ExitEnv},
		{"state push outside a repository", []string{"state", "push", "-C", notRepo}, nil, ExitEnv},
		{"state pull outside a repository", []string{"state", "pull", "-C", notRepo}, nil, ExitEnv},
		{"state merge outside a repository", []string{"state", "merge", "-C", notRepo}, nil, ExitEnv},
		{"state clear outside a repository", []string{"state", "clear", "-C", notRepo}, nil, ExitEnv},
		{"hook install outside a repository", []string{"hook", "install", "-C", notRepo}, nil, ExitEnv},
		{"hook run outside a repository", []string{"hook", "run", "pre-push", "--", "-C", notRepo}, nil, ExitEnv},

		// smates state.
		{"state merge of a missing file", []string{"state", "merge", "-C", g.dir, filepath.Join(g.dir, "nope.json.gz")}, nil, ExitUsage},
		{"state merge of a directory", []string{"state", "merge", "-C", g.dir, t.TempDir()}, nil, ExitUsage},
		{"state merge of a damaged file", []string{"state", "merge", "-C", g.dir, writeFile(t, "bad.json", "{")}, nil, ExitEnv},
		{"state merge -o into a missing directory's file", []string{"state", "merge", "-C", g.dir, "-o", "/dev/null/x.json"}, nil, ExitEnv},
		{"state push on a detached HEAD", []string{"state", "push", "-C", g.dir}, func(t *testing.T) {
			t.Setenv("SMATES_BRANCH", "")
			g.git("checkout", "--detach", "HEAD")
			t.Cleanup(func() { g.git("checkout", "-") })
		}, ExitUsage},
		{"state push without a remote", []string{"state", "push", "-C", g.dir, "--remote", "no-such-remote", "--ref", "refs/smates/state/x"}, nil, ExitEnv},
		{"state push without a state", []string{"state", "push", "-C", g.dir, "--state", filepath.Join(t.TempDir(), "none.json.gz"), "--ref", "refs/smates/state/x"}, nil, ExitEnv},
		{"state merge while the state is locked", []string{"state", "merge", "-C", g.dir}, func(t *testing.T) {
			lockedState(t, stateFile)
		}, ExitEnv},
		{"state clear of a state that cannot be removed", []string{"state", "clear", "-C", g.dir, "--state", filepath.Join(readOnlyDir(t), "s.json.gz")}, nil, ExitEnv},

		// A state that cannot be read at all, or saved.
		{"run with an unreadable state file", []string{"run", "-C", g.dir, "--no-agent"}, func(t *testing.T) { unreadableFile(t, stateFile) }, ExitEnv},
		{"select with an unreadable state file", []string{"select", "-C", g.dir, "--no-agent"}, func(t *testing.T) { unreadableFile(t, stateFile) }, ExitEnv},
		{"state show with an unreadable state file", []string{"state", "show", "-C", g.dir}, func(t *testing.T) { unreadableFile(t, stateFile) }, ExitEnv},
		{"run while the state stays locked", []string{"run", "-C", g.dir, "--no-agent", "--all"}, func(t *testing.T) { lockedState(t, stateFile) }, ExitEnv},

		// --report that cannot be written, tests passing.
		{"run --report into a missing directory", []string{"run", "-C", g.dir, "--no-agent", "--all", "--report", missingDir}, nil, ExitEnv},
		{"run --report into a missing directory, not recorded", []string{"run", "-C", g.dir, "--no-agent", "--all", "--no-record", "--report", missingDir}, nil, ExitEnv},

		// The hooks' own setup.
		{"hook install over a foreign hook", []string{"hook", "install", "-C", g.dir}, func(t *testing.T) {
			hooks := filepath.Join(g.dir, ".git", "hooks")
			os.MkdirAll(hooks, 0o755)
			writeFileAt(t, filepath.Join(hooks, "pre-push"), "#!/bin/sh\necho mine\n")
			t.Cleanup(func() { os.Remove(filepath.Join(hooks, "pre-push")) })
		}, ExitEnv},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if c.setup != nil {
				c.setup(t)
			}
			var out, errb bytes.Buffer
			code := Main(c.args, &out, &errb)
			if code == ExitFailed || code != c.want {
				t.Errorf("exit %d, want %d (never 1)\n%s", code, c.want, errb.String())
			}
		})
	}
}

// A state that cannot be decoded is ignored with a warning, as `smates run` does: the
// state commands that read it start cold instead of failing.
func TestStateCommandsStartColdOnDamagedState(t *testing.T) {
	g := exitFixture(t)
	stateFile := filepath.Join(g.dir, ".smates", "state.json.gz")
	other := filepath.Join(t.TempDir(), "other.json.gz")
	if err := os.Rename(stateFile, other); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"show"}, {"merge", other}, {"stats"}} {
		t.Run(args[0], func(t *testing.T) {
			if err := os.WriteFile(stateFile, []byte("not gzip"), 0o644); err != nil {
				t.Fatal(err)
			}
			var code int
			var errs string
			if args[0] == "stats" {
				code, _, errs = smatesCmd(t, g.dir, "stats")
			} else {
				code, _, errs = smatesCmd(t, g.dir, "state", args...)
			}
			if code != 0 || !strings.Contains(errs, "ignoring unreadable state") {
				t.Fatalf("exit %d\n%s", code, errs)
			}
			if args[0] == "merge" {
				// The merged file replaced the damaged state.
				st, err := state.Load(stateFile)
				if err != nil || len(st.Runs) != 1 {
					t.Errorf("after merge: %v %+v", err, st)
				}
			}
		})
	}
}

// A configuration error in a later batch (a suite command that cannot be parsed) stops
// the run, but what ran before is reported (--report, summary) and recorded first.
func TestConfigErrorKeepsCompletedBatches(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses sh")
	}
	g := exitFixture(t)
	writeTree(t, g.dir, map[string]string{
		"u/b_check.sh": "exit 0\n",
		"smates.yml": "version: 1\norphans: ignore\nsuites:\n" +
			"  - name: a\n    runner: custom\n    tests: [\"t/*_check.sh\"]\n    command: sh -c 'for f; do sh \"$f\" || exit 1; done' sh\n" +
			"  - name: b\n    runner: custom\n    tests: [\"u/*_check.sh\"]\n    command: \"sh -c 'exit 0\"\n",
	})
	report := filepath.Join(t.TempDir(), "r.json")
	md := filepath.Join(t.TempDir(), "s.md")
	code, _, errs := smatesCmd(t, g.dir, "run", "--no-agent", "--all", "--report", report, "--markdown", md)
	if code != ExitConfig {
		t.Fatalf("exit %d, want %d\n%s", code, ExitConfig, errs)
	}
	var rep struct {
		Results  map[string]map[string]string `json:"results"`
		NotRun   map[string][]string          `json:"not_run"`
		ExitCode int                          `json:"exit_code"`
	}
	data, err := os.ReadFile(report)
	if err == nil {
		err = json.Unmarshal(data, &rep)
	}
	if err != nil || rep.ExitCode != ExitConfig || len(rep.NotRun["b"]) != 1 {
		t.Fatalf("report: %v %+v\n%s", err, rep, errs)
	}
	if _, err := os.Stat(md); err != nil {
		t.Errorf("no summary: %v", err)
	}
	// Suite a ran before b (or was left out after it): either way it is accounted for.
	if _, ran := rep.Results["a"]; !ran && len(rep.NotRun["a"]) != 1 {
		t.Errorf("suite a neither ran nor is listed as not run: %+v", rep)
	}
}

// writeFile writes content to a new file in a temporary directory and returns its path.
func writeFile(t *testing.T, name, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	writeFileAt(t, p, content)
	return p
}

func writeFileAt(t *testing.T, p, content string) {
	t.Helper()
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// unreadableFile makes p unreadable until the test ends (skipping when permissions do
// not apply, e.g. as root).
func unreadableFile(t *testing.T, p string) {
	t.Helper()
	if err := os.Chmod(p, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(p, 0o644) })
	if f, err := os.Open(p); err == nil {
		f.Close()
		t.Skip("file permissions are not enforced")
	}
}

// readOnlyDir is a directory holding a file that cannot be removed.
func readOnlyDir(t *testing.T) string {
	t.Helper()
	d := t.TempDir()
	writeFileAt(t, filepath.Join(d, "s.json.gz"), "x")
	if err := os.Chmod(d, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(d, 0o755) })
	return d
}

// lockedState holds the lock of the state at path (a live holder's: not stale) and
// shortens the wait for it.
func lockedState(t *testing.T, path string) {
	t.Helper()
	wait := state.LockWait
	state.LockWait = 200 * time.Millisecond
	t.Cleanup(func() { state.LockWait = wait })
	os.MkdirAll(filepath.Dir(path), 0o755)
	writeFileAt(t, path+".lock", "pid 1\n")
	t.Cleanup(func() { os.Remove(path + ".lock") })
}

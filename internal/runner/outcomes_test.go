package runner

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/dotneet/smates/internal/config"
	"github.com/dotneet/smates/internal/state"
	"github.com/dotneet/smates/internal/trace"
)

// outcomeRepo is a repository with test files a.test.js and b.test.js and a run context
// whose traces name each unit on the command line of its process (as the shims would)
// for the units in traced. The suite writes its JUnit report to junit.xml.
func outcomeRepo(t *testing.T, command string, traced ...string) *RunContext {
	t.Helper()
	units := []string{"a.test.js", "b.test.js"}
	repo := tempRepo(t, units...)
	td := t.TempDir()
	os.MkdirAll(trace.V8Dir(td), 0o755)
	start := func(pid, ppid int, argv ...string) {
		ev := map[string]any{"k": "start", "pid": pid, "ppid": ppid, "t": 1, "argv": argv, "cwd": repo.Root}
		os.WriteFile(filepath.Join(td, fmt.Sprintf("proc-%d-0.jsonl", pid)), []byte(mustJSON(ev)+"\n"), 0o644)
	}
	start(10, 1)
	for i, u := range traced {
		start(11+i, 10, "runner", filepath.Join(repo.Root, u))
	}
	var out bytes.Buffer
	return &RunContext{
		Repo: repo, Suite: &Suite{Suite: config.Suite{Root: ".", Command: command, Junit: "junit.xml"}}, Units: units, Record: true,
		TraceDir: td, Stdout: &out, Stderr: &out, UnitOf: func(f string) string {
			if slices.Contains(units, f) {
				return f
			}
			return ""
		},
	}
}

// staleReport writes a JUnit report an earlier run left: every test passed.
func staleReport(t *testing.T, rc *RunContext) {
	t.Helper()
	xml := `<testsuite><testcase file="a.test.js" name="t"/><testcase file="b.test.js" name="t"/></testsuite>`
	if err := os.WriteFile(filepath.Join(rc.Repo.Root, "junit.xml"), []byte(xml), 0o644); err != nil {
		t.Fatal(err)
	}
}

// reportCommand is a command writing a JUnit report for its last argument (a test
// file), failing when it is a.test.js, and exiting with code.
func reportCommand(t *testing.T, code int) string {
	t.Helper()
	script := filepath.Join(t.TempDir(), "report.sh")
	body := `for a; do last=$a; done
case $last in *a.test*) f='<failure message="boom"/>';; *) f='';; esac
printf '<testsuite><testcase file="%s" name="t">%s</testcase></testsuite>' "$last" "$f" > junit.xml
exit ` + fmt.Sprint(code) + "\n"
	if err := os.WriteFile(script, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return "sh " + script
}

// No adapter records a pass for a unit the run did not prove passing: one whose
// process crashed or was killed, whose only result is a report an earlier run left,
// whose process left no trace, or that skipped itself or never ran. Each row also checks
// a unit that did pass, so that the rows cannot hold by recording nothing.
func TestNoPassWithoutEvidence(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("needs sh")
	}
	type want map[string]string // unit -> status ("" = no observation; "!pass" = anything but pass)
	batch := func(rc *RunContext, batches [][]string) (map[string]*Observation, error) {
		code, err := runChunks(context.Background(), rc, nil, batches, nil)
		if err != nil {
			return nil, err
		}
		obs, _, err := observe(rc, code, traceOptions{unitsFromArgv: true})
		return obs, err
	}
	isolated := func(rc *RunContext) (map[string]*Observation, error) {
		res, err := runIsolatedNode(context.Background(), rc, func(u string) []string { return []string{u} }, traceOptions{unitsFromArgv: true})
		if res == nil {
			return nil, err
		}
		return res.Observations, err
	}
	bunRun := func(rc *RunContext) (map[string]*Observation, error) {
		res, err := bun{}.Run(context.Background(), rc)
		if res == nil {
			return nil, err
		}
		return res.Observations, err
	}
	for _, c := range []struct {
		name string
		run  func(t *testing.T) (map[string]*Observation, error)
		want want
	}{
		{"batch: crashed, stale report says pass", func(t *testing.T) (map[string]*Observation, error) {
			rc := outcomeRepo(t, `sh -c 'exit 1' sh`, "a.test.js", "b.test.js")
			staleReport(t, rc)
			return batch(rc, [][]string{{"a.test.js", "b.test.js"}})
		}, want{"a.test.js": "!pass", "b.test.js": "!pass"}},
		{"batch: killed mid-run, stale report says pass", func(t *testing.T) (map[string]*Observation, error) {
			rc := outcomeRepo(t, `sh -c 'kill -9 $$' sh`, "a.test.js", "b.test.js")
			staleReport(t, rc)
			return batch(rc, [][]string{{"a.test.js", "b.test.js"}})
		}, want{"a.test.js": "!pass", "b.test.js": "!pass"}},
		{"batch: each chunk's report counts", func(t *testing.T) (map[string]*Observation, error) {
			rc := outcomeRepo(t, reportCommand(t, 0), "a.test.js", "b.test.js")
			// The second chunk overwrites the first one's report.
			return batch(rc, [][]string{{"a.test.js"}, {"b.test.js"}})
		}, want{"a.test.js": state.Fail, "b.test.js": state.Pass}},
		{"batch: reported pass without a trace", func(t *testing.T) (map[string]*Observation, error) {
			rc := outcomeRepo(t, reportCommand(t, 0), "a.test.js")
			return batch(rc, [][]string{{"a.test.js"}, {"b.test.js"}})
		}, want{"a.test.js": state.Fail, "b.test.js": state.Unknown}},
		{"isolated: crashed after reporting a pass", func(t *testing.T) (map[string]*Observation, error) {
			rc := outcomeRepo(t, `sh -c 'case $1 in *a.test*) printf "<testsuite><testcase file=\"a.test.js\" name=\"t\"/></testsuite>" > junit.xml; exit 3;; esac' sh`, "a.test.js", "b.test.js")
			return isolated(rc)
		}, want{"a.test.js": state.Fail, "b.test.js": state.Pass}},
		{"isolated: killed", func(t *testing.T) (map[string]*Observation, error) {
			rc := outcomeRepo(t, `sh -c 'case $1 in *a.test*) kill -9 $$;; esac' sh`, "a.test.js", "b.test.js")
			staleReport(t, rc)
			return isolated(rc)
		}, want{"a.test.js": state.Fail, "b.test.js": state.Pass}},
		{"isolated: stale report says the crashed unit passed", func(t *testing.T) (map[string]*Observation, error) {
			rc := outcomeRepo(t, `sh -c 'case $1 in *a.test*) exit 1;; esac' sh`, "a.test.js", "b.test.js")
			staleReport(t, rc)
			return isolated(rc)
		}, want{"a.test.js": state.Fail, "b.test.js": state.Pass}},
		{"bun: no trace of the preload shim", func(t *testing.T) (map[string]*Observation, error) {
			rc := outcomeRepo(t, `sh -c 'exit 0' sh`, "b.test.js")
			rc.Known = func(u string) []string { return []string{u} } // self-contained: no lcov is expected
			return bunRun(rc)
		}, want{"a.test.js": state.Unknown, "b.test.js": state.Pass}},
		{"bun: crashed without a trace", func(t *testing.T) (map[string]*Observation, error) {
			rc := outcomeRepo(t, `sh -c 'for a; do last=$a; done; case $last in *a.test*) kill -9 $$;; esac' sh`, "b.test.js")
			rc.Known = func(u string) []string { return []string{u} }
			staleReport(t, rc)
			return bunRun(rc)
		}, want{"a.test.js": state.Fail, "b.test.js": state.Pass}},
	} {
		t.Run(c.name, func(t *testing.T) {
			obs, err := c.run(t)
			if err != nil {
				t.Fatal(err)
			}
			for u, w := range c.want {
				o := obs[u]
				switch {
				case w == "!pass":
					if o != nil && o.Status == state.Pass {
						t.Errorf("%s: %+v, must not pass", u, o)
					}
				case o == nil || o.Status != w:
					t.Errorf("%s: %+v, want %s", u, o, w)
				}
			}
		})
	}
}

// Without recording, a JUnit report an earlier run left is not this run's result.
func TestStaleReportWithoutRecording(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("needs sh")
	}
	for _, c := range []struct {
		name, command string
		want          map[string]string
	}{
		{"crashed before writing it", `sh -c 'exit 1' sh`, map[string]string{"a.test.js": "", "b.test.js": ""}},
		{"killed", `sh -c 'kill -9 $$' sh`, map[string]string{"a.test.js": "", "b.test.js": ""}},
		{"wrote its own", "report:1", map[string]string{"a.test.js": "", "b.test.js": state.Pass}},
	} {
		t.Run(c.name, func(t *testing.T) {
			command := c.command
			if command == "report:1" {
				command = reportCommand(t, 1)
			}
			rc := outcomeRepo(t, command)
			rc.Record, rc.TraceDir = false, t.TempDir()
			rc.Units = []string{"b.test.js"}
			if c.want["b.test.js"] == "" {
				rc.Units = []string{"a.test.js", "b.test.js"}
			}
			staleReport(t, rc)
			res, err := custom{}.Run(context.Background(), rc)
			if err != nil {
				t.Fatal(err)
			}
			for u, w := range c.want {
				o := res.Observations[u]
				if w == "" && o != nil || w != "" && (o == nil || o.Status != w) {
					t.Errorf("%s: %+v, want %q", u, o, w)
				}
			}
		})
	}
}

// A function recorded with its package as a whole passes only when go test printed its
// passing result line: smates adds -v so that it prints one for every test that ran.
func TestWholePackageFunctionOutcomes(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("needs sh")
	}
	for _, c := range []struct {
		name, out string
		want      map[string]string
	}{
		{"pass and skip", "=== RUN   TestA\n--- PASS: TestA (0.00s)\n=== RUN   TestS\n--- SKIP: TestS (0.00s)\nPASS\nok  \tm/p\t0.01s\n",
			map[string]string{"TestA": state.Pass, "TestS": state.Skip, "TestGone": state.Unknown}},
		{"TestMain exits before m.Run", "ok  \tm/p\t0.01s\n", map[string]string{"TestA": state.Unknown}},
		{"-run matched nothing", "testing: warning: no tests to run\nPASS\nok  \tm/p\t0.01s [no tests to run]\n", map[string]string{"TestA": state.Unknown}},
		{"crashed mid-run", "=== RUN   TestA\n--- PASS: TestA (0.00s)\n=== RUN   TestB\nsignal: killed\nFAIL\tm/p\t0.01s\n",
			map[string]string{"TestA": state.Pass, "TestB": state.Unknown}},
		{"json, never ran", `{"Action":"pass","Package":"m/p","Test":"TestA"}` + "\n" + `{"Action":"pass","Package":"m/p"}` + "\n",
			map[string]string{"TestA": state.Pass, "TestB": state.Unknown}},
	} {
		t.Run(c.name, func(t *testing.T) {
			g := &goOutcomes{verbose: true}
			feed(g, c.out)
			for name, w := range c.want {
				if st, _, _ := g.status("m/p", name); st != w {
					t.Errorf("%s: %q, want %q", name, st, w)
				}
			}
		})
	}
	// Without -v, a package that ran no test proves nothing either.
	plain := &goOutcomes{}
	feed(plain, "testing: warning: no tests to run\nPASS\nok  \tm/p\t0.01s [no tests to run]\nok  \tm/q\t0.01s\n")
	if st, _, _ := plain.status("m/p", "TestA"); st != state.Unknown {
		t.Errorf("no tests to run: %q, want unknown", st)
	}
	if st, _, _ := plain.status("m/q", "TestA"); st != state.Pass {
		t.Errorf("a passing package without -v: %q, want pass", st)
	}

	// runPackages passes -v when the outcomes need it.
	repo := tempRepo(t, "p/p.go")
	os.WriteFile(filepath.Join(repo.Root, "go.mod"), []byte("module m\n"), 0o644)
	args := filepath.Join(t.TempDir(), "args")
	stub := filepath.Join(t.TempDir(), "stub.sh")
	os.WriteFile(stub, []byte(`printf "%s\n" "$@" > "$ARGS"; printf "ok  \tm/p\t0.1s\n"`), 0o644)
	for _, verbose := range []bool{false, true} {
		var out bytes.Buffer
		rc := &RunContext{
			Repo: repo, Suite: &Suite{Suite: config.Suite{Root: ".", Command: "sh " + stub, Env: map[string]string{"ARGS": args}}},
			TraceDir: t.TempDir(), Stdout: &out, Stderr: &out,
		}
		g := &goOutcomes{verbose: verbose}
		if _, err := runPackages(context.Background(), rc, []string{"p"}, false, "^(TestA)$", g); err != nil {
			t.Fatal(err)
		}
		got, _ := os.ReadFile(args)
		if has := slices.Contains(strings.Fields(string(got)), "-v"); has != verbose {
			t.Errorf("verbose %v: arguments %q", verbose, got)
		}
		want := state.Pass
		if verbose {
			want = state.Unknown // no result line for TestA
		}
		if st, _, _ := g.status("m/p", "TestA"); st != want {
			t.Errorf("verbose %v: TestA %q, want %q", verbose, st, want)
		}
	}
}

// A compile check passes only for a package go test reported: when go test fails
// after reporting some packages (killed, or stopped early), the others are unknown.
func TestBuildCheckUnreported(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("needs sh")
	}
	for _, c := range []struct {
		name, script string
		want         map[string]string
	}{
		{"killed after the first package", "printf 'ok  \\tm/a\\t0.1s [no tests to run]\\n'; exit 1",
			map[string]string{"a": state.Pass, "b": state.Unknown}},
		{"failed before reporting any", "echo 'go: errors parsing go.mod'; exit 1",
			map[string]string{"a": state.Fail, "b": state.Fail}},
		{"all reported", "printf 'ok  \\tm/a\\t0.1s\\nok  \\tm/b\\t0.1s\\n'",
			map[string]string{"a": state.Pass, "b": state.Pass}},
		{"exit 0 without reports", "true", map[string]string{"a": state.Pass, "b": state.Pass}},
	} {
		t.Run(c.name, func(t *testing.T) {
			bin := t.TempDir()
			os.WriteFile(filepath.Join(bin, "go"), []byte("#!/bin/sh\n"+c.script+"\n"), 0o755)
			t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
			repo := tempRepo(t, "a/a.go", "b/b.go")
			os.WriteFile(filepath.Join(repo.Root, "go.mod"), []byte("module m\n"), 0o644)
			var out bytes.Buffer
			rc := &RunContext{Repo: repo, Suite: &Suite{Suite: config.Suite{Root: ".", Command: "go test"}}, Stdout: &out, Stderr: &out}
			res, err := runBuildChecks(context.Background(), rc, []string{"a", "b"})
			if err != nil {
				t.Fatal(err)
			}
			for d, w := range c.want {
				if o := res.Observations[FuncUnit(d, BuildCheck)]; o == nil || o.Status != w {
					t.Errorf("%s: %+v, want %s", d, o, w)
				}
			}
		})
	}
}

// A Gradle test class passes only once it finished: tests that reported before its JVM
// died do not prove the class.
func TestGradleClassMustFinish(t *testing.T) {
	classes := newGradleClassMap(map[string]string{"p.ATest": "a", "p.BTest": "b"})
	test := func(class, task, result string) gradleEvent {
		return gradleEvent{K: "test", Task: task, Class: class, Name: "t", Result: result}
	}
	done := func(class, task string) gradleEvent {
		return gradleEvent{K: "class", Task: task, Class: class, Name: class, Result: "SUCCESS"}
	}
	for _, c := range []struct {
		name   string
		events []gradleEvent
		want   string
	}{
		{"finished", []gradleEvent{test("p.ATest", ":test", "SUCCESS"), done("p.ATest", ":test")}, state.Pass},
		{"JVM died mid-class", []gradleEvent{test("p.ATest", ":test", "SUCCESS")}, state.Unknown},
		{"only a nested class finished", []gradleEvent{test("p.ATest$In", ":test", "SUCCESS"), done("p.ATest$In", ":test")}, state.Unknown},
		{"nested and outer finished", []gradleEvent{test("p.ATest$In", ":test", "SUCCESS"), done("p.ATest$In", ":test"), done("p.ATest", ":test")}, state.Pass},
		{"finished in another task only", []gradleEvent{test("p.ATest", ":test", "SUCCESS"), test("p.ATest", ":it", "SUCCESS"), done("p.ATest", ":test")}, state.Unknown},
		{"skip-only by an assumption", []gradleEvent{test("p.ATest", ":test", "SKIPPED"), done("p.ATest", ":test")}, state.Unknown},
		{"a failure before the JVM died", []gradleEvent{test("p.ATest", ":test", "FAILURE")}, state.Fail},
	} {
		t.Run(c.name, func(t *testing.T) {
			events := append(c.events, test("p.BTest", ":test", "SUCCESS"), done("p.BTest", ":test"))
			obs, warns := gradleObservations([]string{"a", "b"}, classes, events, 1, false)
			if o := obs["a"]; o == nil || o.Status != c.want {
				t.Errorf("a: %+v, want %s", o, c.want)
			}
			if o := obs["b"]; o == nil || o.Status != state.Pass {
				t.Errorf("b: %+v, want pass", o)
			}
			cut := slices.ContainsFunc(warns, func(w string) bool { return strings.Contains(w, "did not finish") })
			if cut != (c.want == state.Unknown && !strings.Contains(c.name, "skip")) {
				t.Errorf("warnings %q", warns)
			}
		})
	}
}

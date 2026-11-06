package runner

import (
	"context"
	"os"
	"testing"

	"github.com/dotneet/smates/internal/config"
	"github.com/dotneet/smates/internal/state"
)

// A package unit without test files (a main package matched by a widened tests glob)
// builds no test binary and leaves no trace. Recording takes go test's "[no test
// files]" line as its pass, so it is recorded (with its static closure, the known deps)
// instead of running again on every selective run; one that does not build stays a
// failure. A package whose test files build constraints all exclude in this
// configuration is a skip, not a pass: another configuration sharing the state (a tag
// in the environment's GOFLAGS, another GOOS) compiles them, and none of that is a
// dependency of the unit.
func TestRecordPackagesWithoutTests(t *testing.T) {
	rc := goModule(t, map[string]string{
		"lib/lib.go":           "package lib\n\nfunc F() int { return 1 }\n",
		"lib/l_test.go":        "package lib\n\nimport \"testing\"\n\nfunc TestF(t *testing.T) { F() }\n",
		"cmd/app/main.go":      "package main\n\nimport \"example.com/m/lib\"\n\nfunc main() { lib.F() }\n",
		"tagged/t.go":          "package tagged\n",
		"tagged/t_test.go":     "//go:build smates_never\n\npackage tagged\n\nimport \"testing\"\n\nfunc TestT(t *testing.T) {}\n",
		"cmd/broken/main.go":   "package main\n\nfunc main() { undefined() }\n",
		"plat/p.go":            "package plat\n",
		"plat/p_plan9_test.go": "package plat\n\nimport \"testing\"\n\nfunc TestP(t *testing.T) {}\n",
		"ign/i.go":             "package ign\n",
		"ign/_old_test.go":     "package ign\n\nimport \"testing\"\n\nfunc TestOld(t *testing.T) {}\n",
		"alltagged/a_test.go":  "//go:build smates_never\n\npackage alltagged\n\nimport \"testing\"\n\nfunc TestA(t *testing.T) {}\n",
	})
	suites, err := Resolve(rc.Repo, []config.Suite{{Name: "go", Root: ".", Runner: "go", Tests: []string{"**/*_test.go", "**/cmd/**/*.go"}}})
	if err != nil {
		t.Fatal(err)
	}
	rc.Suite = suites[0]
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	rc.Self = self // this test binary acts as the -exec wrapper (see TestMain)
	units := []string{"lib", "cmd/app", "tagged", "cmd/broken", "plat", "ign", "alltagged"}
	rc.Units = units
	res, err := runPackages(context.Background(), rc, units, false, "", nil)
	if err != nil {
		t.Fatalf("%v\n%s%s", err, rc.Stdout, rc.Stderr)
	}
	if res.ExitCode != 1 {
		t.Errorf("exit %d, want 1 (cmd/broken)", res.ExitCode)
	}
	for _, tt := range []struct {
		unit, want string
	}{
		{"lib", state.Pass},
		{"cmd/app", state.Pass},    // no test files: compiled, recorded as passed
		{"cmd/broken", state.Fail}, // does not build: stays a failure
		{"ign", state.Pass},        // a test file go ignores by name does not count
		// Must keep: every test excluded by a tag or the platform runs again.
		{"tagged", state.Skip},
		{"plat", state.Skip},
		// Must keep: nothing compiles (a wrapper may pass the tag): no failure, no pass.
		{"alltagged", state.Skip},
	} {
		o := res.Observations[tt.unit]
		if o == nil || o.Status != tt.want {
			t.Errorf("%s: %+v, want %s\n%s", tt.unit, o, tt.want, rc.Stdout)
		}
	}
}

// The "[no test files]" outcome is read from plain output and from go test -json.
func TestBuildFailuresNoTestFiles(t *testing.T) {
	for _, tt := range []struct {
		name, out string
		want      bool
	}{
		{"plain", "?   \texample.com/m/cmd/app\t[no test files]\n", true},
		{"json", `{"Action":"skip","Package":"example.com/m/cmd/app","Elapsed":0}` + "\n", true},
		{"json test skip", `{"Action":"skip","Package":"example.com/m/cmd/app","Test":"TestX"}` + "\n", false},
		{"ok", "ok  \texample.com/m/cmd/app\t0.01s\n", false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			b := &buildFailures{}
			b.Write([]byte(tt.out))
			if got := b.noTests["example.com/m/cmd/app"]; got != tt.want {
				t.Errorf("noTests %v, want %v", got, tt.want)
			}
		})
	}
}

// go test fails a package whose Go files build constraints all exclude ("[setup
// failed]") without testing anything; smates keeps such a directory as a unit when it
// cannot see the flags (a wrapper command). The exit code is forgiven only when every
// package of the run is accounted for as passing, without test files or so excluded.
func TestBuildFailuresOnlyExcluded(t *testing.T) {
	rc := goModule(t, map[string]string{"a/a.go": "package a\n", "b/b.go": "package b\n", "c/c.go": "package c\n"})
	const (
		excl  = "package example.com/m/a: build constraints exclude all Go files in /r/a\n"
		setup = "FAIL\texample.com/m/a [setup failed]\n"
		okB   = "ok  \texample.com/m/b\t0.01s\n"
		noC   = "?   \texample.com/m/c\t[no test files]\n"
	)
	for _, tt := range []struct {
		name, stdout, stderr string
		units                []string
		code                 int
		want                 bool
	}{
		{"excluded, others pass", setup + okB + noC + "FAIL\n", excl, []string{"a", "b", "c"}, 1, true},
		{"excluded alone", setup + "FAIL\n", excl, []string{"a"}, 1, true},
		{"excluded, JSON-escaped", `{"Action":"build-output","Output":"package example.com/m/a: build constraints exclude all Go files in /r/a\n"}` + "\n" + setup, "", []string{"a"}, 1, true},
		// Must keep the failure in every other case.
		{"another package fails", setup + "FAIL\texample.com/m/b\t0.01s\n" + noC, excl, []string{"a", "b", "c"}, 1, false},
		{"another package builds not", setup + "FAIL\texample.com/m/b [build failed]\n" + noC, excl, []string{"a", "b", "c"}, 1, false},
		{"a package unaccounted for (reformatted output)", setup, excl, []string{"a", "b"}, 1, false},
		{"no reason given", setup + okB, "", []string{"a", "b"}, 1, false},
		{"an imported package excluded", "FAIL\texample.com/m/b [setup failed]\n",
			"package example.com/m/b\n\timports example.com/m/a: build constraints exclude all Go files in /r/a\n", []string{"b"}, 1, false},
		{"another exit code", setup, excl, []string{"a"}, 2, false},
		{"another exit code (make)", setup + okB, excl, []string{"a", "b"}, 2, false},
		{"excluded in two passes", setup + okB + setup + okB, excl + excl, []string{"a", "b"}, 1, true},
		// go test -json: the summary lines are output events.
		{"excluded, JSON events", `{"ImportPath":"example.com/m/a","Action":"build-output","Output":"package example.com/m/a: build constraints exclude all Go files in /r/a\n"}` + "\n" +
			`{"Action":"output","Package":"example.com/m/a","Output":"FAIL\texample.com/m/a [setup failed]\n"}` + "\n" +
			`{"Action":"fail","Package":"example.com/m/a","Elapsed":0,"FailedBuild":"example.com/m/a"}` + "\n" +
			`{"Action":"output","Package":"example.com/m/b","Test":"TestB","Output":"--- PASS: TestB (0.00s)\n"}` + "\n" +
			`{"Action":"output","Package":"example.com/m/b","Output":"ok  \texample.com/m/b\t0.01s\n"}` + "\n" +
			`{"Action":"pass","Package":"example.com/m/b","Elapsed":0.01}` + "\n", "", []string{"a", "b"}, 1, true},
		// Must keep: a package excluded in one pass of a wrapper and compiled in another
		// (go test run again with the tag) fails for real there, in either order.
		{"excluded, then its test fails", setup + okB + "FAIL\texample.com/m/a\t0.6s\n", excl, []string{"a", "b"}, 1, false},
		{"its test fails, then excluded", "FAIL\texample.com/m/a\t0.6s\n" + setup + okB, excl, []string{"a", "b"}, 1, false},
		{"excluded, then its build fails", setup + okB + "FAIL\texample.com/m/a [build failed]\n", excl, []string{"a", "b"}, 1, false},
		{"its build fails, then excluded", "FAIL\texample.com/m/a [build failed]\n" + setup + okB, excl, []string{"a", "b"}, 1, false},
		{"set up failing more often than excluded", setup + setup + okB, excl, []string{"a", "b"}, 1, false},
		{"excluded, then its test fails, JSON events", setup + `{"Action":"output","Package":"example.com/m/a","Output":"FAIL\texample.com/m/a\t0.6s\n"}` + "\n", excl, []string{"a"}, 1, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			b, e := &buildFailures{}, &buildFailures{}
			b.Write([]byte(tt.stdout))
			e.Write([]byte(tt.stderr))
			b.addExcluded(e)
			if got := b.onlyExcluded(rc, tt.units, tt.code); got != tt.want {
				t.Errorf("onlyExcluded %v, want %v", got, tt.want)
			}
			// A package failing for real keeps its failure: not dropped from the
			// observations, not recorded as a skip.
			obs := b.observations(&RunContext{Repo: rc.Repo, Suite: rc.Suite, Units: tt.units})
			res := &RunResult{Observations: obs}
			b.addSkipped(rc, tt.units, res)
			ex := b.excludedUnits(rc, tt.units)
			if b.status["example.com/m/a"] == state.Fail && b.failures["example.com/m/a"] {
				if o := res.Observations["a"]; o == nil || o.Status != state.Fail {
					t.Errorf("a: observed %+v, want a failure", o)
				}
				if ex["a"] {
					t.Errorf("a counted as excluded although it failed for real")
				}
			}
		})
	}
}

package runner

import (
	"context"
	"os"
	"testing"

	"github.com/dotneet/smates/internal/config"
	"github.com/dotneet/smates/internal/state"
)

// A package unit without test files (a main package matched by a widened tests glob,
// or a package whose tests a build constraint all excludes) builds no test binary and
// leaves no trace. Recording takes go test's "[no test files]" line as its pass, so it
// is recorded (with its static closure, the known deps) instead of running again on
// every selective run; one that does not build stays a failure.
func TestRecordPackagesWithoutTests(t *testing.T) {
	rc := goModule(t, map[string]string{
		"lib/lib.go":         "package lib\n\nfunc F() int { return 1 }\n",
		"lib/l_test.go":      "package lib\n\nimport \"testing\"\n\nfunc TestF(t *testing.T) { F() }\n",
		"cmd/app/main.go":    "package main\n\nimport \"example.com/m/lib\"\n\nfunc main() { lib.F() }\n",
		"tagged/t.go":        "package tagged\n",
		"tagged/t_test.go":   "//go:build smates_never\n\npackage tagged\n\nimport \"testing\"\n\nfunc TestT(t *testing.T) {}\n",
		"cmd/broken/main.go": "package main\n\nfunc main() { undefined() }\n",
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
	units := []string{"lib", "cmd/app", "tagged", "cmd/broken"}
	rc.Units = units
	res, err := runPackages(context.Background(), rc, units, false, "", nil)
	if err != nil {
		t.Fatalf("%v\n%s%s", err, rc.Stdout, rc.Stderr)
	}
	for _, tt := range []struct {
		unit, want string
	}{
		{"lib", state.Pass},
		{"cmd/app", state.Pass},    // no test files: compiled, recorded as passed
		{"tagged", state.Pass},     // every test excluded by a tag: the same
		{"cmd/broken", state.Fail}, // does not build: stays a failure
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

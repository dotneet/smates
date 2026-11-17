package runner

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/dotneet/smates/internal/config"
	"github.com/dotneet/smates/internal/gitx"
	"github.com/dotneet/smates/internal/state"
	"github.com/dotneet/smates/internal/testutil"
)

// A user's -exec program is found in every place go test reads one, in every spelling,
// and the last one wins; on a go test command line it is removed (smates' own -exec
// in GOFLAGS then applies, and runs the user's behind it), but never from the words
// after -args, which are the test binary's.
func TestGoCommandUserExec(t *testing.T) {
	t.Setenv("GOFLAGS", "")
	cases := []struct {
		name    string
		cmd     string
		extra   []string
		goflags string
		// want is the user's -exec program; onCmd whether it is on the command line.
		want  string
		onCmd bool
		// stripped is the command line (without ExtraArgs' package arguments) once the
		// -exec flags are removed.
		stripped []string
	}{
		{"none", "go test ./...", nil, "", "", false, []string{"go", "test", "./..."}},
		{"-exec= on the command", "go test -exec=/x/run -v ./...", nil, "", "/x/run", true, []string{"go", "test", "-v", "./..."}},
		{"--exec= on the command", "go test --exec=/x/run ./...", nil, "", "/x/run", true, []string{"go", "test", "./..."}},
		{"-exec X on the command", "go test -exec /x/run ./...", nil, "", "/x/run", true, []string{"go", "test", "./..."}},
		{"--exec X on the command", "go test --exec /x/run ./...", nil, "", "/x/run", true, []string{"go", "test", "./..."}},
		{"quoted program with arguments", `go test -exec "/x/run -q" ./...`, nil, "", "/x/run -q", true, []string{"go", "test", "./..."}},
		{"-exec= passed through", "go test", []string{"-exec=/x/run"}, "", "/x/run", true, []string{"go", "test"}},
		{"--exec= passed through", "go test", []string{"--exec=/x/run"}, "", "/x/run", true, []string{"go", "test"}},
		{"-exec X passed through", "go test", []string{"-exec", "/x/run"}, "", "/x/run", true, []string{"go", "test"}},
		{"--exec X passed through", "go test", []string{"--exec", "/x/run"}, "", "/x/run", true, []string{"go", "test"}},
		{"-exec= in GOFLAGS", "go test", nil, "-count=1 -exec=/x/run", "/x/run", false, []string{"go", "test"}},
		{"--exec= in GOFLAGS", "go test", nil, "--exec=/x/run -v", "/x/run", false, []string{"go", "test"}},
		// The command line wins over GOFLAGS, the arguments passed through over the
		// command.
		{"command over GOFLAGS", "go test -exec /cmd/run", nil, "-exec=/env/run", "/cmd/run", true, []string{"go", "test"}},
		{"passed through over the command", "go test -exec=/cmd/run {tests}", []string{"--exec", "/arg/run"}, "", "/arg/run", true, []string{"go", "test"}},
		// -exec after -args is the test binary's own argument.
		{"after -args", "go test ./... -args -exec /bin/x", nil, "", "", false, []string{"go", "test", "./...", "-args", "-exec", "/bin/x"}},
		{"before and after -args", "go test --exec /x/run ./... -args -exec=/bin/x", nil, "", "/x/run", true, []string{"go", "test", "./...", "-args", "-exec=/bin/x"}},
		// go test hands "--" and what follows to the test binary: not its -exec.
		{"after --", "go test ./... -- -exec foo", nil, "", "", false, []string{"go", "test", "./...", "--", "-exec", "foo"}},
		{"after -- passed through", "go test ./...", []string{"--", "-exec=foo"}, "", "", false, []string{"go", "test", "./...", "--", "-exec=foo"}},
		{"before and after --", "go test -exec /x/run ./... -- --exec=/bin/x", nil, "", "/x/run", true, []string{"go", "test", "./...", "--", "--exec=/bin/x"}},
		{"-- after -args", "go test ./... -args -- -exec /bin/x", nil, "", "", false, []string{"go", "test", "./...", "-args", "--", "-exec", "/bin/x"}},
		// Another go command, by path.
		{"go by path", "/opt/go1/bin/go test -exec=/x/run ./...", nil, "", "/x/run", true, []string{"/opt/go1/bin/go", "test", "./..."}},
		// A wrapper's command line is not changed.
		{"wrapper", "gotestsum -- -exec=/x/run ./...", nil, "", "/x/run", true, []string{"gotestsum", "--", "-exec=/x/run", "./..."}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := &Suite{Suite: config.Suite{Command: c.cmd}}
			if c.goflags != "" {
				s.Env = map[string]string{"GOFLAGS": c.goflags}
			}
			gc := goCommandOf(&RunContext{Suite: s, ExtraArgs: c.extra})
			if got := gc.userExec(); got != c.want {
				t.Errorf("userExec = %q, want %q", got, c.want)
			}
			if got := gc.onCommandLine("exec"); got != c.onCmd {
				t.Errorf("onCommandLine = %v, want %v", got, c.onCmd)
			}
			if got := gc.withoutExec(gc.argv); !reflect.DeepEqual(got, c.stripped) {
				t.Errorf("withoutExec = %q, want %q", got, c.stripped)
			}
			// GOFLAGS keeps everything but -exec.
			for _, w := range gc.goflags(func(a GoArg) bool { return a.Name == "exec" }) {
				if w == "-exec=/x/run" || w == "--exec=/x/run" || w == "-exec=/env/run" {
					t.Errorf("GOFLAGS keeps %q", w)
				}
			}
		})
	}
}

// The go command is the suite command's own whenever it runs go (any path whose base
// name is go); the auxiliary go commands (go list, go test -c, compile checks) run it.
func TestGoCommandBin(t *testing.T) {
	root := t.TempDir()
	for _, c := range []struct{ cmd, want string }{
		{"go test ./...", "go"},
		{"/opt/go1/bin/go test ./...", "/opt/go1/bin/go"},
		{"tools/go test ./...", filepath.Join(root, "tools", "go")},
		{"./go test", filepath.Join(root, "go")},
		{"gotestsum -- ./...", "go"},
		{"make test", "go"},
		{"", "go"},
	} {
		rc := &RunContext{Repo: &gitx.Repo{Root: root}, Suite: &Suite{Suite: config.Suite{Root: ".", Command: c.cmd}}}
		if got := goCommandOf(rc).bin; got != c.want {
			t.Errorf("%q: bin %q, want %q", c.cmd, got, c.want)
		}
		if got := parseGoFlags(rc).bin; got != c.want {
			t.Errorf("%q: goFlags.bin %q, want %q", c.cmd, got, c.want)
		}
		if argv, _ := checkArgv(rc, nil); argv[0] != c.want {
			t.Errorf("%q: compile check runs %q, want %q", c.cmd, argv[0], c.want)
		}
	}
}

// With a go command given by path and none on smates' PATH, go list (the -coverpkg
// files, build constraints) still runs: it uses the suite's go.
func TestGoAuxiliaryCommandsUseSuiteGo(t *testing.T) {
	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Skip("go not on PATH")
	}
	goBin, _ = filepath.Abs(goBin)
	t.Setenv("GOWORK", "")
	t.Setenv("GOFLAGS", "")
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	testutil.WriteTree(t, root, map[string]string{
		"go.mod":            "module example.com/m\n\ngo 1.22\n",
		"a/a.go":            "package a\n",
		"a/x_test.go":       "//go:build smates_never\n\npackage a\n",
		"a/a_test.go":       "package a\n",
		"b/b_linux.go":      "package b\n",
		"b/b_test.go":       "package b\n",
		"b/b_plan9_test.go": "package b\n",
	})
	t.Setenv("PATH", t.TempDir()) // no go there
	s := &Suite{Suite: config.Suite{Root: ".", Command: goBin + " test -coverpkg=./a"}, RepoRoot: root}
	rc := &RunContext{Repo: &gitx.Repo{Root: root}, Suite: s}
	files, err := goPackageFiles(context.Background(), rc, goCommandOf(rc).coverPkgs())
	if err != nil {
		t.Fatalf("go list with the suite's go: %v", err)
	}
	if !slices.Equal(files, []string{"a/a.go"}) {
		t.Errorf("files %v", files)
	}
	ex := excludedTestFiles(s, []string{"a/x_test.go", "a/a_test.go", "b/b_plan9_test.go"})
	if !ex["a/x_test.go"] || ex["a/a_test.go"] {
		t.Errorf("excluded %v", ex)
	}
	if runtime.GOOS != "plan9" && !ex["b/b_plan9_test.go"] {
		t.Errorf("excluded %v", ex)
	}
}

// Package recording runs the test binaries through smates' -exec wrapper whatever
// -exec the suite passes, and runs the user's program behind it: every spelling, in
// GOFLAGS, on the command line and among the arguments passed through.
func TestRecordPackagesUserExec(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("runs sh")
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name    string
		cmd     string
		extra   []string
		goflags bool // the -exec goes in GOFLAGS
	}{
		{"-exec= on the command", "go test -exec=%s", nil, false},
		{"--exec= on the command", "go test --exec=%s", nil, false},
		{"-exec X on the command", "go test -exec %s", nil, false},
		{"--exec X on the command", "go test --exec %s", nil, false},
		{"-exec= passed through", "go test", []string{"-exec=%s"}, false},
		{"--exec= passed through", "go test", []string{"--exec=%s"}, false},
		{"-exec X passed through", "go test", []string{"-exec", "%s"}, false},
		{"--exec X passed through", "go test", []string{"--exec", "%s"}, false},
		{"-exec= in GOFLAGS", "go test", []string{"-exec=%s"}, true},
		{"--exec= in GOFLAGS", "go test", []string{"--exec=%s"}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rc := goModule(t, map[string]string{
				"lib/lib.go":    "package lib\n\nfunc F() int { return 1 }\n",
				"lib/l_test.go": "package lib\n\nimport \"testing\"\n\nfunc TestF(t *testing.T) { F() }\n",
			})
			dir := t.TempDir()
			marker := filepath.Join(dir, "ran")
			user := testutil.WriteMode(t, dir, "user-exec.sh", "#!/bin/sh\ntouch '"+marker+"'\nexec \"$@\"\n", 0o755)
			fill := func(w string) string { return strings.ReplaceAll(w, "%s", user) }
			s := *rc.Suite
			s.Granularity = ""
			s.Command = fill(c.cmd)
			var extra []string
			for _, w := range c.extra {
				extra = append(extra, fill(w))
			}
			if c.goflags {
				s.Env = map[string]string{"GOFLAGS": extra[0]}
				extra = nil
			}
			rc.Suite, rc.ExtraArgs, rc.Self, rc.Units = &s, extra, self, []string{"lib"}
			res, err := runPackages(context.Background(), rc, rc.Units, false, "", nil)
			if err != nil {
				t.Fatalf("%v\n%s%s", err, rc.Stdout, rc.Stderr)
			}
			if o := res.Observations["lib"]; o == nil || o.Status != state.Pass {
				t.Errorf("lib not recorded through smates' wrapper: %+v\n%s%s", o, rc.Stdout, rc.Stderr)
			}
			if _, err := os.Stat(marker); err != nil {
				t.Errorf("the user's -exec program did not run: %v\n%s%s", err, rc.Stdout, rc.Stderr)
			}
		})
	}
}

// A wrapper command passing -exec cannot have it moved behind smates' wrapper: the run
// says so, rather than leaving the packages unrecorded without a reason.
func TestRecordPackagesWrapperExecWarns(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("runs sh")
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	rc := goModule(t, map[string]string{
		"lib/lib.go":    "package lib\n",
		"lib/l_test.go": "package lib\n\nimport \"testing\"\n\nfunc TestF(t *testing.T) {}\n",
	})
	s := *rc.Suite
	s.Granularity = ""
	// The wrapper hands its arguments to go test: the -exec reaches it.
	s.Command = "sh -c 'exec go test \"$@\"' sh -exec=env"
	rc.Suite, rc.Self, rc.Units = &s, self, []string{"lib"}
	res, err := runPackages(context.Background(), rc, rc.Units, false, "", nil)
	if err != nil {
		t.Fatalf("%v\n%s%s", err, rc.Stdout, rc.Stderr)
	}
	if !slices.ContainsFunc(res.Warnings, func(w string) bool { return strings.Contains(w, "passes -exec") }) {
		t.Errorf("no warning about the wrapper's -exec: %q", res.Warnings)
	}
	if o := res.Observations["lib"]; o != nil && o.Status == state.Pass {
		t.Errorf("must keep: a package run without smates' wrapper is recorded as passed: %+v", o)
	}
}

// A test function recording does not run is recorded as passed only when it is known
// to be compiled (an example without output); one not shown to be built is left
// unrecorded, so it runs again.
func TestFuncPlan(t *testing.T) {
	compiled := map[string]bool{"TestRun": true, "ExampleSilent": false}
	listed := map[string]bool{"TestRun": true}
	for _, c := range []struct {
		name  string
		runs  map[string]bool
		known bool
		fn    string
		want  int
	}{
		{"compiled and run", compiled, true, "TestRun", funcRun},
		{"compiled, never run (example without output)", compiled, true, "ExampleSilent", funcCompiledOnly},
		// Must keep: a function of a file this configuration does not compile.
		{"not compiled", compiled, true, "TestTagged", funcUnconfirmed},
		{"listed by the binary", listed, false, "TestRun", funcRun},
		// Must keep: the test files could not tell and -test.list does not name it.
		{"not listed by the binary", listed, false, "TestTagged", funcUnconfirmed},
		{"not listed, nothing listed", map[string]bool{}, false, "ExampleSilent", funcUnconfirmed},
	} {
		if got := funcPlan(c.runs, c.known, c.fn); got != c.want {
			t.Errorf("%s: %d, want %d", c.name, got, c.want)
		}
	}
}

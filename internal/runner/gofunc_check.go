package runner

// Compile checks for granularity: func. A function unit depends only on the bodies it
// executed, but go test fails a whole package, and every package importing it, when
// any of its files or its test files does not compile or vet: a body edit no test
// executes (an unused helper returning the wrong type, an import left unused) would
// otherwise select nothing while go test fails. Each package with test functions
// therefore also has a compile-check unit, "dir#(build)", which depends on the content
// of every file of the package's static closure (its Go and test files and those of
// the packages it imports, embedded files) and runs as go test with the test binaries
// built and vetted but not run.

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/dotneet/smates/internal/state"
)

// BuildCheck is the name of a package's compile-check unit (FuncUnit(dir, BuildCheck)).
// It is not a Go identifier, so no test function has it.
const BuildCheck = "(build)"

// IsBuildCheck reports whether unit is a package's compile-check unit.
func IsBuildCheck(unit string) bool {
	_, name, ok := SplitFuncUnit(unit)
	return ok && name == BuildCheck
}

// GoCheckEnv marks a smates process started by go test -exec for a compile check: it
// exits at once without running the test binary (see IsGoCheck).
const GoCheckEnv = "SMATES_GOCHECK"

// IsGoCheck reports whether this process was started as the -exec program of a
// compile check; it then has nothing to do but exit 0.
func IsGoCheck() bool { return os.Getenv(GoCheckEnv) != "" }

// checkArgv is the go test command compiling and vetting the test binaries of pkgs
// without running them, and the environment it needs: the build flags and -vet setting
// of GOFLAGS, the suite command and the arguments passed through, and -exec=smates in
// check mode (when smates' path is known; otherwise the binaries run with -run '^$',
// which still runs TestMain and init functions).
func checkArgv(rc *RunContext, pkgs []string) (argv, env []string) {
	g := parseGoFlags(rc)
	argv = []string{"go", "test", "-run=^$", "-count=1"}
	env = []string{"GOFLAGS=" + checkGoflags(rc)}
	if exe := checkExec(rc); exe != "" {
		argv = append(argv, "-exec="+exe)
		env = append(env, GoCheckEnv+"=1")
	}
	argv = append(argv, g.build...)
	if g.vet != "" {
		argv = append(argv, "-vet="+g.vet)
	}
	return append(argv, pkgs...), env
}

// checkGoflags is the suite's GOFLAGS without test flags (-run, -json, -coverprofile,
// -exec, ...), which would change what the check runs or prints; -vet is kept.
func checkGoflags(rc *RunContext) string {
	var kept []string
	for _, w := range strings.Fields(Getenv(suiteEnv(rc.Suite), "GOFLAGS")) {
		name, _, _ := strings.Cut(strings.TrimLeft(w, "-"), "=")
		if name != "vet" && (goTestValueFlags[name] || goTestBoolFlags[name] || strings.HasPrefix(name, "test.")) {
			continue
		}
		kept = append(kept, w)
	}
	return strings.Join(kept, " ")
}

// checkExec is the -exec value running smates in check mode ("" when its path is not
// known or cannot be quoted for go).
func checkExec(rc *RunContext) string {
	switch {
	case rc.Self == "" || strings.ContainsAny(rc.Self, `'"`):
		return ""
	case strings.ContainsAny(rc.Self, " \t"):
		return "'" + rc.Self + "'" // go splits -exec with shell-like quoting
	}
	return rc.Self
}

// runBuildChecks runs the compile checks of package directories dirs, in one go test
// invocation (chunked when the arguments are long). A package go test reports as
// failing to build or vet ([build failed], [setup failed]) fails its check with that
// output; when the invocation failed before reporting any package, every check fails
// with its output. A package whose Go files build constraints all exclude (a wrapper
// command's flags are unseen, see buildFailures.excluded) is a skip, not a failure.
// Checks have no dependencies of their own: recording gives them
// their known ones (the static closure).
func runBuildChecks(ctx context.Context, rc *RunContext, dirs []string) (*RunResult, error) {
	res := &RunResult{Observations: map[string]*Observation{}}
	start := time.Now()
	for _, args := range chunk(pkgArgs(rc.Suite, dirs)) {
		argv, env := checkArgv(rc, args)
		failed := &buildFailures{}
		var out bytes.Buffer
		began := time.Now()
		code, err := rc.ExecIn(ctx, rc.Dir(), argv, env, &out, &out)
		failed.Write(out.Bytes())
		failed.Write([]byte("\n"))
		fmt.Fprintf(rc.Stdout, "── smates: compile check of %s (exit %d, %s)\n", strings.Join(args, " "), code, time.Since(began).Round(time.Millisecond))
		rc.Stdout.Write(out.Bytes())
		if err != nil {
			return res, err
		}
		var chunkDirs []string
		for _, d := range dirs {
			if slices.Contains(args, pkgArg(rc.Suite, d)) {
				chunkDirs = append(chunkDirs, d)
			}
		}
		if failed.onlyExcluded(rc, chunkDirs, code) {
			code = 0 // packages build constraints all exclude: no failure (see buildFailures.excluded)
		}
		res.ExitCode = max(res.ExitCode, code)
		for _, d := range chunkDirs {
			ip := failed.key(rc, d)
			o := &Observation{Files: map[string]bool{}, Status: state.Pass}
			switch {
			case failed.excluded[ip]:
				// Nothing of it compiles in this configuration: not proven to build.
				o.Status = state.Skip
			case failed.paths[ip] || failed.status[ip] == state.Fail:
				o.Status = state.Fail
				if o.Failure = failed.output[ip]; o.Failure == "" {
					o.Failure = string(lastMessage(out.Bytes())) // a package it imports does not build
				}
			case code != 0 && len(failed.status) == 0 && len(failed.paths) == 0:
				// go test failed before reporting any package (go.mod does not parse).
				o.Status, o.Failure = state.Fail, string(lastMessage(out.Bytes()))
			}
			res.Observations[FuncUnit(d, BuildCheck)] = o
		}
	}
	res.Duration = time.Since(start)
	for _, o := range res.Observations {
		o.DurationMs = res.Duration.Milliseconds() / int64(len(res.Observations))
	}
	return res, nil
}

package runner

// Per-test-function granularity for Go (suite option granularity: func).
//
// A unit is a top-level test function, "dir#TestName" (TestXxx, FuzzXxx with its seed
// corpus, ExampleXxx), found by parsing the package's _test.go files. While recording,
// each package's test binary is built once with coverage over the repository packages
// it links (go test -c -cover -coverpkg=...), and every selected function runs in its
// own process with -test.run '^Name$', a coverage profile and a test log. A function
// then depends on:
//
//   - the content of every non-test Go file in which it executed a block (coverage);
//   - the declarations (depkey.Decls: the file without function bodies) of every Go
//     file of the package's import closure, so that constants, variables, types,
//     signatures and method sets, which coverage cannot see, still count;
//   - the content of Go files of packages the binary does not link (packages a test
//     builds or runs as a subprocess);
//   - the content of its own _test.go file, of the package's test files it references
//     by name (helpers), and of test files that run code for every test (TestMain,
//     init, package-level variables initialized by calls); the declarations of the
//     other test files of the package;
//   - the listing of the package directory (a file added to it selects every function);
//   - non-Go files of the static closure (go.mod, go.sum, embedded files), what its test
//     log recorded (files read, directories listed, environment variables), and what
//     processes it started read — as for a package.
//
// Coverage also counts what re-executions of the test binary ran (the helper-process
// pattern, exec.Command(os.Args[0], "-test.run=...")): the binary carries an init
// that hands them the function's coverage directory (see childCoverSource).
//
// When the binary cannot be built with coverage (or the suite command is not a plain
// `go test`), the package is recorded as a whole: every selected function gets the
// package's observation, with every Go file of the closure as content.
//
// Every package with functions also has a compile-check unit, "dir#(build)", which
// depends on the content of the package's whole static closure and builds and vets its
// test binary without running it (gofunc_check.go): go test fails a package that does
// not compile, whichever bodies its tests execute.
//
// Without recording, selected functions run with go test -run '^(A|B)$' and their
// outcomes are read from go test's output.

import (
	"context"
	"path"
	"path/filepath"
	"strings"

	"github.com/dotneet/smates/internal/config"
)

func funcGranularity(s *Suite) bool { return s.Granularity == config.GranularityFunc }

// FuncUnit is the unit ID of test function name in package directory dir.
func FuncUnit(dir, name string) string { return dir + "#" + name }

// SplitFuncUnit splits a Go test function unit ID into its package directory and name.
func SplitFuncUnit(unit string) (dir, name string, ok bool) {
	i := strings.LastIndexByte(unit, '#')
	if i < 0 {
		return "", "", false
	}
	return unit[:i], unit[i+1:], true
}

// funcUnits lists the test functions of the test files, then the compile-check unit
// (BuildCheck) of each of their packages. A file that does not parse yields its
// package directory as a unit (the package does not compile; it runs, and fails, as a
// whole). Files the suite's configuration does not compile (build constraints, see
// excludedTestFiles) have no units.
func funcUnits(s *Suite, testFiles []string) []string {
	seen := map[string]bool{}
	var out, checks []string
	add := func(u string) {
		if !seen[u] {
			seen[u] = true
			out = append(out, u)
		}
	}
	excluded := excludedTestFiles(s, testFiles)
	for _, f := range testFiles {
		if excluded[f] {
			continue
		}
		d := path.Dir(f)
		if c := FuncUnit(d, BuildCheck); !seen[c] {
			seen[c] = true
			checks = append(checks, c)
		}
		tf, err := readTestFile(s.RepoRoot, f)
		if err != nil {
			add(d)
			continue
		}
		for _, n := range tf.Tests {
			add(FuncUnit(d, n))
		}
	}
	return append(out, checks...)
}

// splitUnits separates package units, function units (grouped by directory, in order
// of first appearance) and the directories of compile checks.
func splitUnits(units []string) (pkgs, dirs []string, funcs map[string][]string, checks []string) {
	funcs = map[string][]string{}
	for _, u := range units {
		d, n, ok := SplitFuncUnit(u)
		switch {
		case !ok:
			pkgs = append(pkgs, u)
			continue
		case n == BuildCheck:
			checks = append(checks, d)
			continue
		}
		if _, seen := funcs[d]; !seen {
			dirs = append(dirs, d)
		}
		funcs[d] = append(funcs[d], n)
	}
	return pkgs, dirs, funcs, checks
}

// Commands implements Commander (for --dry-run).
func (goTest) Commands(rc *RunContext) ([][]string, error) {
	if !funcGranularity(rc.Suite) {
		return nil, nil
	}
	pkgs, dirs, funcs, checks := splitUnits(rc.Units)
	var out [][]string
	for _, args := range chunk(pkgArgs(rc.Suite, checks)) {
		if len(args) > 0 {
			// With its environment, so that it can be run as printed.
			argv, env := checkArgv(rc, args)
			out = append(out, append(append([]string{"env"}, env...), argv...))
		}
	}
	if rc.All && !rc.Suite.Narrowed {
		argv, err := rc.CommandLine(nil, allPkgArgs(rc))
		return append(out, argv), err
	}
	if len(pkgs) > 0 {
		argv, err := rc.CommandLine(nil, pkgArgs(rc.Suite, pkgs))
		if err != nil {
			return nil, err
		}
		out = append(out, argv)
	}
	for _, inv := range plainInvocations(dirs, funcs) {
		argv, err := rc.CommandLine(runFlag(inv.run), pkgArgs(rc.Suite, inv.dirs))
		if err != nil {
			return nil, err
		}
		out = append(out, argv)
	}
	return out, nil
}

// merge adds r to res.
func (res *RunResult) merge(r *RunResult) {
	if r == nil {
		return
	}
	res.ExitCode = max(res.ExitCode, r.ExitCode)
	res.Duration += r.Duration
	res.Warnings = append(res.Warnings, r.Warnings...)
	for u, o := range r.Observations {
		if res.Observations == nil {
			res.Observations = map[string]*Observation{}
		}
		res.Observations[u] = o
	}
}

func runFuncs(ctx context.Context, rc *RunContext) (*RunResult, error) {
	pkgs, dirs, funcs, checks := splitUnits(rc.Units)
	res := &RunResult{}
	if len(checks) > 0 {
		r, err := runBuildChecks(ctx, rc, checks)
		res.merge(r)
		if err != nil {
			return res, err
		}
	}
	if len(pkgs) > 0 {
		r, err := runPackages(ctx, rc, pkgs, false, "", nil)
		res.merge(r)
		if err != nil {
			return res, err
		}
	}
	if len(dirs) == 0 {
		return res, nil
	}
	var r *RunResult
	var err error
	if rc.Record {
		r, err = recordFuncs(ctx, rc, dirs, funcs)
		if err == nil {
			addCoverPkgDeps(ctx, rc, r)
		}
	} else {
		r, err = runFuncsPlain(ctx, rc, dirs, funcs)
	}
	res.merge(r)
	return res, err
}

// runFuncsPlain runs the selected functions with go test -run and reports each one's
// outcome as go test printed it.
func runFuncsPlain(ctx context.Context, rc *RunContext, dirs []string, funcs map[string][]string) (*RunResult, error) {
	res := &RunResult{Observations: map[string]*Observation{}}
	invs := plainInvocations(dirs, funcs)
	if rc.All && !rc.Suite.Narrowed {
		invs = []goInvocation{{dirs: dirs}}
	}
	for _, inv := range invs {
		oc := &goOutcomes{}
		all := rc.All && !rc.Suite.Narrowed
		r, err := runPackages(ctx, rc, inv.dirs, all, inv.run, oc)
		res.merge(r)
		if err != nil {
			return res, err
		}
		for _, d := range inv.dirs {
			ip := dirImportPath(rc.Repo.Root, d)
			for _, n := range funcs[d] {
				st, inferred, ran := oc.status(ip, n)
				if !ran {
					continue
				}
				res.Observations[FuncUnit(d, n)] = &Observation{Files: map[string]bool{}, Status: st, Inferred: inferred, Failure: oc.failure(ip, n)}
			}
		}
	}
	return res, nil
}

// dirImportPath is the import path of a repo-relative package directory ("" if unknown).
func dirImportPath(root, dir string) string {
	abs := filepath.Join(root, filepath.FromSlash(dir))
	modDir, mod := findModule(root, abs)
	return importPath(modDir, mod, abs)
}

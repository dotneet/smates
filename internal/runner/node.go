package runner

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/dotneet/smates/internal/config"
	"github.com/dotneet/smates/internal/gitx"
	"github.com/dotneet/smates/internal/trace"
)

func init() {
	Register(vitest{})
	Register(jest{})
	Register(nodeTest{})
}

var jsTestExts = "{js,mjs,cjs,jsx,ts,mts,cts,tsx}"

var jsExclude = []string{"**/node_modules/**", "**/dist/**", "**/build/**", "**/coverage/**", "**/.next/**", "**/out/**"}

// runNode runs a Node-based runner, tracing it when recording, and hands the V8
// coverage to a NODE_V8_COVERAGE the user set.
func runNode(ctx context.Context, rc *RunContext, extra, testArgs []string, opt traceOptions) (*RunResult, error) {
	res, err := runNodeAttempt(ctx, rc, extra, testArgs, opt)
	exportNodeV8(rc, res)
	return res, err
}

// exportNodeV8 copies the V8 coverage of a recorded run (its final attempt) into the
// user's NODE_V8_COVERAGE.
func exportNodeV8(rc *RunContext, res *RunResult) {
	if res != nil && rc.Record {
		res.Warnings = append(res.Warnings, exportV8(rc)...)
	}
}

// runNodeAttempt is runNode without exporting the coverage: one attempt that may be
// retried (runNodeWithFallback).
func runNodeAttempt(ctx context.Context, rc *RunContext, extra, testArgs []string, opt traceOptions) (*RunResult, error) {
	env := resultsEnv(rc)
	if rc.Record {
		if err := os.MkdirAll(trace.V8Dir(rc.TraceDir), 0o755); err != nil {
			return nil, err
		}
		env = trace.NodeEnv(rc.TraceDir, rc.Repo.Root, rc.Shims(), suiteEnv(rc.Suite))
	}
	start := time.Now()
	code, err := runBatches(ctx, rc, extra, testArgs, env)
	res := &RunResult{ExitCode: code, Duration: time.Since(start)}
	if err != nil {
		return res, err
	}
	if !rc.Record {
		res.Observations, res.Warnings = reported(rc)
		return res, nil
	}
	res.Observations, res.Warnings, err = observe(rc, code, opt)
	return res, err
}

// ---- vitest ----

type vitest struct{}

func (vitest) Name() string { return "vitest" }

func (vitest) Defaults(repo *gitx.Repo, root string) Defaults {
	dir := filepath.Join(repo.Root, filepath.FromSlash(root))
	return Defaults{
		Tests:   []string{"**/*.{test,spec}." + jsTestExts},
		Exclude: jsExclude,
		Inputs: append([]string{
			"vitest.config.*", "vite.config.*", "vitest.workspace.*", "vitest.setup.*", "/vitest.config.*", "/vitest.workspace.*",
		}, jsLockInputs(root)...),
		Command: jsBinCommand(repo.Root, dir, "vitest", "run"),
	}
}

func (vitest) Units(s *Suite, testFiles []string) []string { return unitsAreFiles(testFiles) }

func (vitest) Run(ctx context.Context, rc *RunContext) (*RunResult, error) {
	// Vitest runs each test file in a fresh worker (isolate: true, the default), so V8
	// coverage per worker attributes exactly. Per-file status comes from a reporter we
	// add next to the default one, unless the user configures reporters (CLI reporters
	// would replace theirs); then a failing run marks every recorded file failed. The
	// reporter is also added without recording (no tracing), for per-file status.
	var extra []string
	if rc.TraceDir != "" && !userConfigures(rc, []string{"vitest.config.*", "vite.config.*", "vitest.workspace.*"}, "reporters", "--reporter") {
		// Older Vitest loads reporters through Vite, which only serves files inside
		// the project, so the reporter is placed in node_modules/.cache.
		if path, cleanup := placeInProject(rc, "vitest-reporter.mjs"); path != "" {
			defer cleanup()
			extra = []string{"--reporter=default"}
			if os.Getenv("GITHUB_ACTIONS") == "true" {
				extra = append(extra, "--reporter=github-actions")
			}
			extra = append(extra, "--reporter="+path)
		}
	}
	if rc.Record && config.Bool(rc.Suite.Isolate, false) {
		// The project runs files in shared workers (isolate: false): run each file on
		// its own while recording.
		return runIsolatedNode(ctx, rc, func(u string) []string {
			return append(append([]string{}, extra...), relToSuite(rc.Suite, u))
		}, traceOptions{})
	}
	return runNodeWithFallback(ctx, rc, extra, rc.RelArgs(""), traceOptions{})
}

// runNodeWithFallback runs with smates' injected arguments and, if the run failed
// before any test file ran (an injected reporter the runner rejects, for example),
// runs again without them: smates must never be the reason a test run fails.
// The V8 coverage goes to the user's NODE_V8_COVERAGE once, from the attempt kept: a
// rejected first attempt's files are discarded with its trace.
func runNodeWithFallback(ctx context.Context, rc *RunContext, injected, testArgs []string, opt traceOptions) (*RunResult, error) {
	res, err := runNodeAttempt(ctx, rc, injected, testArgs, opt)
	if err != nil || len(injected) == 0 || res.ExitCode == 0 || len(res.Observations) > 0 || rc.TraceDir == "" {
		exportNodeV8(rc, res)
		return res, err
	}
	if ctx.Err() != nil {
		exportNodeV8(rc, res)
		return res, ctx.Err() // interrupted, not rejected
	}
	if len(trace.Results(rc.TraceDir, rc.Repo.Root)) > 0 {
		exportNodeV8(rc, res)
		return res, nil
	}
	fmt.Fprintf(rc.Stderr, "smates: %s failed before running any test with smates' reporter; retrying without it\n", rc.Suite.Runner)
	if err := os.RemoveAll(rc.TraceDir); err != nil {
		return res, err
	}
	if err := os.MkdirAll(rc.TraceDir, 0o755); err != nil {
		return res, err
	}
	keep := []string{}
	for _, a := range injected {
		if a == "--runTestsByPath" {
			keep = append(keep, a)
		}
	}
	res, err = runNode(ctx, rc, keep, testArgs, opt)
	if res != nil {
		res.Warnings = append(res.Warnings, "smates' reporter could not be loaded; per-file pass/fail falls back to the overall exit code")
	}
	return res, err
}

// placeInProject copies a shim into <node_modules>/.cache/smates of the suite (or the
// nearest ancestor up to the repository root) and returns its path and a cleanup
// function; "" when there is no node_modules directory.
func placeInProject(rc *RunContext, shim string) (string, func()) {
	data, err := os.ReadFile(filepath.Join(rc.ShimDir, shim))
	if err != nil {
		return "", func() {}
	}
	for d := rc.Dir(); ; d = filepath.Dir(d) {
		nm := filepath.Join(d, "node_modules")
		if fi, err := os.Stat(nm); err == nil && fi.IsDir() {
			dir := filepath.Join(nm, ".cache", "smates")
			if err := os.MkdirAll(dir, 0o755); err != nil {
				return "", func() {}
			}
			p := filepath.Join(dir, fmt.Sprintf("%d-%s", os.Getpid(), shim))
			if err := os.WriteFile(p, data, 0o644); err != nil {
				return "", func() {}
			}
			return p, func() { os.Remove(p) }
		}
		if len(d) <= len(rc.Repo.Root) || d == filepath.Dir(d) {
			return "", func() {}
		}
	}
}

// userConfigures reports whether the user set a runner option we would otherwise add
// (in config files matching globs in the suite directory or its ancestors up to the
// repository root, package.json, or the command line), so smates leaves it alone. An
// explicit config file on the command line counts as configuring everything: smates
// cannot tell what it contains without evaluating it.
func userConfigures(rc *RunContext, globs []string, key, flag string) bool {
	for _, a := range append(append([]string{}, rc.ExtraArgs...), strings.Fields(rc.Suite.Command)...) {
		if strings.HasPrefix(a, flag) || a == "--config" || strings.HasPrefix(a, "--config=") || a == "-c" ||
			a == "--projects" || a == "--root" || a == "--dir" || a == "--rootDir" || strings.HasPrefix(a, "--rootDir=") {
			return true
		}
	}
	var dirs []string
	for d := rc.Dir(); ; d = filepath.Dir(d) {
		dirs = append(dirs, d)
		if len(d) <= len(rc.Repo.Root) || d == filepath.Dir(d) {
			break
		}
	}
	var candidates []string
	for i, d := range dirs {
		for _, g := range globs {
			candidates = append(candidates, filepath.Join(d, g))
		}
		if i == 0 {
			candidates = append(candidates, filepath.Join(d, "package.json"))
		}
	}
	for _, g := range candidates {
		matches, _ := filepath.Glob(g)
		for _, m := range matches {
			data, err := os.ReadFile(m)
			if err != nil {
				continue
			}
			if strings.Contains(string(data), key) && (!strings.HasSuffix(m, "package.json") || strings.Contains(string(data), `"`+key+`"`)) {
				return true
			}
		}
	}
	return false
}

// ---- jest ----

type jest struct{}

func (jest) Name() string { return "jest" }

func (jest) Defaults(repo *gitx.Repo, root string) Defaults {
	dir := filepath.Join(repo.Root, filepath.FromSlash(root))
	return Defaults{
		Tests:   []string{"**/__tests__/**/*." + jsTestExts, "**/*.{test,spec}." + jsTestExts},
		Exclude: append([]string{"**/__tests__/**/__fixtures__/**", "**/__tests__/**/fixtures/**", "**/__mocks__/**"}, jsExclude...),
		Inputs: append([]string{
			"jest.config.*", "/jest.config.*", "jest.setup.*", "babel.config.*", ".babelrc", ".babelrc.*", "/babel.config.*", ".swcrc",
		}, jsLockInputs(root)...),
		Command: jsBinCommand(repo.Root, dir, "jest", ""),
	}
}

func (jest) Units(s *Suite, testFiles []string) []string { return unitsAreFiles(testFiles) }

func (jest) Run(ctx context.Context, rc *RunContext) (*RunResult, error) {
	// Per-file status comes from a reporter added next to the default one, with or
	// without recording.
	var extra []string
	if rc.TraceDir != "" && !userConfigures(rc, []string{"jest.config.*"}, "reporters", "--reporters") {
		extra = []string{"--reporters=default", "--reporters=" + filepath.Join(rc.ShimDir, "jest-reporter.cjs")}
	}
	// Jest workers run many test files each, which merges their dependencies. With
	// workerIdleMemoryLimit=0 jest restarts a worker after every file, and it never runs
	// tests in band (in its main process, which also crawls the project).
	inBand := userConfigures(rc, []string{"jest.config.*"}, "runInBand", "--runInBand") || hasArg(rc, "-i")
	if rc.Record && !inBand && !userConfigures(rc, []string{"jest.config.*"}, "workerIdleMemoryLimit", "--workerIdleMemoryLimit") {
		// 0 (restart after every file) exists since Jest 30; Jest 29 rejects it, so a
		// tiny limit is used there (almost always a restart per file). Older versions
		// have no such option.
		switch major := packageMajor(rc, "jest"); {
		case major >= 30:
			extra = append(extra, "--workerIdleMemoryLimit=0")
		case major == 29:
			extra = append(extra, "--workerIdleMemoryLimit=1KB")
		}
	}
	// In band, the process that runs tests also crawls the project: its directory
	// listings are not dependencies of the tests.
	opt := traceOptions{dropDirs: inBand}
	if rc.Record && config.Bool(rc.Suite.Isolate, false) {
		return runIsolatedNode(ctx, rc, func(u string) []string {
			return append(append([]string{}, extra...), "--runTestsByPath", relToSuite(rc.Suite, u))
		}, opt)
	}
	if !rc.All {
		extra = append(extra, "--runTestsByPath")
	}
	return runNodeWithFallback(ctx, rc, extra, rc.RelArgs(""), opt)
}

// packageMajor returns the major version of an installed npm package (0 if unknown),
// looking in node_modules from the suite directory up to the repository root.
func packageMajor(rc *RunContext, name string) int {
	for d := rc.Dir(); ; d = filepath.Dir(d) {
		data, err := os.ReadFile(filepath.Join(d, "node_modules", name, "package.json"))
		if err == nil {
			var pj struct {
				Version string `json:"version"`
			}
			if json.Unmarshal(data, &pj) == nil {
				major, _, _ := strings.Cut(pj.Version, ".")
				n, _ := strconv.Atoi(major)
				return n
			}
		}
		if len(d) <= len(rc.Repo.Root) || d == filepath.Dir(d) {
			return 0
		}
	}
}

func hasArg(rc *RunContext, arg string) bool {
	for _, a := range append(append([]string{}, rc.ExtraArgs...), strings.Fields(rc.Suite.Command)...) {
		if a == arg {
			return true
		}
	}
	return false
}

// runIsolatedNode runs each unit in its own runner process while recording.
func runIsolatedNode(ctx context.Context, rc *RunContext, args func(string) []string, opt traceOptions) (*RunResult, error) {
	// rc.Units lists every unit even when rc.All is set.
	units := rc.Units
	if err := os.MkdirAll(trace.V8Dir(rc.TraceDir), 0o755); err != nil {
		return nil, err
	}
	env := trace.NodeEnv(rc.TraceDir, rc.Repo.Root, rc.Shims(), suiteEnv(rc.Suite))
	start := time.Now()
	jobs := runtime.NumCPU()
	if hasArg(rc, "--runInBand") || hasArg(rc, "-i") || hasArg(rc, "--no-file-parallelism") {
		jobs = 1
	}
	codes, err := isolatedRun(ctx, rc, units, jobs, func(u string, _ int) ([]string, []string, error) {
		argv, err := rc.CommandLine(nil, args(u))
		return argv, env, err
	})
	worst := worstCode(codes)
	res := &RunResult{ExitCode: worst, Duration: time.Since(start)}
	if err != nil {
		return res, err
	}
	obs, warns, err := observe(rc, worst, opt)
	for u, o := range obs {
		if c, ok := codes[u]; ok {
			setUnitExit(o, c)
		}
	}
	res.Observations, res.Warnings = obs, append(warns, exportV8(rc)...)
	return res, err
}

// ---- node --test ----

type nodeTest struct{}

func (nodeTest) Name() string { return "node" }

func (nodeTest) Defaults(repo *gitx.Repo, root string) Defaults {
	return Defaults{
		// Node's default patterns, plus TypeScript variants (type stripping).
		Tests: []string{
			"**/*.test.{js,mjs,cjs,ts,mts,cts}", "**/*-test.{js,mjs,cjs,ts,mts,cts}", "**/*_test.{js,mjs,cjs,ts,mts,cts}",
			"**/test-*.{js,mjs,cjs,ts,mts,cts}", "**/test.{js,mjs,cjs}", "**/test/**/*.{js,mjs,cjs}",
		},
		Exclude: append([]string{"**/test/**/fixtures/**", "**/test/**/helpers/**", "**/test/**/_*"}, jsExclude...),
		Inputs:  jsLockInputs(root),
		Command: "node --test",
	}
}

func (nodeTest) Units(s *Suite, testFiles []string) []string { return unitsAreFiles(testFiles) }

func (nodeTest) Run(ctx context.Context, rc *RunContext) (*RunResult, error) {
	// node --test runs every file in its own process: exit codes are per file. Files are
	// always passed explicitly: node's own discovery would also run helpers that smates
	// does not consider tests (test/helpers/*.js) as if they were test files.
	explicit := *rc
	explicit.All = false
	return runNode(ctx, &explicit, nil, rc.RelArgs(""), traceOptions{trustProcStatus: true})
}

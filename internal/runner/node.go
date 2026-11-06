package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"slices"
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

// nodeTestExts are the extensions of node --test's default patterns.
var nodeTestExts = "{js,mjs,cjs,ts,mts,cts}"

// jsExclude are the default excludes of bun suites.
var jsExclude = []string{"**/node_modules/**", "**/dist/**", "**/build/**", "**/coverage/**", "**/.next/**", "**/out/**"}

// jsOutputExclude are the default excludes of vitest, jest and node --test suites. These
// runners collect test files everywhere but node_modules (and, before Vitest 4, dist and
// a few tool directories): a test under a build/, out/ or coverage/ directory git tracks
// (src/build/compile.test.ts) runs, so it is a test. Output git ignores is never seen
// anyway. dist/ stays excluded as where compiled copies of the tests go (tsc's outDir).
var jsOutputExclude = []string{"**/node_modules/**", "**/dist/**"}

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
		env = trace.Env(rc.TraceDir, rc.Repo.Root, rc.Shims(), suiteEnv(rc.Suite))
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
		Exclude: jsOutputExclude,
		Inputs: append([]string{
			"vitest.config.*", "vite.config.*", "vitest.*.config.*", "vitest.workspace.*", "vitest.setup.*", "/vitest.config.*", "/vitest.workspace.*",
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
	keep := []string{}
	for _, a := range injected {
		if a == "--runTestsByPath" {
			keep = append(keep, a)
		}
	}
	// A run that loaded the reporter, or would run again with the same arguments,
	// failed on its own.
	if len(keep) == len(injected) || trace.ReporterLoaded(rc.TraceDir) || len(trace.Results(rc.TraceDir, rc.Repo.Root)) > 0 {
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
		Exclude: append([]string{"**/__tests__/**/__fixtures__/**", "**/__tests__/**/fixtures/**", "**/__mocks__/**"}, jsOutputExclude...),
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
	env := trace.Env(rc.TraceDir, rc.Repo.Root, rc.Shims(), suiteEnv(rc.Suite))
	start := time.Now()
	// A command that runs files one after another (tests sharing a database or ports)
	// is recorded one file at a time too; record_jobs overrides this.
	jobs := runtime.NumCPU()
	if slices.ContainsFunc([]string{"--runInBand", "-i", "--no-file-parallelism", "--test-isolation=none",
		"--experimental-test-isolation=none", "--test-concurrency=1"}, func(a string) bool { return hasArg(rc, a) }) {
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
		// Node's default patterns, with the TypeScript extensions Node adds to every one of
		// them when it strips types (23.6+).
		Tests: []string{
			"**/*.test." + nodeTestExts, "**/*-test." + nodeTestExts, "**/*_test." + nodeTestExts,
			"**/test-*." + nodeTestExts, "**/test." + nodeTestExts, "**/test/**/*." + nodeTestExts,
		},
		Exclude: append([]string{"**/test/**/fixtures/**", "**/test/**/helpers/**", "**/test/**/_*"}, jsOutputExclude...),
		Inputs:  jsLockInputs(root),
		Command: "node --test",
	}
}

func (nodeTest) Units(s *Suite, testFiles []string) []string { return unitsAreFiles(testFiles) }

func (nodeTest) Run(ctx context.Context, rc *RunContext) (*RunResult, error) {
	// node --test runs every file in its own process: exit codes are per file. Files are
	// always passed explicitly: node's own discovery would also run helpers that smates
	// does not consider tests (test/helpers/*.js) as if they were test files.
	esc := nodeArgEscaper(ctx, rc)
	if rc.Record && config.Bool(rc.Suite.Isolate, false) {
		// --test-isolation=none runs every file in one process: run each file on its own
		// while recording.
		return runIsolatedNode(ctx, rc, func(u string) []string {
			return []string{esc(relToSuite(rc.Suite, u))}
		}, traceOptions{trustProcStatus: true})
	}
	explicit := *rc
	explicit.All = false
	args := rc.RelArgs("")
	for i, a := range args {
		args[i] = esc(a)
	}
	return runNode(ctx, &explicit, nil, args, traceOptions{trustProcStatus: true})
}

// nodeGlobChars are the characters that make a path a pattern of Node's glob.
const nodeGlobChars = `[]*?(){}+@\`

// nodeArgEscaper returns how test files are passed to node --test. Node 21+ expands its
// file arguments as globs, so a path with glob syntax (Next.js app/[slug]/page.test.js)
// would match other files or none, and the run would pass without running it; such
// paths are escaped there. The version is asked of the suite command's node (or the one
// on PATH) only when a path needs it; when it cannot be told, paths are escaped: on an
// older Node that fails loudly (file not found) instead of passing silently.
func nodeArgEscaper(ctx context.Context, rc *RunContext) func(string) string {
	same := func(p string) string { return p }
	if !slices.ContainsFunc(rc.Units, func(u string) bool { return strings.ContainsAny(u, nodeGlobChars) }) {
		return same
	}
	if major := nodeMajor(ctx, rc); major > 0 && major < 21 {
		return same
	}
	return escapeNodeGlob
}

// nodeMajor returns the major version of the node the suite command runs (the command
// itself when it starts with node, else node on PATH), 0 if unknown.
func nodeMajor(ctx context.Context, rc *RunContext) int {
	bin := "node"
	if words, err := SplitWords(rc.Suite.Command); err == nil {
		for _, w := range words {
			if isAssignment(w) {
				continue
			}
			if strings.TrimSuffix(filepath.Base(w), ".exe") == "node" {
				bin = w
			}
			break
		}
	}
	var out bytes.Buffer
	if code, err := rc.ExecIn(ctx, rc.Dir(), []string{bin, "--version"}, nil, &out, io.Discard); err != nil || code != 0 {
		return 0
	}
	major, _, _ := strings.Cut(strings.TrimPrefix(strings.TrimSpace(out.String()), "v"), ".")
	n, _ := strconv.Atoi(major)
	return n
}

// escapeNodeGlob makes a path a pattern of Node's glob (minimatch, without negation and
// with backslashes as path separators) that matches only itself: special characters go
// into character classes. Two cannot be escaped that way: a backslash (a separator to
// Node's glob) becomes "?" outside Windows, and braces holding a comma or ".." are
// expanded before classes are read, so in a path with braces commas become "?" and the
// second dot of ".." is put in a class. "?" matches any character, so such a path may
// also run other test files named like it but for that character.
func escapeNodeGlob(p string) string {
	braces := strings.ContainsAny(p, "{}")
	segs := strings.Split(p, "/")
	for i, seg := range segs {
		if seg == "." || seg == ".." {
			continue
		}
		var b strings.Builder
		prev := rune(0)
		for _, c := range seg {
			switch {
			case strings.ContainsRune(`[]*?(){}+@`, c):
				b.WriteString("[" + string(c) + "]")
			case c == '\\' && runtime.GOOS != "windows":
				b.WriteByte('?')
			case braces && c == ',':
				b.WriteByte('?')
			case braces && c == '.' && prev == '.':
				b.WriteString("[.]")
			default:
				b.WriteRune(c)
			}
			prev = c
		}
		segs[i] = b.String()
	}
	return strings.Join(segs, "/")
}

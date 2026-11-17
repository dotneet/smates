package runner

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/dotneet/smates/internal/config"
	"github.com/dotneet/smates/internal/depkey"
	"github.com/dotneet/smates/internal/pathutil"
	"github.com/dotneet/smates/internal/state"
	"github.com/dotneet/smates/internal/trace"
)

// maxArgBytes bounds the size of a command line outside Windows; larger selections are
// split into several invocations of the runner.
const maxArgBytes = 96 * 1024

// Windows limits a whole command line: CreateProcess takes at most 32767 UTF-16 code
// units, and cmd.exe, which runs .cmd and .bat launchers (node_modules/.bin on
// Windows), 8191 characters.
const (
	maxWindowsCmdLine = 32767
	maxWindowsBatch   = 8191
)

// Shims returns the extracted shim locations.
func (rc *RunContext) Shims() *trace.Shims {
	return &trace.Shims{Dir: rc.ShimDir, Node: trace.NodeShim(rc.ShimDir), Python: filepath.Join(rc.ShimDir, "py")}
}

// argLimit is the most bytes a command line starting with argv0 may take on goos.
func argLimit(goos, argv0 string) int {
	if goos != "windows" {
		return maxArgBytes
	}
	name := argv0
	if goos == runtime.GOOS {
		if p, err := exec.LookPath(argv0); err == nil {
			name = p
		}
	}
	switch strings.ToLower(filepath.Ext(name)) {
	case ".cmd", ".bat":
		return maxWindowsBatch
	}
	return maxWindowsCmdLine
}

// argSize is what argument a takes on a command line on goos: the argument and a
// separator, and on Windows the quotes and escapes it may need (a UTF-8 byte count is
// never below the UTF-16 one).
func argSize(goos, a string) int {
	n := len(a) + 1
	if goos == "windows" {
		n += 2 + strings.Count(a, `"`) + strings.Count(a, `\`)
	}
	return n
}

// chunk splits args into groups each of which fits on one command line together with
// the fixed words base (the command and the flags every invocation gets).
func chunk(base, args []string) [][]string {
	return chunkFor(runtime.GOOS, base, args)
}

func chunkFor(goos string, base, args []string) [][]string {
	argv0 := ""
	if len(base) > 0 {
		argv0 = base[0]
	}
	room := argLimit(goos, argv0)
	for _, w := range base {
		room -= argSize(goos, w)
	}
	var out [][]string
	var cur []string
	size := 0
	for _, a := range args {
		n := argSize(goos, a)
		if size+n > room && len(cur) > 0 {
			out = append(out, cur)
			cur, size = nil, 0
		}
		cur = append(cur, a)
		size += n
	}
	if len(cur) > 0 || len(out) == 0 {
		out = append(out, cur)
	}
	return out
}

// runBatches runs the runner once (all tests) or once per chunk of test arguments,
// returning the worst exit code; okCodes count as 0.
func runBatches(ctx context.Context, rc *RunContext, extra []string, testArgs []string, env []string, okCodes ...int) (int, error) {
	if rc.All {
		return runChunks(ctx, rc, extra, [][]string{nil}, env, okCodes...)
	}
	base, err := rc.CommandLine(extra, nil)
	if err != nil {
		return 1, err
	}
	return runChunks(ctx, rc, extra, rc.chunk(base, testArgs), env, okCodes...)
}

// chunk is chunk for a command line run with the suite's environment: on Windows its
// limit depends on the program the suite's PATH finds (a .cmd launcher).
func (rc *RunContext) chunk(base, args []string) [][]string {
	if runtime.GOOS == "windows" && len(base) > 0 {
		if path, ok := envPath(MergeEnv(baseEnv(), suiteEnv(rc.Suite))); ok {
			if p, err := lookPathIn(base[0], path, rc.Dir()); err == nil {
				base = append([]string{p}, base[1:]...)
			}
		}
	}
	return chunk(base, args)
}

// runChunks runs the suite command once per batch of test arguments, returning the
// worst exit code; okCodes count as 0. The suite's JUnit report is read after each
// invocation (reportInvocation).
func runChunks(ctx context.Context, rc *RunContext, extra []string, batches [][]string, env []string, okCodes ...int) (int, error) {
	rc.beginReport()
	worst := 0
	for i, b := range batches {
		argv, err := rc.CommandLine(extra, b)
		if err != nil {
			return 1, err
		}
		if rc.rewrite != nil {
			argv = rc.rewrite(argv)
		}
		// Each invocation writes V8 coverage to a directory of its own: a coverage tool
		// cleaning it when it starts (c8) must not delete an earlier invocation's files.
		env, err := trace.SeparateV8(env, rc.TraceDir, i)
		if err != nil {
			return 1, err
		}
		code, err := rc.reportInvocation(func() (int, error) { return rc.Exec(ctx, argv, env) })
		if err != nil {
			return code, err
		}
		if slices.Contains(okCodes, code) {
			code = 0
		}
		worst = max(worst, code)
	}
	return worst, nil
}

// junitReport holds what the suite's JUnit report (junit: in smates.yml) said after
// the invocations of the current run.
type junitReport struct {
	results  map[string]trace.Result
	warnings []string
}

// beginReport starts collecting the JUnit report of a run: what an earlier run with
// rc (one a flaky retry repeats) collected no longer counts.
func (rc *RunContext) beginReport() {
	rc.report = nil
	if rc.Suite.Junit != "" {
		rc.report = &junitReport{results: map[string]trace.Result{}}
	}
}

// reportPath is the absolute path of the suite's JUnit report ("" when none is set).
func (rc *RunContext) reportPath() string {
	if rc.Suite.Junit == "" {
		return ""
	}
	return filepath.Join(rc.Dir(), filepath.FromSlash(rc.Suite.Junit))
}

// reportInvocation runs one invocation of the runner, which may write the suite's
// JUnit report: the report is removed first, so that one an earlier run left, or an
// earlier chunk of this run (each invocation overwrites it), is never read as this
// invocation's; it is read after it. An invocation that writes none (it crashed) gives
// its units no reported result: their trace or exit code decides.
func (rc *RunContext) reportInvocation(run func() (int, error)) (int, error) {
	p := rc.reportPath()
	if p == "" {
		return run()
	}
	if rc.report == nil {
		rc.beginReport()
	}
	if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
		// What it holds afterwards may be an earlier run's: not used.
		rc.report.warnings = append(rc.report.warnings, "could not remove the JUnit report before the run, so its results are not used: "+err.Error())
		return run()
	}
	code, err := run()
	if err == nil {
		rc.collectReport()
	}
	return code, err
}

// collectReport adds the results of the JUnit report an invocation just wrote to the
// run's.
func (rc *RunContext) collectReport() {
	p := rc.reportPath()
	if p == "" {
		return
	}
	if rc.report == nil {
		rc.beginReport()
	}
	jr, err := trace.JUnit(p, rc.Dir(), rc.Repo.Root)
	if err != nil {
		if w := "could not read JUnit report: " + err.Error(); !slices.Contains(rc.report.warnings, w) {
			rc.report.warnings = append(rc.report.warnings, w)
		}
		return
	}
	for f, r := range jr {
		if prev, ok := rc.report.results[f]; ok {
			// A file several invocations report keeps the worst result.
			r = trace.Result{Status: trace.Worse(prev.Status, r.Status), Ms: prev.Ms + r.Ms, Message: trace.JoinMessage(prev.Message, r.Message)}
		}
		rc.report.results[f] = r
	}
}

// traceOptions tune how trace data turns into observations.
type traceOptions struct {
	// trustProcStatus uses per-process exit codes as per-unit status (one process per
	// test file, e.g. node --test). Otherwise only runner plugins or the overall exit
	// code decide.
	trustProcStatus bool
	// dropDirs ignores directory listings (runners that crawl the tree in the same
	// process that runs tests).
	dropDirs bool
	// unitsFromArgv identifies units by the test file on each process's command line.
	unitsFromArgv bool
	// workersOwnArgv: with unitsFromArgv, the pool workers of a runner naming a test
	// file own its unit, and the runner is infrastructure (trace.Options.WorkersOwnArgv).
	workersOwnArgv bool
}

// untracedOptions are the trace options of a run of one test file per runner process
// (runIsolatedNode): without coverage (RunContext.Untraced), no executed code tells
// which unit a process ran, so units come from the test file on the runner's command
// line, and what the pool workers below that runner use is that unit's.
func untracedOptions(rc *RunContext, opt traceOptions) traceOptions {
	if rc.Untraced {
		opt.unitsFromArgv, opt.workersOwnArgv = true, true
	}
	return opt
}

// observe collects a trace directory into per-unit observations.
func observe(rc *RunContext, exitCode int, opt traceOptions) (map[string]*Observation, []string, error) {
	obs, warns, _, err := observeAll(rc, exitCode, opt)
	return obs, warns, err
}

// traceEnv is the environment of a recorded Node or Bun run: trace.Env, less the V8
// coverage of an untraced run (RunContext.Untraced), whose processes then write their
// coverage where the user's NODE_V8_COVERAGE says, if anywhere.
func traceEnv(rc *RunContext) []string {
	env := trace.Env(rc.TraceDir, rc.Repo.Root, rc.Shims(), suiteEnv(rc.Suite))
	if rc.Untraced {
		env = slices.DeleteFunc(env, func(kv string) bool { return strings.HasPrefix(kv, "NODE_V8_COVERAGE=") })
	}
	return env
}

// observeAll is observe that also returns what source files read when they ran.
func observeAll(rc *RunContext, exitCode int, opt traceOptions) (map[string]*Observation, []string, map[string]map[string]bool, error) {
	procs, err := trace.Collect(rc.TraceDir, rc.Repo.Root)
	if err != nil {
		return nil, nil, nil, err
	}
	readsBy := map[string]map[string]bool{}
	for _, p := range procs {
		for by, keys := range p.ReadsBy {
			if readsBy[by] == nil {
				readsBy[by] = map[string]bool{}
			}
			for k := range keys {
				readsBy[by][k] = true
			}
		}
	}
	if !opt.trustProcStatus {
		for _, p := range procs {
			p.Exit = nil
		}
	}
	attr := trace.Attribute(procs, rc.UnitOf, trace.Options{UnitsFromArgv: opt.unitsFromArgv, WorkersOwnArgv: opt.workersOwnArgv, Rel: trace.RelFunc(rc.Repo.Root)})
	var warnings []string
	if attr.SharedProcs > 0 {
		warnings = append(warnings, fmt.Sprintf("%d test process(es) ran several test files; their dependencies were merged (still correct, less precise). Set \"isolate: true\" on the suite in smates.yml (one process per test file while recording) for exact attribution.", attr.SharedProcs))
	}
	results, warns := reportedResults(rc)
	warnings = append(warnings, warns...)
	obs := map[string]*Observation{}
	for id, u := range attr.Units {
		o := &Observation{Files: map[string]bool{}, DurationMs: u.Ms}
		for f := range u.Files {
			if !(opt.dropDirs && depkey.KindOf(f) == depkey.KindListing) {
				o.Files[f] = true
			}
		}
		for f := range attr.Global {
			o.Files[f] = true
		}
		o.ev = unitEvidence{traced: u.Status, exit: exitCode, hasTrace: true}
		o.Failure = u.Message
		if r, ok := results[id]; ok {
			o.ev.reported = r.Status
			o.Failure = trace.JoinMessage(o.Failure, r.Message)
			if r.Ms > 0 {
				o.DurationMs = r.Ms
			}
		}
		o.settle()
		obs[id] = o
	}
	// A test file that failed before executing (an import error during collection)
	// leaves no trace, but the runner still reported it. One that passed without a trace
	// ran in a process whose trace was lost (a pool worker killed before it wrote its
	// coverage), or in a browser, which is never traced: what it used is unknown, so it
	// must run again rather than be skipped. With trust_browser_passes, a pass of a test
	// that runs only in a browser is recorded with the dependencies known statically
	// and those of the runner's own process.
	untraced, browser := 0, 0
	trust := config.Bool(rc.Suite.TrustBrowserPasses, false)
	for f, r := range results {
		id := rc.UnitOf(f)
		if id == "" || obs[id] != nil {
			continue
		}
		inBrowser := rc.Suite.BrowserUnit([]string{f})
		if trust && inBrowser && r.Status == state.Pass {
			o := &Observation{Files: map[string]bool{}, Status: state.Pass, DurationMs: r.Ms, StaticOnly: true}
			for g := range attr.Global {
				o.Files[g] = true
			}
			obs[id] = o
			continue
		}
		o := &Observation{Files: map[string]bool{}, DurationMs: r.Ms, Failure: r.Message}
		o.ev = unitEvidence{reported: r.Status, exit: exitCode}
		if o.settle(); o.Status != state.Fail {
			if inBrowser {
				browser++
			} else {
				untraced++
			}
		}
		obs[id] = o
	}
	if w := untracedWarning(untraced, len(attr.Units) > 0); w != "" {
		warnings = append(warnings, w)
	}
	if browser > 0 {
		warnings = append(warnings, fmt.Sprintf("%d test file(s) ran in a browser, which smates cannot trace; they will run again next time (\"trust_browser_passes: true\" on the suite records their passes with their static dependencies)", browser))
	}
	return obs, warnings, readsBy, nil
}

// untracedWarning explains the test files a runner reported as passed although they
// left no trace (untraced): a process killed before it wrote its trace when other
// test processes left one (traced), otherwise more likely a command that does not
// pass smates' tracing environment on to the test processes or replays cached
// results. Either way they will run again.
func untracedWarning(untraced int, traced bool) string {
	switch {
	case untraced == 0:
		return ""
	case traced:
		return fmt.Sprintf("%d test file(s) passed but left no trace (their process was likely killed before writing it); they will run again next time", untraced)
	}
	return fmt.Sprintf("%d test file(s) passed but none left a trace: %s; they will run again next time", untraced, NoTraceCauses)
}

// NoTraceCauses are the likely reasons a run that reported tests left no trace at all.
const NoTraceCauses = "the suite command probably runs them through a wrapper that drops the tracing environment " +
	"(NODE_OPTIONS, NODE_V8_COVERAGE, SMATES_*: a task runner such as turbo or nx, env -i, a container) or replays cached results; " +
	"run the test runner directly (with root: set to its package) or pass that environment through"

// reportedResults reads the per-file results runner reporters wrote into the trace
// directory and those of the suite's JUnit report, which win unless theirs are worse
// (fail beats unknown beats pass). Only reports the invocations of this run wrote count
// (reportInvocation): one an earlier run left is never read.
func reportedResults(rc *RunContext) (map[string]trace.Result, []string) {
	results := trace.Results(rc.TraceDir, rc.Repo.Root)
	if rc.Suite.Junit == "" || rc.report == nil {
		return results, nil
	}
	warnings := slices.Clone(rc.report.warnings)
	for f, r := range rc.report.results {
		// A JUnit report lists only the tests that ran: a file a reporter saw the run
		// stop in (unknown) or fail does not pass because the tests that ran passed.
		if prev, ok := results[f]; ok {
			if st := trace.Worse(r.Status, prev.Status); st != r.Status {
				r.Status, r.Message = st, trace.JoinMessage(prev.Message, r.Message)
			}
		}
		results[f] = r
	}
	return results, warnings
}

// resultsEnv is the environment of a run that is not traced but whose runner
// reporters write per-file results (rc.TraceDir set without rc.Record).
func resultsEnv(rc *RunContext) []string {
	if rc.TraceDir == "" {
		return nil
	}
	return []string{"SMATES_TRACE_DIR=" + rc.TraceDir}
}

// reported returns the outcomes of the units runner reporters (or the suite's JUnit
// report) gave in a run that was not traced: statuses and failure output, no
// dependencies. Units without a report have no observation.
func reported(rc *RunContext) (map[string]*Observation, []string) {
	if rc.TraceDir == "" {
		return nil, nil
	}
	results, warnings := reportedResults(rc)
	obs := map[string]*Observation{}
	for f, r := range results {
		id := rc.UnitOf(f)
		if id == "" {
			continue
		}
		o := obs[id]
		if o == nil {
			o = &Observation{Files: map[string]bool{}}
			obs[id] = o
		}
		if o.Status != state.Fail {
			o.Status = r.Status
		}
		o.DurationMs += r.Ms
		o.Failure = trace.JoinMessage(o.Failure, r.Message)
	}
	return obs, warnings
}

// overallStatus combines a unit status with the exit code of the whole run: a failed
// run without per-unit information marks every unit failed (it will run again).
func overallStatus(unit string, exitCode int) string {
	switch unit {
	case state.Fail:
		return state.Fail
	case state.Pass:
		return state.Pass
	}
	if exitCode == 0 {
		return state.Pass
	}
	return state.Fail
}

// isolatedRun runs one command per unit in parallel (for recording runners that share
// processes between files), buffering each command's output so logs stay readable.
func isolatedRun(ctx context.Context, rc *RunContext, units []string, jobs int, argvFor func(unit string, i int) ([]string, []string, error)) (map[string]int, error) {
	if rc.Suite.RecordJobs > 0 {
		jobs = rc.Suite.RecordJobs
	}
	// Build every command line first: a failure then starts nothing.
	type job struct{ argv, env []string }
	cmds := make([]job, len(units))
	for i, u := range units {
		argv, env, err := argvFor(u, i)
		if err == nil {
			env, err = trace.SeparateV8(env, rc.TraceDir, i)
		}
		if err != nil {
			return nil, err
		}
		cmds[i] = job{argv, env}
	}
	// The processes run at once: the suite's JUnit report holds what the last one wrote
	// (the exit code of each decides its unit's failure).
	rc.beginReport()
	var codes map[string]int
	_, err := rc.reportInvocation(func() (int, error) {
		var err error
		codes, err = runJobs(ctx, rc, units, jobs, func(i int) (argv, env []string) { return cmds[i].argv, cmds[i].env })
		return 0, err
	})
	return codes, err
}

// runJobs runs the command of each unit, at most jobs at once, printing each one's
// output after it ends.
func runJobs(ctx context.Context, rc *RunContext, units []string, jobs int, cmdOf func(i int) (argv, env []string)) (map[string]int, error) {
	codes := map[string]int{}
	var mu sync.Mutex
	var outMu sync.Mutex
	sem := make(chan struct{}, max(1, jobs))
	var wg sync.WaitGroup
	var firstErr error
	for i, u := range units {
		argv, env := cmdOf(i)
		wg.Add(1)
		sem <- struct{}{}
		go func(u string, argv, env []string) {
			defer wg.Done()
			defer func() { <-sem }()
			var buf bytes.Buffer
			start := time.Now()
			code, err := rc.ExecIn(ctx, rc.Dir(), argv, env, &buf, &buf)
			outMu.Lock()
			fmt.Fprintf(rc.Stdout, "── smates: %s (exit %d, %s)\n", u, code, time.Since(start).Round(time.Millisecond))
			rc.Stdout.Write(buf.Bytes())
			outMu.Unlock()
			mu.Lock()
			codes[u] = code
			if err != nil && firstErr == nil {
				firstErr = err
			}
			mu.Unlock()
		}(u, argv, env)
	}
	wg.Wait()
	return codes, firstErr
}

// exportV8 copies the V8 coverage of a recorded run into the directory the user set
// NODE_V8_COVERAGE to (smates points it at its own trace directory while recording),
// returning a warning when that fails.
func exportV8(rc *RunContext) []string {
	dst := trace.UserV8Dir(suiteEnv(rc.Suite), rc.Dir())
	if dst == "" {
		return nil
	}
	if err := trace.CopyV8(rc.TraceDir, dst); err != nil {
		return []string{"could not copy V8 coverage to NODE_V8_COVERAGE (" + dst + "): " + err.Error()}
	}
	return nil
}

// worstCode returns the highest exit code of an isolated run.
func worstCode(codes map[string]int) int {
	worst := 0
	for _, c := range codes {
		worst = max(worst, c)
	}
	return worst
}

// unitEvidence is what a run tells about the outcome of one unit (Observation.settle).
type unitEvidence struct {
	// reported is the status a runner reporter or the JUnit report of this run gave the
	// unit ("" for none).
	reported string
	// traced is the status its trace gave: the exit code of a process that ran only it
	// (traceOptions.trustProcStatus), a failure message ("" for none).
	traced string
	// exit is the exit code of the process that ran the unit: its own (own) or that of
	// a run of several units.
	exit int
	own  bool
	// hasTrace is set when the unit's process left a trace: what it used is known.
	hasTrace bool
}

// settle sets the unit's status from its evidence. Only evidence that the unit itself
// passed, from a process that left its trace, makes a pass; without a reliable result
// a unit is not a pass. In order:
//
//   - a reported, traced or own-exit failure fails it;
//   - a reported (else traced) status decides otherwise: a skip or unknown stays so;
//   - with neither, the exit code decides: 0 passes, another fails, inferred when it
//     is the exit code of several units;
//   - a pass without a trace (its process was killed before writing it, or the
//     command dropped smates' environment) is unknown: it runs again.
func (o *Observation) settle() {
	e := o.ev
	o.Status, o.Inferred = settleStatus(e)
}

func settleStatus(e unitEvidence) (status string, inferred bool) {
	switch {
	case e.reported == state.Fail, e.traced == state.Fail, e.own && e.exit != 0:
		return state.Fail, false
	}
	st := e.reported
	if st == "" {
		st = e.traced
	}
	if st == "" {
		if e.exit != 0 {
			return state.Fail, true // !e.own
		}
		st = state.Pass
	}
	if st == state.Pass && !e.hasTrace {
		return state.Unknown, false
	}
	return st, false
}

// setUnitExit settles a unit that ran in a process of its own (isolatedRun) with that
// process's exit code: the outcome is the unit's own, not inferred from the run. A unit
// that left no trace does not pass: it runs again.
func setUnitExit(o *Observation, code int) {
	o.ev.exit, o.ev.own = code, true
	o.settle()
}

// findBin looks for node_modules/.bin/<name> from dir up to the repository root and
// returns it relative to dir.
func findBin(repoRoot, dir, name string) string {
	d := dir
	for {
		p := filepath.Join(d, "node_modules", ".bin", name)
		if _, err := os.Stat(p); err == nil {
			if r, err := filepath.Rel(dir, p); err == nil {
				return filepath.ToSlash(r)
			}
			return p
		}
		if d == repoRoot || len(d) <= len(repoRoot) {
			return ""
		}
		d = filepath.Dir(d)
	}
}

// packageManager guesses the JS package manager from lockfiles near dir.
func packageManager(repoRoot, dir string) string {
	d := dir
	for {
		for _, c := range []struct{ file, pm string }{
			{"pnpm-lock.yaml", "pnpm"}, {"bun.lock", "bun"}, {"bun.lockb", "bun"}, {"yarn.lock", "yarn"}, {"package-lock.json", "npm"},
		} {
			if _, err := os.Stat(filepath.Join(d, c.file)); err == nil {
				return c.pm
			}
		}
		if d == repoRoot || len(d) <= len(repoRoot) {
			return "npm"
		}
		d = filepath.Dir(d)
	}
}

// jsBinCommand returns a command that runs a JS tool installed in node_modules.
func jsBinCommand(repoRoot, dir, bin string, args string) string {
	if p := findBin(repoRoot, dir, bin); p != "" {
		if !strings.HasPrefix(p, ".") && !filepath.IsAbs(p) {
			p = "./" + p
		}
		return strings.TrimSpace(p + " " + args)
	}
	switch packageManager(repoRoot, dir) {
	case "pnpm":
		return strings.TrimSpace("pnpm exec " + bin + " " + args)
	case "yarn":
		return strings.TrimSpace("yarn " + bin + " " + args)
	case "bun":
		return strings.TrimSpace("bunx " + bin + " " + args)
	}
	return strings.TrimSpace("npx --no-install " + bin + " " + args)
}

// JSBinCommand is jsBinCommand for a repo-relative suite root, for callers that write
// a suite's command (detection).
func JSBinCommand(repoRoot, root, bin, args string) string {
	return jsBinCommand(repoRoot, filepath.Join(repoRoot, filepath.FromSlash(root)), bin, args)
}

// jsLockInputs are the dependency manifests, lockfiles and runtime pins every JS test
// of a suite rooted at root depends on: in the suite directory and in every directory
// above it up to the repository root (workspace roots).
func jsLockInputs(root string) []string {
	names := []string{
		"package.json", "pnpm-lock.yaml", "pnpm-workspace.yaml", "package-lock.json", "npm-shrinkwrap.json", "yarn.lock",
		"bun.lock", "bun.lockb", ".npmrc", ".yarnrc.yml", ".nvmrc", ".node-version", ".tool-versions", "bunfig.toml",
	}
	out := []string{"tsconfig*.json", "/tsconfig*.json", ".env", ".env.*"}
	for d := root; ; d = path.Dir(d) {
		for _, n := range names {
			if d == "." {
				out = append(out, "/"+n)
			} else {
				out = append(out, "/"+d+"/"+n)
			}
		}
		if d == "." || d == "/" || d == "" {
			break
		}
	}
	return out
}

// unitsAreFiles is the Units implementation for runners whose unit is a test file.
func unitsAreFiles(testFiles []string) []string {
	out := append([]string{}, testFiles...)
	sort.Strings(out)
	return out
}

// relTo returns repo-relative path p relative to the suite directory.
func relToSuite(s *Suite, p string) string { return pathutil.RelTo(p, s.Root) }

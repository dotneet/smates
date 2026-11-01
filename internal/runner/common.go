package runner

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/dotneet/smates/internal/depkey"
	"github.com/dotneet/smates/internal/pathutil"
	"github.com/dotneet/smates/internal/state"
	"github.com/dotneet/smates/internal/trace"
)

// maxArgBytes bounds the size of test arguments per invocation; larger selections are
// split into several invocations of the runner.
const maxArgBytes = 96 * 1024

// Shims returns the extracted shim locations.
func (rc *RunContext) Shims() *trace.Shims {
	return &trace.Shims{Dir: rc.ShimDir, Node: trace.NodeShim(rc.ShimDir), Python: filepath.Join(rc.ShimDir, "py")}
}

// chunk splits args into groups whose total length stays under maxArgBytes.
func chunk(args []string) [][]string {
	var out [][]string
	var cur []string
	size := 0
	for _, a := range args {
		if size+len(a)+1 > maxArgBytes && len(cur) > 0 {
			out = append(out, cur)
			cur, size = nil, 0
		}
		cur = append(cur, a)
		size += len(a) + 1
	}
	if len(cur) > 0 || len(out) == 0 {
		out = append(out, cur)
	}
	return out
}

// runBatches runs the runner once (all tests) or once per chunk of test arguments,
// returning the worst exit code.
func runBatches(ctx context.Context, rc *RunContext, extra []string, testArgs []string, env []string, okCodes ...int) (int, error) {
	batches := [][]string{nil}
	if !rc.All {
		batches = chunk(testArgs)
	}
	worst := 0
	for i, b := range batches {
		argv, err := rc.CommandLine(extra, b)
		if err != nil {
			return 1, err
		}
		// Each invocation writes V8 coverage to a directory of its own: a coverage tool
		// cleaning it when it starts (c8) must not delete an earlier invocation's files.
		env, err := trace.SeparateV8(env, rc.TraceDir, i)
		if err != nil {
			return 1, err
		}
		code, err := rc.Exec(ctx, argv, env)
		if err != nil {
			return code, err
		}
		for _, ok := range okCodes {
			if code == ok {
				code = 0
			}
		}
		if code > worst {
			worst = code
		}
	}
	return worst, nil
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
}

// observe collects a trace directory into per-unit observations.
func observe(rc *RunContext, exitCode int, opt traceOptions) (map[string]*Observation, []string, error) {
	obs, warns, _, err := observeAll(rc, exitCode, opt)
	return obs, warns, err
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
	attr := trace.Attribute(procs, rc.UnitOf, trace.Options{UnitsFromArgv: opt.unitsFromArgv, Rel: trace.RelFunc(rc.Repo.Root)})
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
		o.Status = overallStatus(u.Status, exitCode)
		o.Inferred = u.Status == "" && exitCode != 0
		o.Failure = u.Message
		if r, ok := results[id]; ok {
			o.Status, o.Inferred = r.Status, false
			o.Failure = trace.JoinMessage(o.Failure, r.Message)
			if r.Ms > 0 {
				o.DurationMs = r.Ms
			}
		}
		obs[id] = o
	}
	// A test file that failed before executing (an import error during collection)
	// leaves no trace, but the runner still reported it. One that passed without a trace
	// ran in a process whose trace was lost (a pool worker killed before it wrote its
	// coverage): what it used is unknown, so it must run again rather than be skipped.
	untraced := 0
	for f, r := range results {
		id := rc.UnitOf(f)
		if id == "" || obs[id] != nil {
			continue
		}
		o := &Observation{Files: map[string]bool{}, Status: r.Status, DurationMs: r.Ms, Failure: r.Message}
		if o.Status != state.Fail {
			o.Status = state.Unknown
			untraced++
		}
		obs[id] = o
	}
	if untraced > 0 {
		warnings = append(warnings, fmt.Sprintf("%d test file(s) passed but left no trace (their process was likely killed before writing it); they will run again next time", untraced))
	}
	return obs, warnings, readsBy, nil
}

// reportedResults reads the per-file results runner reporters wrote into the trace
// directory and the suite's JUnit report, which wins unless theirs is worse (fail beats
// unknown beats pass).
func reportedResults(rc *RunContext) (map[string]trace.Result, []string) {
	results := trace.Results(rc.TraceDir, rc.Repo.Root)
	if rc.Suite.Junit == "" {
		return results, nil
	}
	var warnings []string
	jr, err := trace.JUnit(filepath.Join(rc.Dir(), filepath.FromSlash(rc.Suite.Junit)), rc.Dir(), rc.Repo.Root)
	if err != nil {
		warnings = append(warnings, "could not read JUnit report: "+err.Error())
	}
	for f, r := range jr {
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
	codes := map[string]int{}
	var mu sync.Mutex
	var outMu sync.Mutex
	sem := make(chan struct{}, max(1, jobs))
	var wg sync.WaitGroup
	var firstErr error
	for i, u := range units {
		argv, env := cmds[i].argv, cmds[i].env
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

// setUnitExit sets the status of a unit that ran in its own process (isolatedRun) from
// that process's exit code: the outcome is the unit's own, not inferred from the run.
func setUnitExit(o *Observation, code int) {
	o.Status, o.Inferred = overallStatus("", code), false
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

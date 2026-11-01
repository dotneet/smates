package trace

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"
)

// GoExecEnv marks a smates process started by `go test -exec` (see GoExec).
const GoExecEnv = "SMATES_GOEXEC"

// GoExecUserEnv carries a user's own -exec program (from GOFLAGS), which GoExec runs
// the test binary through.
const GoExecUserEnv = "SMATES_GOEXEC_USER"

// GoParentPrefix starts the SMATES_TRACE_PARENT value given to Go test binaries; the
// rest is the package directory.
const GoParentPrefix = "go:"

// GoChildren attributes traced processes started (directly or not) by Go test binaries
// to their package directory (absolute), returning the files each package's children used.
func GoChildren(procs []*Proc) map[string]map[string]bool {
	byKey := map[string]*Proc{}
	for _, p := range procs {
		byKey[p.Key()] = p
	}
	out := map[string]map[string]bool{}
	for _, p := range procs {
		anc := p.Anc
		for n := 0; n < 64 && anc != "" && !strings.HasPrefix(anc, GoParentPrefix); n++ {
			a := byKey[anc]
			if a == nil {
				break
			}
			anc = a.Anc
		}
		dir, ok := strings.CutPrefix(anc, GoParentPrefix)
		if !ok {
			continue
		}
		if out[dir] == nil {
			out[dir] = map[string]bool{}
		}
		for f := range p.allFiles() {
			out[dir][f] = true
		}
	}
	return out
}

// GoChildLogEnv names the directory in which re-executions of a Go test binary keep test
// logs of their own (see GoTestLogs): the test file smates adds to the binary while
// recording (runner's childSource) leaves a marker there holding the process's working
// directory: <pid>.test for a process run as a test binary, whose -test.testlogfile it
// points at <pid>.testlog, and <pid>.cmd for one run with arguments of its own. The
// process smates started (its command line sets -test.testlogfile) leaves none.
const GoChildLogEnv = "SMATES_GOTESTLOGDIR"

// Suffixes of the markers in a GoChildLogEnv directory.
const (
	GoChildTestSuffix = "test"
	GoChildCmdSuffix  = "cmd"
)

// GoChildCoverEnv names the coverage directory of the Go test function being recorded
// (see runner's childSource); package recording leaves it unset.
const GoChildCoverEnv = "SMATES_GOCOVERDIR"

// GoFlagsOrigEnv carries the user's GOFLAGS, restored for the test binary so that go
// commands the test runs itself (go run, go build) do not inherit -exec.
const GoFlagsOrigEnv = "SMATES_GOFLAGS_ORIG"

// GoRun is one traced Go test binary execution.
type GoRun struct {
	Dir   string   `json:"dir"`           // absolute package directory (the binary's cwd)
	Files []string `json:"files"`         // absolute paths opened or stat'ed by the test
	Env   []string `json:"env,omitempty"` // environment variables the test looked up
	Exit  int      `json:"exit"`
	Ms    int64    `json:"ms"`
}

// GoExec runs a Go test binary (args[0]) with a test log, the way `go test` itself does
// for its cache, and records which files the test opened. It is invoked through
// GOFLAGS=-exec=<smates> with SMATES_GOEXEC set, and returns the binary's exit code.
func GoExec(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "smates goexec: missing test binary")
		return 2
	}
	traceDir := os.Getenv("SMATES_TRACE_DIR")
	bin, rest := args[0], args[1:]
	logFile := ""
	for _, a := range rest {
		if v, ok := strings.CutPrefix(a, "-test.testlogfile="); ok {
			logFile = v // go test caching asked for it; read it after the run
		}
	}
	if logFile == "" && traceDir != "" {
		logFile = filepath.Join(traceDir, fmt.Sprintf("go-%d.testlog", os.Getpid()))
		rest = append([]string{"-test.testlogfile=" + logFile}, rest...)
	}
	argv := append([]string{bin}, rest...)
	if user := strings.Fields(os.Getenv(GoExecUserEnv)); len(user) > 0 {
		argv = append(user, argv...)
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	var env []string
	for _, kv := range os.Environ() {
		switch {
		case strings.HasPrefix(kv, GoExecEnv+"="), strings.HasPrefix(kv, GoFlagsOrigEnv+"="), strings.HasPrefix(kv, GoExecUserEnv+"="), strings.HasPrefix(kv, "GOFLAGS="),
			strings.HasPrefix(kv, GoChildLogEnv+"="), strings.HasPrefix(kv, GoChildCoverEnv+"="):
			continue
		}
		env = append(env, kv)
	}
	// Re-executions of the binary write their test logs there.
	childDir := ""
	if traceDir != "" {
		childDir = filepath.Join(traceDir, fmt.Sprintf("go-%d.children", os.Getpid()))
		if err := os.MkdirAll(childDir, 0o755); err == nil {
			env = append(env, GoChildLogEnv+"="+childDir)
		}
	}
	if orig := os.Getenv(GoFlagsOrigEnv); orig != "" {
		env = append(env, "GOFLAGS="+orig)
	}
	// Node and Python processes the test starts trace themselves; this marks them as
	// belonging to this package's test binary.
	if cwd, err := os.Getwd(); err == nil {
		env = append(env, "SMATES_TRACE_PARENT="+GoParentPrefix+cwd)
	}
	cmd.Env = env
	start := time.Now()
	if err := cmd.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "smates goexec: %v\n", err)
		return 1
	}
	sigs := make(chan os.Signal, 4)
	signal.Notify(sigs, forwardedSignals...)
	go func() {
		for s := range sigs {
			_ = cmd.Process.Signal(s)
		}
	}()
	err := cmd.Wait()
	signal.Stop(sigs)
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
		if code < 0 {
			code = 1
		}
	} else if err != nil {
		code = 1
	}
	if traceDir != "" {
		cwd, _ := os.Getwd()
		run := GoRun{Dir: cwd, Exit: code, Ms: time.Since(start).Milliseconds()}
		run.Files, run.Env = GoTestLogs(logFile, cwd, bin, childDir)
		if data, err := json.Marshal(run); err == nil {
			_ = os.WriteFile(filepath.Join(traceDir, fmt.Sprintf("go-%d.json", os.Getpid())), data, 0o644)
		}
	}
	return code
}

// ParseTestLog reads a Go test log ("# test log" then "open|stat|chdir|getenv <arg>"):
// the files the test used and the environment variables it looked up (os.Getenv,
// os.LookupEnv; the same records go test uses to invalidate its cache).
func ParseTestLog(file, cwd string) (files, env []string) {
	files, env, _, _ = parseTestLog(file, cwd)
	return files, env
}

// goStart is a program a Go test started, as its test log shows it.
type goStart struct {
	prog string   // absolute path of the program
	dirs []string // directories it may run in (absolute)
}

// parseTestLog is ParseTestLog, with the programs the test started (see started) and
// whether the log is complete. testing buffers the log and writes it when the tests end
// (bufio's default 4096 bytes at a time when it fills up): a process that exits earlier
// (os.Exit in a test, a panic, a kill) leaves it empty or cut at a multiple of 4096 bytes.
// A complete log of that size counts as cut too (more dependencies, never fewer).
func parseTestLog(file, cwd string) (files, env []string, starts []goStart, complete bool) {
	data, err := os.ReadFile(file)
	if err != nil {
		return nil, nil, nil, false
	}
	complete = len(data)%4096 != 0 && strings.HasPrefix(string(data), "# test log\n")
	var out []string
	seen := map[string]bool{}
	dir := cwd
	lines := strings.Split(string(data), "\n")
	for i, line := range lines {
		op, arg, ok := strings.Cut(line, " ")
		if !ok {
			continue
		}
		switch op {
		case "getenv":
			if !seen[line] {
				seen[line] = true
				env = append(env, arg)
			}
		case "chdir":
			dir = absIn(arg, dir)
		case "open", "stat":
			raw := arg
			arg = absIn(arg, dir)
			if op == "open" {
				if s, ok := started(raw, arg, dir, lines[i+1:]); ok {
					starts = append(starts, s)
				}
			}
			key := op + " " + arg
			if seen[key] {
				continue
			}
			seen[key] = true
			fi, err := os.Stat(arg)
			switch {
			case err != nil || op == "stat":
				// Existence checks (including of files that do not exist yet).
				out = append(out, arg+"?")
			case fi.IsDir():
				out = append(out, arg+string(filepath.Separator)) // opening a directory lists it
			default:
				out = append(out, arg)
			}
		}
	}
	return out, env, starts, complete
}

// absIn makes the test log path p absolute, relative to dir.
func absIn(p, dir string) string {
	if !filepath.IsAbs(p) {
		p = filepath.Join(dir, p)
	}
	return filepath.Clean(p)
}

// started reports whether the test log line "open <arg>" (abs: arg resolved against the
// test's directory dir; rest: the lines after it) is os.StartProcess starting a program.
// It logs the program that way, followed by a stat of the command's directory when it
// has one (cmd.Dir, without SysProcAttr), where a program named by a relative path is
// found. Any executable file opened counts (one opened to be read: more dependencies,
// never fewer); a program removed before the test ends (built into t.TempDir()) is not
// seen. The program may run in dir, or in the directory of a stat that follows (a stat
// the test makes itself looks the same). Only the next line counts: another goroutine's
// line in between (parallel tests) hides a cmd.Dir.
func started(arg, abs, dir string, rest []string) (goStart, bool) {
	s := goStart{prog: abs, dirs: []string{dir}}
	if len(rest) > 0 {
		if op, d, ok := strings.Cut(rest[0], " "); ok && op == "stat" {
			d = absIn(d, dir)
			if fi, err := os.Stat(d); err == nil && fi.IsDir() {
				s.dirs = append(s.dirs, d)
				if p := absIn(arg, d); !filepath.IsAbs(arg) && !executable(abs) && executable(p) {
					s.prog = p
				}
			}
		}
	}
	return s, executable(s.prog)
}

// executable reports whether p is a file a program can be started from.
func executable(p string) bool {
	fi, err := os.Stat(p)
	if err != nil || !fi.Mode().IsRegular() {
		return false
	}
	if runtime.GOOS == "windows" {
		switch strings.ToLower(filepath.Ext(p)) {
		case ".exe", ".com", ".bat", ".cmd":
			return true
		}
		return false
	}
	return fi.Mode()&0o111 != 0
}

// GoTestLogs is ParseTestLog for the test log of Go test binary bin run in cwd, with what
// the programs the test started may have read (absolute paths; dir/** marks a tree,
// every file below dir).
//
//   - Node, Bun and Python processes trace themselves (GoChildren);
//   - a re-execution of bin run as a test binary (no arguments, or a -test. flag first:
//     the helper-process pattern) runs the package's own test code, with a test log of
//     its own in childDir (GoChildLogEnv, <pid>.test giving its directory), read the
//     same way. When that log is missing or cut short (the classic TestHelperProcess
//     ends in os.Exit) what it read is unknown: the test depends on the tree of its Go
//     module (the repository's when the module encloses it), as the test code itself
//     may read anything there (../../testdata);
//   - a re-execution with arguments of its own (a TestMain dispatching to the program's
//     command line, an -exec wrapper; <pid>.cmd) and any other program (a shell, a
//     compiled binary) may read anything below the directory it runs in, as for
//     children smates cannot see into elsewhere (see execAnalysis.opaque), but its
//     command line is unknown: paths it names outside that directory are not seen. So
//     do opens of bin no child accounts for (a test reading its own binary with
//     debug/buildinfo, a child given an environment of its own).
func GoTestLogs(file, cwd, bin, childDir string) (files, env []string) {
	files, env, starts, _ := parseTestLog(file, cwd)
	seen := map[string]bool{}
	for _, f := range files {
		seen[f] = true
	}
	for _, e := range env {
		seen["$"+e] = true
	}
	add := func(fs, es []string) {
		for _, f := range fs {
			if !seen[f] {
				seen[f] = true
				files = append(files, f)
			}
		}
		for _, e := range es {
			if !seen["$"+e] {
				seen["$"+e] = true
				env = append(env, e)
			}
		}
	}
	opaque := func(prog string, dirs ...string) {
		add([]string{prog}, nil)
		for _, d := range dirs {
			add([]string{filepath.Join(d, "**")}, nil)
		}
	}
	binInfo, _ := os.Stat(absIn(bin, cwd))
	var reexecs []goStart
	classify := func(starts []goStart) {
		for _, s := range starts {
			fi, err := os.Stat(s.prog)
			base := strings.TrimSuffix(filepath.Base(s.prog), ".exe")
			switch {
			case err == nil && binInfo != nil && os.SameFile(fi, binInfo):
				reexecs = append(reexecs, s) // see the children's markers
			case tracedPrograms[base] || strings.HasPrefix(base, "python3.") || scriptRuntime(s.prog, false) != "":
				// It traces itself.
			default:
				opaque(s.prog, s.dirs...)
			}
		}
	}
	classify(starts)
	children, unknown := 0, false
	if childDir != "" {
		entries, _ := os.ReadDir(childDir)
		for _, e := range entries {
			pid, kind, _ := strings.Cut(e.Name(), ".")
			if kind != GoChildTestSuffix && kind != GoChildCmdSuffix {
				continue
			}
			children++
			wd, err := os.ReadFile(filepath.Join(childDir, e.Name()))
			switch {
			case err != nil || !filepath.IsAbs(string(wd)):
				unknown = true
			case kind == GoChildCmdSuffix:
				opaque(bin, string(wd))
			default:
				cf, ce, cs, complete := parseTestLog(filepath.Join(childDir, pid+".testlog"), string(wd))
				if !complete {
					unknown = true
					continue
				}
				add(cf, ce)
				classify(cs)
			}
		}
	}
	if unknown {
		add([]string{filepath.Join(goModuleDir(cwd), "**")}, nil)
	}
	// Opens of the binary that no child accounts for: read, or run without smates' test
	// file in its environment. Which ones is not known: they all count as programs.
	if len(reexecs) > children {
		for _, s := range reexecs {
			opaque(s.prog, s.dirs...)
		}
	}
	return files, env
}

// goModuleDir is the directory of the go.mod enclosing dir (the file system root when
// there is none).
func goModuleDir(dir string) string {
	for d := filepath.Clean(dir); ; d = filepath.Dir(d) {
		if _, err := os.Stat(filepath.Join(d, "go.mod")); err == nil || filepath.Dir(d) == d {
			return d
		}
	}
}

var goRunRe = regexp.MustCompile(`^go-(\d+)\.json$`)

// CollectGo reads GoExec records from a trace directory.
func CollectGo(traceDir string) ([]GoRun, error) {
	entries, err := os.ReadDir(traceDir)
	if err != nil {
		return nil, err
	}
	var runs []GoRun
	for _, e := range entries {
		if !goRunRe.MatchString(e.Name()) {
			continue
		}
		data, err := os.ReadFile(filepath.Join(traceDir, e.Name()))
		if err != nil {
			return nil, err
		}
		var r GoRun
		if err := json.Unmarshal(data, &r); err != nil {
			continue
		}
		runs = append(runs, r)
	}
	return runs, nil
}

// IsGoExec reports whether this process was started as a go test -exec wrapper.
func IsGoExec() bool { return os.Getenv(GoExecEnv) != "" }

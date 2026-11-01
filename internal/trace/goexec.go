package trace

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"

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
		case strings.HasPrefix(kv, GoExecEnv+"="), strings.HasPrefix(kv, GoFlagsOrigEnv+"="), strings.HasPrefix(kv, GoExecUserEnv+"="), strings.HasPrefix(kv, "GOFLAGS="):
			continue
		}
		env = append(env, kv)
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
		run.Files, run.Env = ParseTestLog(logFile, cwd)
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
	f, err := os.Open(file)
	if err != nil {
		return nil, nil
	}
	defer f.Close()
	var out []string
	seen := map[string]bool{}
	dir := cwd
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		op, arg, ok := strings.Cut(sc.Text(), " ")
		if !ok {
			continue
		}
		switch op {
		case "getenv":
			if !seen[sc.Text()] {
				seen[sc.Text()] = true
				env = append(env, arg)
			}
		case "chdir":
			if !filepath.IsAbs(arg) {
				arg = filepath.Join(dir, arg)
			}
			dir = filepath.Clean(arg)
		case "open", "stat":
			if !filepath.IsAbs(arg) {
				arg = filepath.Join(dir, arg)
			}
			arg = filepath.Clean(arg)
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
	return out, env
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

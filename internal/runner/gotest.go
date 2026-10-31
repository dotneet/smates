package runner

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/dotneet/smates/internal/gitx"
	"github.com/dotneet/smates/internal/pathutil"
	"github.com/dotneet/smates/internal/state"
	"github.com/dotneet/smates/internal/trace"
)

func init() { Register(goTest{}) }

type goTest struct{}

func (goTest) Name() string { return "go" }

func (goTest) Defaults(repo *gitx.Repo, root string) Defaults {
	return Defaults{
		Tests:   []string{"**/*_test.go"},
		Exclude: []string{"**/testdata/**", "**/vendor/**", "**/_*/**", "**/.*/**"},
		Inputs:  []string{"go.mod", "go.sum", "/go.work", "/go.work.sum", "/.tool-versions"},
		Command: "go test",
	}
}

// Units are package directories.
func (goTest) Units(s *Suite, testFiles []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, f := range testFiles {
		d := path.Dir(f)
		if !seen[d] {
			seen[d] = true
			out = append(out, d)
		}
	}
	return out
}

func (goTest) Run(ctx context.Context, rc *RunContext) (*RunResult, error) {
	var pkgs []string
	for _, u := range rc.Units {
		r := relToSuite(rc.Suite, u)
		if r == "." {
			pkgs = append(pkgs, ".")
		} else {
			pkgs = append(pkgs, "./"+r)
		}
	}
	var testArgs []string
	if rc.All {
		testArgs = []string{"./..."}
	} else {
		testArgs = pkgs
	}
	var env []string
	if rc.Record {
		exe, err := goExecWrapper(rc)
		if err != nil {
			return nil, err
		}
		orig := Getenv(suiteEnv(rc.Suite), "GOFLAGS")
		// A user -exec program in GOFLAGS keeps running the test binaries, behind
		// smates' wrapper.
		var kept []string
		userExec := ""
		for _, f := range strings.Fields(orig) {
			if v, ok := strings.CutPrefix(f, "-exec="); ok {
				userExec = v
				continue
			}
			kept = append(kept, f)
		}
		flags := strings.TrimSpace(strings.Join(kept, " ") + " -exec=" + exe + " -count=1")
		env = []string{"GOFLAGS=" + flags, trace.GoExecEnv + "=1", trace.GoFlagsOrigEnv + "=" + orig, trace.GoExecUserEnv + "=" + userExec}
		// Node and Python processes started by tests are traced too.
		if err := os.MkdirAll(trace.V8Dir(rc.TraceDir), 0o755); err != nil {
			return nil, err
		}
		base := suiteEnv(rc.Suite)
		env = append(env, trace.NodeEnv(rc.TraceDir, rc.Repo.Root, rc.Shims(), base)...)
		env = append(env, trace.PythonEnv(rc.TraceDir, rc.Repo.Root, rc.Shims(), base)[2:3]...) // PYTHONPATH only
	}
	start := time.Now()
	// Packages that fail to build never start a test binary; go test reports them as
	// "FAIL <import path> [build failed]".
	failed := &buildFailures{}
	out := *rc
	out.Stdout = io.MultiWriter(rc.Stdout, failed)
	code, err := runAll(ctx, &out, testArgs, env)
	res := &RunResult{ExitCode: code, Duration: time.Since(start)}
	if err != nil || !rc.Record {
		return res, err
	}
	runs, err := trace.CollectGo(rc.TraceDir)
	if err != nil {
		return res, err
	}
	procs, err := trace.Collect(rc.TraceDir, rc.Repo.Root)
	if err != nil {
		return res, err
	}
	children := trace.GoChildren(procs)
	res.Observations = map[string]*Observation{}
	for _, r := range runs {
		unit, ok := pathutil.Rel(rc.Repo.Root, r.Dir)
		if !ok {
			if filepath.Clean(r.Dir) != rc.Repo.Root {
				continue
			}
			unit = "."
		}
		o := res.Observations[unit]
		if o == nil {
			o = &Observation{Files: map[string]bool{}}
			res.Observations[unit] = o
		}
		for _, f := range r.Files {
			suffix := ""
			switch {
			case strings.HasSuffix(f, string(filepath.Separator)):
				suffix, f = "/", strings.TrimSuffix(f, string(filepath.Separator))
			case strings.HasSuffix(f, "?"):
				suffix, f = "?", strings.TrimSuffix(f, "?")
			}
			if rel, ok := pathutil.Rel(rc.Repo.Root, f); ok && !pathutil.IsVendored(rel) {
				o.AddFile(rel + suffix)
			} else if suffix == "/" && filepath.Clean(f) == rc.Repo.Root {
				o.AddFile("./")
			}
		}
		for f := range children[r.Dir] {
			o.AddFile(f)
		}
		o.DurationMs += r.Ms
		st := overallStatus("", r.Exit)
		if o.Status == "" || st == "fail" {
			o.Status = st
		}
	}
	if len(failed.paths) > 0 {
		mod := modulePath(filepath.Join(rc.Dir(), "go.mod"))
		for _, u := range rc.Units {
			ip := mod
			if r := relToSuite(rc.Suite, u); r != "." {
				ip = mod + "/" + r
			}
			if mod != "" && failed.paths[ip] {
				res.Observations[u] = &Observation{Files: map[string]bool{}, Status: state.Fail}
			}
		}
	}
	return res, nil
}

// Also matches the JSON-escaped form printed by go test -json ("FAIL\\tpkg [build failed]").
var buildFailedRe = regexp.MustCompile(`FAIL(?:\\t|\s)+([^\s\\]+)\s+\[(?:build|setup) failed\]`)

// buildFailures collects import paths of packages go test could not build.
type buildFailures struct {
	mu    sync.Mutex
	buf   []byte
	paths map[string]bool
}

func (b *buildFailures) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.buf = append(b.buf, p...)
	for {
		i := bytes.IndexByte(b.buf, '\n')
		if i < 0 {
			break
		}
		if m := buildFailedRe.FindSubmatch(b.buf[:i]); m != nil {
			if b.paths == nil {
				b.paths = map[string]bool{}
			}
			b.paths[string(m[1])] = true
		}
		b.buf = b.buf[i+1:]
	}
	return len(p), nil
}

// modulePath reads the module path from a go.mod file ("" if unknown).
func modulePath(gomod string) string {
	data, err := os.ReadFile(gomod)
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(data), "\n") {
		if f := strings.Fields(line); len(f) >= 2 && f[0] == "module" {
			return strings.Trim(f[1], `"`)
		}
	}
	return ""
}

// runAll runs go test without the "./..." expansion trick: go accepts many package
// arguments, chunked if needed.
func runAll(ctx context.Context, rc *RunContext, args []string, env []string) (int, error) {
	worst := 0
	for _, b := range chunk(args) {
		argv, err := rc.CommandLine(nil, b)
		if err != nil {
			return 1, err
		}
		code, err := rc.Exec(ctx, argv, env)
		if err != nil {
			return code, err
		}
		worst = max(worst, code)
	}
	return worst, nil
}

// goExecWrapper returns a path to the smates binary usable inside GOFLAGS (which is
// split on spaces), linking it into the trace directory when its path has spaces.
func goExecWrapper(rc *RunContext) (string, error) {
	if rc.Self == "" {
		return "", fmt.Errorf("cannot locate the smates executable for Go tracing")
	}
	if !strings.ContainsAny(rc.Self, " \t") {
		return rc.Self, nil
	}
	link := filepath.Join(rc.TraceDir, "smates-goexec")
	if err := os.Symlink(rc.Self, link); err != nil {
		return "", err
	}
	return link, nil
}

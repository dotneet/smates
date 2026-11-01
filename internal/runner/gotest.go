package runner

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/dotneet/smates/internal/depkey"
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

// Units are package directories, or with granularity "func" top-level test functions
// ("dir#TestName", see gofunc.go).
func (goTest) Units(s *Suite, testFiles []string) []string {
	if funcGranularity(s) {
		return funcUnits(s, testFiles)
	}
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
	if funcGranularity(rc.Suite) {
		return runFuncs(ctx, rc)
	}
	return runPackages(ctx, rc, rc.Units, rc.All, "", nil)
}

// runPackages runs go test on package units (all: on ./..., letting go find them),
// restricted to the tests matching run when it is set; out also receives the output.
// When recording, observations are keyed by package unit.
func runPackages(ctx context.Context, rc *RunContext, units []string, all bool, run string, out io.Writer) (*RunResult, error) {
	var pkgs []string
	for _, u := range units {
		pkgs = append(pkgs, pkgArg(rc.Suite, u))
	}
	var testArgs []string
	if all {
		testArgs = []string{"./..."}
	} else {
		testArgs = pkgs
	}
	var extra []string
	if run != "" {
		extra = []string{"-run", run}
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
	sub := *rc
	sub.Stdout = io.MultiWriter(rc.Stdout, failed)
	if out != nil {
		sub.Stdout = io.MultiWriter(rc.Stdout, failed, out)
	}
	code, err := runAll(ctx, &sub, extra, testArgs, env)
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
	res.Warnings = append(res.Warnings, exportV8(rc)...)
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
		addTestLog(o, rc.Repo.Root, r.Files, r.Env)
		for f := range children[r.Dir] {
			o.AddFile(f)
		}
		o.DurationMs += r.Ms
		st := overallStatus("", r.Exit)
		if o.Status == "" || st == state.Fail {
			o.Status = st
		}
	}
	addCoverPkgDeps(ctx, rc, res)
	if len(failed.paths) > 0 || len(failed.output) > 0 {
		modDir, mod := findModule(rc.Repo.Root, rc.Dir())
		for _, u := range units {
			ip := importPath(modDir, mod, filepath.Join(rc.Repo.Root, filepath.FromSlash(u)))
			if ip == "" {
				continue
			}
			if failed.paths[ip] {
				res.Observations[u] = &Observation{Files: map[string]bool{}, Status: state.Fail}
			}
			if o := res.Observations[u]; o != nil && o.Status == state.Fail {
				o.Failure = failed.output[ip]
			}
		}
	}
	return res, nil
}

// pkgArg is the go test argument naming the package directory unit (repo-relative).
func pkgArg(s *Suite, unit string) string {
	if r := relToSuite(s, unit); r != "." {
		return "./" + r
	}
	return "."
}

// addTestLog adds what a Go test log recorded (absolute paths from trace.ParseTestLog:
// a directory that was listed ends in a separator, an existence check in "?"; and the
// environment variables looked up) to an observation.
func addTestLog(o *Observation, root string, files, env []string) {
	for _, f := range files {
		kind := depkey.KindContent
		switch {
		case strings.HasSuffix(f, string(filepath.Separator)):
			kind, f = depkey.KindListing, strings.TrimSuffix(f, string(filepath.Separator))
		case strings.HasSuffix(f, "?"):
			kind, f = depkey.KindExistence, strings.TrimSuffix(f, "?")
		}
		if rel, ok := pathutil.Rel(root, f); ok && !pathutil.IsVendored(rel) {
			o.AddFile(depkey.Make(kind, rel))
		} else if kind == depkey.KindListing && filepath.Clean(f) == root {
			o.AddFile(depkey.Listing("."))
		}
	}
	for _, name := range env {
		o.AddFile(depkey.Env(name))
	}
}

// Also matches the JSON-escaped form printed by go test -json ("FAIL\\tpkg [build failed]").
var buildFailedRe = regexp.MustCompile(`FAIL(?:\\t|\s)+([^\s\\]+)\s+\[(?:build|setup) failed\]`)

// goSummaryRe matches the line go test prints when a package finishes.
var goSummaryRe = regexp.MustCompile(`^(ok|FAIL|\?)\s+(\S+)(\s|$)`)

// maxGoPending bounds the output kept for the package currently printing.
const maxGoPending = 16 << 10

// buildFailures collects import paths of packages go test could not build, and the
// output of failing packages (go test prints each package's output in one block,
// followed by its summary line).
type buildFailures struct {
	mu      sync.Mutex
	buf     []byte
	paths   map[string]bool
	pending []byte
	output  map[string]string // import path -> failure output (bounded)
}

// collect accumulates one output line, attributing it to the package whose summary
// line follows.
func (b *buildFailures) collect(line []byte) {
	if m := goSummaryRe.FindSubmatch(line); m != nil {
		if string(m[1]) == "FAIL" && len(bytes.TrimSpace(b.pending)) > 0 {
			if b.output == nil {
				b.output = map[string]string{}
			}
			out := b.pending
			if len(out) > trace.MaxMessage {
				out = out[len(out)-trace.MaxMessage:] // the failures and panics are at the end
			}
			b.output[string(m[2])] = trace.JoinMessage(b.output[string(m[2])], string(out))
		}
		b.pending = b.pending[:0]
		return
	}
	b.pending = append(b.pending, line...)
	b.pending = append(b.pending, '\n')
	if extra := len(b.pending) - maxGoPending; extra > 0 {
		b.pending = append(b.pending[:0], b.pending[extra:]...)
	}
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
		b.collect(bytes.TrimRight(b.buf[:i], "\r"))
		b.buf = b.buf[i+1:]
	}
	return len(p), nil
}

// findModule finds the go.mod governing dir (searching up to root) and returns its
// directory and module path ("" if there is none).
func findModule(root, dir string) (string, string) {
	for d := dir; ; d = filepath.Dir(d) {
		if mod := modulePath(filepath.Join(d, "go.mod")); mod != "" {
			return d, mod
		}
		if d == root || len(d) <= len(root) {
			return "", ""
		}
	}
}

// importPath is the import path of package directory pkgDir in the module rooted at
// modDir ("" if it is not in the module).
func importPath(modDir, mod, pkgDir string) string {
	if mod == "" {
		return ""
	}
	r, err := filepath.Rel(modDir, pkgDir)
	if err != nil || r == ".." || strings.HasPrefix(r, ".."+string(filepath.Separator)) {
		return ""
	}
	if r == "." {
		return mod
	}
	return mod + "/" + filepath.ToSlash(r)
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
// arguments, chunked if needed; extra flags go before them.
func runAll(ctx context.Context, rc *RunContext, extra, args []string, env []string) (int, error) {
	worst := 0
	for i, b := range chunk(args) {
		argv, err := rc.CommandLine(extra, b)
		if err != nil {
			return 1, err
		}
		env, err := trace.SeparateV8(env, rc.TraceDir, i)
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

// addCoverPkgDeps adds the source files of the -coverpkg packages to every observation
// of res. With -coverpkg, go test builds the packages it names into every test binary of
// the invocation: one that does not compile fails them all, whatever they import. That
// holds for function units too: they run with the user's command when not recording.
func addCoverPkgDeps(ctx context.Context, rc *RunContext, res *RunResult) {
	pats := coverPkgs(rc)
	if len(pats) == 0 || res == nil || len(res.Observations) == 0 {
		return
	}
	files, err := goPackageFiles(ctx, rc, pats)
	if err != nil {
		res.Warnings = append(res.Warnings, "could not list the -coverpkg packages ("+err.Error()+"); tests depend only on what they import")
	}
	for _, o := range res.Observations {
		for _, f := range files {
			o.AddFile(f)
		}
	}
}

// coverPkgs returns the package patterns of a -coverpkg flag on the suite command, the
// arguments passed through or GOFLAGS (nil when there is none).
func coverPkgs(rc *RunContext) []string {
	words, _ := SplitWords(rc.Suite.Command)
	words = append(words, rc.ExtraArgs...)
	words = append(words, strings.Fields(Getenv(suiteEnv(rc.Suite), "GOFLAGS"))...)
	var pats []string
	for i, w := range words {
		flag, value, hasValue := strings.Cut(strings.TrimPrefix(strings.TrimPrefix(w, "-"), "-"), "=")
		if flag != "coverpkg" || w == flag {
			continue
		}
		if !hasValue {
			if i+1 >= len(words) {
				continue
			}
			value = words[i+1]
		}
		for _, p := range strings.Split(value, ",") {
			if p = strings.TrimSpace(p); p != "" {
				pats = append(pats, p)
			}
		}
	}
	return pats
}

// goPackageFiles lists the Go source files (repository-relative) of the packages
// matching patterns, resolved in the suite directory.
func goPackageFiles(ctx context.Context, rc *RunContext, patterns []string) ([]string, error) {
	args := append([]string{"list", "-e", "-find", "-f", "{{.Dir}}{{range .GoFiles}}|{{.}}{{end}}{{range .CgoFiles}}|{{.}}{{end}}", "--"}, patterns...)
	cmd := exec.CommandContext(ctx, "go", args...)
	cmd.Dir = rc.Dir()
	cmd.Env = MergeEnv(baseEnv(), suiteEnv(rc.Suite), []string{"GOFLAGS="})
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("go list: %v: %s", err, strings.TrimSpace(stderr.String()))
	}
	var files []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		parts := strings.Split(line, "|")
		for _, name := range parts[1:] {
			if rel, ok := pathutil.Rel(rc.Repo.Root, filepath.Join(parts[0], name)); ok && !pathutil.IsVendored(rel) {
				files = append(files, rel)
			}
		}
	}
	return files, nil
}

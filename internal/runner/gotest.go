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
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/dotneet/smates/internal/config"
	"github.com/dotneet/smates/internal/depkey"
	"github.com/dotneet/smates/internal/gitx"
	"github.com/dotneet/smates/internal/pathutil"
	"github.com/dotneet/smates/internal/state"
	"github.com/dotneet/smates/internal/trace"
)

func init() { Register(goTest{}) }

type goTest struct{}

func (goTest) Name() string { return "go" }

func (g goTest) Defaults(repo *gitx.Repo, root string) Defaults {
	return g.DefaultsForSuite(repo, config.Suite{Root: root})
}

// DefaultsForSuite implements SuiteDefaulter: which nested modules belong to the suite
// depends on GOWORK in its env:.
func (goTest) DefaultsForSuite(repo *gitx.Repo, c config.Suite) Defaults {
	exclude := []string{"**/testdata/**", "**/vendor/**", "**/_*/**", "**/.*/**"}
	if repo != nil {
		// Packages of a nested module are not the suite's: go test of the outer module
		// cannot build them (and ./... skips them), unless the go.work workspace has them.
		dir := filepath.Join(repo.Root, filepath.FromSlash(c.Root))
		nested, _ := nestedGoModules(repo, dir, goWorkEnv(c.Env))
		for _, m := range nested {
			exclude = append(exclude, m+"/**")
		}
	}
	return Defaults{
		Tests:   []string{"**/*_test.go"},
		Exclude: exclude,
		Inputs:  []string{"go.mod", "go.sum", "/go.work", "/go.work.sum", "/.tool-versions"},
		Command: "go test",
	}
}

// goWorkEnv is the GOWORK go test sees with the suite's env: on top of smates' own.
func goWorkEnv(env map[string]string) string {
	if v, ok := env["GOWORK"]; ok {
		return v
	}
	return os.Getenv("GOWORK")
}

// nestedGoModules finds the Go modules below the suite directory dir (absolute), as
// directories relative to it: those outside the go.work workspace go uses there
// (other: the suite cannot run their tests) and those in it (workspace: go test runs
// them when named, but ./... does not reach them). gowork is $GOWORK.
func nestedGoModules(repo *gitx.Repo, dir, gowork string) (other, workspace []string) {
	use := goWorkModules(repo.Root, dir, gowork)
	for _, p := range goModDirs(repo, dir) {
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			continue
		}
		rel = filepath.ToSlash(rel)
		if slices.ContainsFunc(other, func(o string) bool { return pathutil.Under(rel, o) }) {
			continue // everything below belongs to that module
		}
		if use[filepath.Clean(p)] {
			workspace = append(workspace, rel)
		} else {
			other = append(other, rel)
		}
	}
	return other, workspace
}

// goModDirs lists the directories (absolute, sorted) below dir that have a go.mod,
// leaving out what go ignores (testdata, vendor, names starting with "." or "_") and
// node_modules. git lists them (tracked or untracked, not ignored: a build tree stays
// unread); outside a git repository the directory is walked.
func goModDirs(repo *gitx.Repo, dir string) []string {
	ignored := func(rel string) bool {
		for _, seg := range strings.Split(rel, "/") {
			if seg == "testdata" || seg == "vendor" || seg == "node_modules" || strings.HasPrefix(seg, ".") || strings.HasPrefix(seg, "_") {
				return true
			}
		}
		return false
	}
	var out []string
	add := func(rel string) {
		if rel != "." && !ignored(rel) {
			out = append(out, filepath.Join(dir, filepath.FromSlash(rel)))
		}
	}
	base, ok := repoDir(repo.Root, dir)
	spec := ":(glob)**/go.mod"
	if base != "." {
		spec = ":(glob)" + base + "/**/go.mod"
	}
	listed, err := repo.Git("-c", "core.quotepath=off", "ls-files", "-co", "--exclude-standard", "-z", "--", spec)
	if err != nil || !ok {
		filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
			if err != nil || !d.IsDir() || p == dir {
				return nil
			}
			rel, _ := filepath.Rel(dir, p)
			if ignored(filepath.ToSlash(rel)) {
				return filepath.SkipDir
			}
			if exists(filepath.Join(p, "go.mod")) {
				add(filepath.ToSlash(rel))
			}
			return nil
		})
		return out
	}
	for _, f := range strings.Split(listed, "\x00") {
		if path.Base(f) != "go.mod" || !exists(filepath.Join(repo.Root, filepath.FromSlash(f))) {
			continue // a tracked go.mod deleted from the working tree
		}
		add(pathutil.RelTo(path.Dir(f), base))
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// goWorkModules returns the module directories (absolute) of the go.work file go uses
// in dir: $GOWORK, or the first go.work found in dir or its parents up to root (none
// with GOWORK=off).
func goWorkModules(root, dir, gowork string) map[string]bool {
	file := ""
	switch {
	case gowork == "off":
		return nil
	case gowork != "":
		file = gowork
	default:
		for d := dir; file == ""; d = filepath.Dir(d) {
			if _, err := os.Stat(filepath.Join(d, "go.work")); err == nil {
				file = filepath.Join(d, "go.work")
			} else if d == root || len(d) <= len(root) {
				return nil
			}
		}
	}
	data, err := os.ReadFile(file)
	if err != nil {
		return nil
	}
	out := map[string]bool{}
	block := false
	for _, line := range strings.Split(string(data), "\n") {
		if i := strings.Index(line, "//"); i >= 0 {
			line = line[:i]
		}
		f := strings.Fields(line)
		switch {
		case len(f) == 0:
			continue
		case block && f[0] == ")":
			block = false
			continue
		case !block && len(f) >= 2 && f[0] == "use" && f[1] == "(":
			block = true
			continue
		case !block && len(f) >= 2 && f[0] == "use":
			f = f[1:]
		case !block:
			continue
		}
		p := filepath.FromSlash(strings.Trim(f[0], "\""))
		if !filepath.IsAbs(p) {
			p = filepath.Join(filepath.Dir(file), p)
		}
		out[filepath.Clean(p)] = true
	}
	return out
}

// allPkgArgs are the go test arguments running every package of the suite: ./... and
// the nested modules of the go.work workspace, which ./... does not reach.
func allPkgArgs(rc *RunContext) []string {
	args := []string{"./..."}
	_, ws := nestedGoModules(rc.Repo, rc.Dir(), goWorkEnv(rc.Suite.Env))
	for _, m := range ws {
		args = append(args, "./"+m+"/...")
	}
	return args
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
	var testArgs []string
	if all {
		testArgs = allPkgArgs(rc)
	} else {
		testArgs = pkgArgs(rc.Suite, units)
	}
	extra := runFlag(run)
	var env []string
	if rc.Fresh && !rc.Record {
		// go test does not recheck files outside the module before reusing a cached
		// result: a change there would pass unseen.
		env = []string{"GOFLAGS=" + strings.TrimSpace(Getenv(suiteEnv(rc.Suite), "GOFLAGS")+" -count=1")}
	}
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
		env = append(env, childTraceEnv(rc, rc.TraceDir)...)
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
	if err != nil {
		return res, err
	}
	if !rc.Record {
		if rc.TraceDir != "" {
			res.Observations = failed.observations(rc)
		}
		return res, nil
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
		unit, ok := repoDir(rc.Repo.Root, r.Dir)
		if !ok {
			continue
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
		for _, u := range units {
			ip := failed.key(rc, u)
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

// pkgArgs is pkgArg of each package directory unit.
func pkgArgs(s *Suite, units []string) []string {
	var out []string
	for _, u := range units {
		out = append(out, pkgArg(s, u))
	}
	return out
}

// runFlag is the go test flag restricting a run to the tests matching run (none if "").
func runFlag(run string) []string {
	if run == "" {
		return nil
	}
	return []string{"-run", run}
}

// repoDir is the repo-relative form of the absolute package directory dir ("." for
// the root); ok is false when dir is outside the repository.
func repoDir(root, dir string) (string, bool) {
	if rel, ok := pathutil.Rel(root, dir); ok {
		return rel, true
	}
	if filepath.Clean(dir) == root {
		return ".", true
	}
	return "", false
}

// childTraceEnv is the environment tracing the Node and Python processes a Go test
// starts, writing their traces under dir.
func childTraceEnv(rc *RunContext, dir string) []string {
	base := suiteEnv(rc.Suite)
	node := trace.NodeEnv(dir, rc.Repo.Root, rc.Shims(), base)
	return slices.Concat(node, trace.PythonEnv(dir, rc.Repo.Root, rc.Shims(), base)[2:3]) // PYTHONPATH only
}

// lastMessage keeps the last trace.MaxMessage bytes of a test's output.
func lastMessage[T string | []byte](out T) T {
	if len(out) > trace.MaxMessage {
		return out[len(out)-trace.MaxMessage:]
	}
	return out
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

// buildFailures collects import paths of packages go test could not build, the
// output of failing packages (go test prints each package's output in one block,
// followed by its summary line) and the status each summary line gives.
type buildFailures struct {
	mu      sync.Mutex
	buf     []byte
	paths   map[string]bool
	pending []byte
	output  map[string]string // import path -> failure output (bounded)
	status  map[string]string // import path -> state.Pass / state.Fail
}

// collect accumulates one output line, attributing it to the package whose summary
// line follows.
func (b *buildFailures) collect(line []byte) {
	if m := goSummaryRe.FindSubmatch(line); m != nil {
		if b.status == nil {
			b.status = map[string]string{}
		}
		switch string(m[1]) {
		case "ok":
			if b.status[string(m[2])] != state.Fail {
				b.status[string(m[2])] = state.Pass
			}
		case "FAIL":
			b.status[string(m[2])] = state.Fail
		}
		if string(m[1]) == "FAIL" && len(bytes.TrimSpace(b.pending)) > 0 {
			if b.output == nil {
				b.output = map[string]string{}
			}
			out := lastMessage(b.pending) // the failures and panics are at the end
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

// observations returns the per-package outcomes of a run that was not traced, from the
// summary lines go test printed: statuses and failure output, no dependencies.
func (b *buildFailures) observations(rc *RunContext) map[string]*Observation {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.status) == 0 {
		return nil
	}
	obs := map[string]*Observation{}
	for _, u := range rc.Units {
		ip := b.key(rc, u)
		st, ok := b.status[ip]
		if ip == "" || !ok {
			continue
		}
		o := &Observation{Files: map[string]bool{}, Status: st}
		if st == state.Fail {
			o.Failure = b.output[ip]
		}
		obs[u] = o
	}
	return obs
}

// key is the name go test printed for the package directory unit u in its summary and
// build failure lines: its import path (in the module of its own directory, which in a
// workspace is not the suite's), or the relative argument naming it when go test could
// not resolve that ("FAIL ./x [setup failed]").
func (b *buildFailures) key(rc *RunContext, u string) string {
	if arg := pkgArg(rc.Suite, u); b.paths[arg] || b.status[arg] != "" || b.output[arg] != "" {
		return arg
	}
	return dirImportPath(rc.Repo.Root, u)
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

// Build flags that change which files go test compiles, forwarded to go list. Others
// (test flags, -exec, -coverpkg, -count) are not go list's and are left out.
var (
	goListValueFlags = map[string]bool{"tags": true, "mod": true, "modfile": true, "overlay": true}
	goListBoolFlags  = map[string]bool{"race": true, "msan": true, "asan": true}
)

// goListFlags returns the build flags of GOFLAGS, the suite command and the arguments
// passed through (in that order: later ones win, as with go test) that go list needs to
// see the files go test builds.
func goListFlags(rc *RunContext) []string {
	words := strings.Fields(Getenv(suiteEnv(rc.Suite), "GOFLAGS"))
	cmd, _ := SplitWords(rc.Suite.Command)
	words = append(append(words, cmd...), rc.ExtraArgs...)
	var out []string
	for i := 0; i < len(words); i++ {
		w := words[i]
		if w == "--" || w == "-args" || w == "--args" {
			break // what follows goes to the test binary
		}
		if !strings.HasPrefix(w, "-") {
			continue
		}
		flag, value, hasValue := strings.Cut(strings.TrimPrefix(strings.TrimPrefix(w, "-"), "-"), "=")
		switch {
		case goListValueFlags[flag] && hasValue:
			out = append(out, "-"+flag+"="+value)
		case goListValueFlags[flag] && i+1 < len(words):
			out = append(out, "-"+flag+"="+words[i+1])
			i++
		case goListBoolFlags[flag]:
			out = append(out, w)
		}
	}
	return out
}

// goPackageFiles lists the Go source files (repository-relative) of the packages
// matching patterns, resolved in the suite directory. Files excluded by build
// constraints count too (minus tests): go test may build them under tags or a platform
// this listing cannot see, and listing too many only selects more tests.
func goPackageFiles(ctx context.Context, rc *RunContext, patterns []string) ([]string, error) {
	const format = "{{.Dir}}{{range .GoFiles}}|{{.}}{{end}}{{range .CgoFiles}}|{{.}}{{end}}{{range .IgnoredGoFiles}}|{{.}}{{end}}"
	args := append(append([]string{"list", "-e", "-find"}, goListFlags(rc)...), "-f", format, "--")
	args = append(args, patterns...)
	cmd := exec.CommandContext(ctx, "go", args...)
	cmd.Dir = rc.Dir()
	// The relevant GOFLAGS are on the command line; the rest (-exec, -coverpkg, test
	// flags) must not reach go list.
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
			if strings.HasSuffix(name, "_test.go") || !strings.HasSuffix(name, ".go") {
				continue // IgnoredGoFiles lists tests too; -coverpkg builds no test files
			}
			if rel, ok := pathutil.Rel(rc.Repo.Root, filepath.Join(parts[0], name)); ok && !pathutil.IsVendored(rel) {
				files = append(files, rel)
			}
		}
	}
	return files, nil
}

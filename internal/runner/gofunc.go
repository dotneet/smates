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
// When the binary cannot be built with coverage (or the suite command is not a plain
// `go test`), the package is recorded as a whole: every selected function gets the
// package's observation, with every Go file of the closure as content.
//
// Without recording, selected functions run with go test -run '^(A|B)$' and their
// outcomes are read from go test's output.

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/dotneet/smates/internal/config"
	"github.com/dotneet/smates/internal/depkey"
	"github.com/dotneet/smates/internal/gitx"
	"github.com/dotneet/smates/internal/pathutil"
	"github.com/dotneet/smates/internal/state"
	"github.com/dotneet/smates/internal/static/golang"
	"github.com/dotneet/smates/internal/trace"
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

// testFileCache memoizes parsed test files by content hash.
var testFileCache sync.Map

type parsedTestFile struct {
	tf  *golang.TestFile
	err error
}

// readTestFile parses a repo-relative _test.go file.
func readTestFile(root, rel string) (*golang.TestFile, error) {
	if root == "" {
		return nil, fmt.Errorf("repository root unknown")
	}
	abs := filepath.Join(root, filepath.FromSlash(rel))
	fi, err := os.Stat(abs)
	if err != nil {
		return nil, err
	}
	// Selection looks each test file up once per function of its package: a file whose
	// size and modification time are unchanged, and not so recent that a rewrite could
	// keep them, is not read again.
	st := fileStat{fi.Size(), fi.ModTime()}
	if v, ok := testFileStats.Load(abs); ok && v.(statHash).stat == st && time.Since(st.mod) > 2*time.Second {
		if p, ok := testFileCache.Load(v.(statHash).hash); ok {
			return p.(parsedTestFile).tf, p.(parsedTestFile).err
		}
	}
	src, err := os.ReadFile(abs)
	if err != nil {
		return nil, err
	}
	key := gitx.HashBytes(src)
	testFileStats.Store(abs, statHash{st, key})
	if v, ok := testFileCache.Load(key); ok {
		p := v.(parsedTestFile)
		return p.tf, p.err
	}
	tf, err := golang.ParseTestFile(src)
	testFileCache.Store(key, parsedTestFile{tf, err})
	return tf, err
}

type fileStat struct {
	size int64
	mod  time.Time
}

type statHash struct {
	stat fileStat
	hash string
}

// testFileStats maps a test file's absolute path to its last seen stat and hash.
var testFileStats sync.Map

// funcUnits lists the test functions of the test files. A file that does not parse
// yields its package directory as a unit (the package does not compile; it runs, and
// fails, as a whole).
func funcUnits(s *Suite, testFiles []string) []string {
	seen := map[string]bool{}
	var out []string
	add := func(u string) {
		if !seen[u] {
			seen[u] = true
			out = append(out, u)
		}
	}
	for _, f := range testFiles {
		d := path.Dir(f)
		tf, err := readTestFile(s.RepoRoot, f)
		if err != nil {
			add(d)
			continue
		}
		for _, n := range tf.Tests {
			add(FuncUnit(d, n))
		}
	}
	return out
}

// MapStatic implements StaticMapper: a test function depends on the declarations of the
// Go files of its static closure (their bodies only when it executed them, which
// recording adds), on its own test file and the test files it needs, and on the
// listing of its package directory.
func (goTest) MapStatic(s *Suite, unit string, closure []string) []string {
	dir, name, ok := SplitFuncUnit(unit)
	if !funcGranularity(s) || !ok {
		return closure
	}
	content := testFileDeps(s.RepoRoot, dir, name, closure)
	out := make([]string, 0, len(closure)+1)
	for _, f := range closure {
		switch {
		case !strings.HasSuffix(f, ".go"), content[f]:
			out = append(out, f)
		case strings.HasSuffix(f, "_test.go") && path.Dir(f) != dir:
			out = append(out, f) // not expected: test files of other packages are not linked
		default:
			out = append(out, depkey.Decls(f))
		}
	}
	return append(out, depkey.Listing(dir))
}

// testFileDeps returns the test files of package directory dir (among files) whose
// content test function name depends on: the file declaring it, files running code for
// every test, and, transitively, files declaring a name one of those mentions. Files
// that do not parse count too.
func testFileDeps(root, dir, name string, files []string) map[string]bool {
	infos := map[string]*golang.TestFile{}
	need := map[string]bool{}
	var queue []string
	for _, f := range files {
		if path.Dir(f) != dir || !strings.HasSuffix(f, "_test.go") {
			continue
		}
		tf, err := readTestFile(root, f)
		if err != nil {
			need[f] = true
			continue
		}
		infos[f] = tf
		if tf.Global || slices.Contains(tf.Tests, name) {
			need[f] = true
			queue = append(queue, f)
		}
	}
	for len(queue) > 0 {
		f := queue[len(queue)-1]
		queue = queue[:len(queue)-1]
		uses := infos[f].Uses
		for g, tf := range infos {
			if need[g] {
				continue
			}
			for _, d := range tf.Decls {
				if uses[d] {
					need[g] = true
					queue = append(queue, g)
					break
				}
			}
		}
	}
	return need
}

// ---- running ----

// maxRunRegex bounds the -run pattern of one go test invocation; packages whose
// selected functions need more run as a whole.
const maxRunRegex = 16 << 10

// runRegex is the -run pattern matching exactly the given top-level test names.
func runRegex(names []string) string {
	names = slices.Clone(names)
	sort.Strings(names)
	names = slices.Compact(names)
	q := make([]string, len(names))
	for i, n := range names {
		q[i] = regexp.QuoteMeta(n)
	}
	return "^(" + strings.Join(q, "|") + ")$"
}

// goInvocation is one go test call over package directories, restricted to run.
type goInvocation struct {
	dirs []string
	run  string // "" = every test
}

// plainInvocations groups the selected functions into go test calls: one -run pattern
// for as many packages as fit (a function of the same name in another of those
// packages runs too, which is harmless), whole packages when a package's own pattern
// is too long.
func plainInvocations(dirs []string, funcs map[string][]string) []goInvocation {
	var out []goInvocation
	var cur goInvocation
	var names []string
	flush := func() {
		if len(cur.dirs) > 0 {
			cur.run = runRegex(names)
			out = append(out, cur)
		}
		cur, names = goInvocation{}, nil
	}
	var whole []string
	for _, d := range dirs {
		if len(runRegex(funcs[d])) > maxRunRegex {
			whole = append(whole, d)
			continue
		}
		if len(cur.dirs) > 0 && len(runRegex(append(slices.Clone(names), funcs[d]...))) > maxRunRegex {
			flush()
		}
		cur.dirs = append(cur.dirs, d)
		names = append(names, funcs[d]...)
	}
	flush()
	if len(whole) > 0 {
		out = append(out, goInvocation{dirs: whole})
	}
	return out
}

// splitUnits separates package units from function units (grouped by directory, in
// order of first appearance).
func splitUnits(units []string) (pkgs, dirs []string, funcs map[string][]string) {
	funcs = map[string][]string{}
	for _, u := range units {
		d, n, ok := SplitFuncUnit(u)
		if !ok {
			pkgs = append(pkgs, u)
			continue
		}
		if _, seen := funcs[d]; !seen {
			dirs = append(dirs, d)
		}
		funcs[d] = append(funcs[d], n)
	}
	return pkgs, dirs, funcs
}

// Commands implements Commander (for --dry-run).
func (goTest) Commands(rc *RunContext) ([][]string, error) {
	if !funcGranularity(rc.Suite) {
		return nil, nil
	}
	pkgs, dirs, funcs := splitUnits(rc.Units)
	var out [][]string
	if rc.All && !rc.Suite.Narrowed {
		argv, err := rc.CommandLine(nil, []string{"./..."})
		return [][]string{argv}, err
	}
	if len(pkgs) > 0 {
		var args []string
		for _, u := range pkgs {
			args = append(args, pkgArg(rc.Suite, u))
		}
		argv, err := rc.CommandLine(nil, args)
		if err != nil {
			return nil, err
		}
		out = append(out, argv)
	}
	for _, inv := range plainInvocations(dirs, funcs) {
		var args []string
		for _, d := range inv.dirs {
			args = append(args, pkgArg(rc.Suite, d))
		}
		var extra []string
		if inv.run != "" {
			extra = []string{"-run", inv.run}
		}
		argv, err := rc.CommandLine(extra, args)
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
	pkgs, dirs, funcs := splitUnits(rc.Units)
	res := &RunResult{}
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

// goFlags is the suite command split for building a test binary and running it.
type goFlags struct {
	build []string // flags for go list / go test -c
	test  []string // -test.* flags for the binary
	tail  []string // arguments after -args
	exec  []string // a user -exec program
	p     int      // -p: packages built and tested at once (0 = the default)
	// why is set when per-function recording cannot be used with this command.
	why string
}

// Test flags of go test (go help testflag) that take a value.
var goTestValueFlags = map[string]bool{
	"bench": true, "benchtime": true, "blockprofile": true, "blockprofilerate": true, "count": true,
	"coverprofile": true, "cpu": true, "cpuprofile": true, "fuzz": true, "fuzzcachedir": true,
	"fuzzminimizetime": true, "fuzztime": true, "list": true, "memprofile": true, "memprofilerate": true,
	"mutexprofile": true, "mutexprofilefraction": true, "outputdir": true, "parallel": true, "run": true,
	"shuffle": true, "skip": true, "timeout": true, "trace": true,
	// Flags of the go command that take a value and only matter when running tests.
	"exec": true, "covermode": true, "coverpkg": true, "vet": true, "o": true,
}

// Boolean test flags.
var goTestBoolFlags = map[string]bool{"v": true, "short": true, "failfast": true, "fullpath": true, "benchmem": true, "json": true, "cover": true, "c": true}

// Build flags taking a value in a separate word.
var goBuildValueFlags = map[string]bool{
	"asmflags": true, "buildmode": true, "compiler": true, "gccgoflags": true, "gcflags": true, "installsuffix": true,
	"ldflags": true, "mod": true, "modfile": true, "overlay": true, "pgo": true, "pkgdir": true, "tags": true,
	"toolexec": true, "C": true, "p": true,
}

// parseGoFlags splits the suite command (with the run's extra arguments).
func parseGoFlags(rc *RunContext) goFlags {
	var g goFlags
	argv, err := rc.CommandLine(nil, nil)
	if err != nil {
		g.why = err.Error()
		return g
	}
	if len(argv) < 2 || strings.TrimSuffix(filepath.Base(argv[0]), ".exe") != "go" || argv[1] != "test" {
		g.why = "the suite command is not `go test ...`"
		return g
	}
	// GOFLAGS first: flags on the command line come later and win (the binary, like the
	// flag package, takes the last value of a repeated flag).
	timeout := false
	g.words(strings.Fields(Getenv(suiteEnv(rc.Suite), "GOFLAGS")), true, &timeout)
	g.words(argv[2:], false, &timeout)
	if !timeout {
		g.test = append(g.test, "-test.timeout=10m0s") // go test's default
	}
	return g
}

// words translates go test arguments: from GOFLAGS (fromEnv: one flag per word; build
// flags are left to the go command, which reads GOFLAGS itself) or from the command.
func (g *goFlags) words(words []string, fromEnv bool, timeout *bool) {
	where := "the suite command"
	if fromEnv {
		where = "GOFLAGS"
	}
	for i := 0; i < len(words); i++ {
		w := words[i]
		if !fromEnv && (w == "-args" || w == "--args") {
			g.tail = words[i+1:]
			break
		}
		if !strings.HasPrefix(w, "-") || w == "-" || w == "--" {
			continue // package patterns: smates names the package itself
		}
		name, val, hasVal := strings.Cut(strings.TrimLeft(w, "-"), "=")
		short := strings.TrimPrefix(name, "test.")
		takesValue := goTestValueFlags[short] || (goBuildValueFlags[name] && !strings.HasPrefix(name, "test."))
		if takesValue && !hasVal && !fromEnv && i+1 < len(words) {
			i++
			val, hasVal = words[i], true
		}
		if strings.HasPrefix(name, "test.") {
			switch short {
			case "run", "skip", "bench", "fuzz", "list":
				g.why = where + " passes -" + name
			case "timeout":
				*timeout = true
			}
			g.test = append(g.test, "-"+name+suffixValue(val, hasVal))
			continue
		}
		switch {
		case name == "run" || name == "skip" || name == "bench" || name == "fuzz" || name == "list" || name == "c" || name == "o":
			g.why = where + " passes -" + name
		case name == "exec":
			g.exec = strings.Fields(val)
		case name == "timeout" || name == "parallel" || name == "cpu" || name == "count" || name == "shuffle":
			*timeout = *timeout || name == "timeout"
			g.test = append(g.test, "-test."+name+"="+val)
		case name == "v" || name == "short" || name == "failfast" || name == "fullpath":
			g.test = append(g.test, "-test."+name+suffixValue(val, hasVal))
		case goTestValueFlags[name] || goTestBoolFlags[name]:
			// Profiles, -json, coverage settings (smates sets its own), -vet: not used.
		default:
			if name == "p" {
				if n, err := strconv.Atoi(val); err == nil && n > 0 {
					g.p = n
				}
			}
			if fromEnv {
				continue
			}
			if hasVal && !strings.Contains(w, "=") {
				g.build = append(g.build, w, val) // the value was the next word
			} else {
				g.build = append(g.build, w)
			}
		}
	}
}

func suffixValue(v string, has bool) string {
	if !has {
		return ""
	}
	return "=" + v
}

// funcRecorder records the functions of the selected packages.
type funcRecorder struct {
	rc    *RunContext
	flags goFlags
	jobs  int           // functions of one package at once
	procs chan struct{} // bounds test processes over all packages

	mu       sync.Mutex
	outMu    sync.Mutex
	res      *RunResult
	fallback map[string]string // package directory -> why it is recorded as a whole
}

// packages is how many packages are recorded at once: -p, or GOMAXPROCS as in go test.
func (fr *funcRecorder) packages() int {
	if fr.flags.p > 0 {
		return fr.flags.p
	}
	return max(1, runtime.GOMAXPROCS(0))
}

func recordFuncs(ctx context.Context, rc *RunContext, dirs []string, funcs map[string][]string) (*RunResult, error) {
	start := time.Now()
	fr := &funcRecorder{rc: rc, flags: parseGoFlags(rc), jobs: 1, res: &RunResult{Observations: map[string]*Observation{}}, fallback: map[string]string{}}
	if rc.Suite.RecordJobs > 0 {
		fr.jobs = rc.Suite.RecordJobs
	}
	// Packages at once: -p as go test takes it (default GOMAXPROCS), so a suite that
	// runs `go test -p 1` because its packages share a database or a port keeps them
	// apart while recording too.
	fr.procs = make(chan struct{}, max(fr.packages(), fr.jobs))
	pkgSem := make(chan struct{}, fr.packages())
	var wg sync.WaitGroup
	var firstErr error
	for i, d := range dirs {
		if fr.flags.why != "" {
			fr.fallback[d] = fr.flags.why
			continue
		}
		wg.Add(1)
		pkgSem <- struct{}{}
		go func(i int, d string) {
			defer wg.Done()
			defer func() { <-pkgSem }()
			if why, err := fr.recordPackage(ctx, i, d, funcs[d]); err != nil || why != "" {
				fr.mu.Lock()
				if err != nil && firstErr == nil {
					firstErr = err
				}
				if why != "" {
					fr.fallback[d] = why
				}
				fr.mu.Unlock()
			}
		}(i, d)
	}
	wg.Wait()
	if firstErr != nil {
		return fr.res, firstErr
	}
	if len(fr.fallback) > 0 {
		if err := fr.recordWhole(ctx, dirs, funcs); err != nil {
			return fr.res, err
		}
	}
	fr.res.Duration = time.Since(start)
	return fr.res, nil
}

// goPackage is what go list says about the packages a test binary links.
type goPackage struct {
	dirs   map[string]string // import path -> absolute directory (main-module packages)
	linked map[string]bool   // repo-relative directories of those packages
}

// listPackages lists the main-module packages linked into the test binary of pkg.
func (fr *funcRecorder) listPackages(ctx context.Context, pkg string) (*goPackage, string) {
	rc := fr.rc
	const format = `{{if .Module}}{{if .Module.Main}}{{.ImportPath}}{{"\t"}}{{.Dir}}{{"\t"}}{{.Name}}{{end}}{{end}}`
	argv := append(append([]string{"go", "list", "-deps", "-test", "-f", format}, fr.flags.build...), pkg)
	var out, errb bytes.Buffer
	code, err := rc.ExecIn(ctx, rc.Dir(), argv, nil, &out, &errb)
	if err != nil || code != 0 {
		return nil, "go list failed: " + firstLine(errb.String())
	}
	gp := &goPackage{dirs: map[string]string{}, linked: map[string]bool{}}
	sc := bufio.NewScanner(&out)
	for sc.Scan() {
		f := strings.Split(sc.Text(), "\t")
		if len(f) != 3 || strings.Contains(f[0], " ") || (f[2] == "main" && strings.HasSuffix(f[0], ".test")) {
			continue
		}
		gp.dirs[f[0]] = f[1]
		if rel, ok := pathutil.Rel(rc.Repo.Root, f[1]); ok {
			gp.linked[rel] = true
		} else if filepath.Clean(f[1]) == rc.Repo.Root {
			gp.linked["."] = true
		}
	}
	if len(gp.dirs) == 0 {
		return nil, "go list found no packages to cover"
	}
	return gp, ""
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 200 {
		s = s[:200] + "…"
	}
	return s
}

// recordPackage builds the coverage test binary of package directory dir and runs each
// function in its own process. A non-empty result says why the package must be
// recorded as a whole instead (nothing was recorded for it then).
func (fr *funcRecorder) recordPackage(ctx context.Context, idx int, dir string, names []string) (string, error) {
	rc := fr.rc
	pkg := pkgArg(rc.Suite, dir)
	gp, why := fr.listPackages(ctx, pkg)
	if why != "" {
		return why, nil
	}
	ips := make([]string, 0, len(gp.dirs))
	for ip := range gp.dirs {
		ips = append(ips, ip)
	}
	sort.Strings(ips)
	work := filepath.Join(rc.TraceDir, fmt.Sprintf("gofunc-%d", idx))
	if err := os.MkdirAll(work, 0o755); err != nil {
		return "", err
	}
	bin := filepath.Join(work, "pkg.test")
	if runtime.GOOS == "windows" {
		bin += ".exe"
	}
	argv := append(append([]string{"go", "test"}, fr.flags.build...), "-c", "-cover", "-coverpkg="+strings.Join(ips, ","), "-o", bin, pkg)
	var out bytes.Buffer
	code, err := rc.ExecIn(ctx, rc.Dir(), argv, nil, &out, &out)
	if err != nil || code != 0 {
		return "building the test binary with coverage failed: " + firstLine(out.String()), nil
	}
	if _, err := os.Stat(bin); err != nil {
		return "go test -c wrote no test binary", nil
	}
	pkgDir := filepath.Join(rc.Repo.Root, filepath.FromSlash(dir))
	covDir := filepath.Join(work, "covdata")
	if err := os.MkdirAll(covDir, 0o755); err != nil {
		return "", err
	}
	out.Reset()
	listArgv := append(append(slices.Clone(fr.flags.exec), bin, "-test.list", ".*"), fr.flags.tail...)
	code, err = rc.ExecIn(ctx, pkgDir, listArgv, []string{"GOCOVERDIR=" + covDir}, &out, &out)
	if err != nil || code != 0 {
		return "listing the tests failed: " + firstLine(out.String()), nil
	}
	listed := map[string]bool{}
	for _, l := range strings.Split(out.String(), "\n") {
		listed[strings.TrimSpace(l)] = true
	}

	sem := make(chan struct{}, fr.jobs)
	var wg sync.WaitGroup
	var firstErr error
	var errMu sync.Mutex
	for j, name := range names {
		unit := FuncUnit(dir, name)
		if !listed[name] {
			// Not compiled in this configuration (build constraints) or not run (an
			// example without output): running it would run nothing.
			fr.add(unit, &Observation{Files: map[string]bool{}, Status: state.Pass})
			continue
		}
		wg.Add(1)
		sem <- struct{}{}
		fr.procs <- struct{}{}
		go func(j int, name string) {
			defer wg.Done()
			defer func() { <-fr.procs; <-sem }()
			o, err := fr.runFunc(ctx, gp, filepath.Join(work, fmt.Sprintf("f%d", j)), bin, dir, pkgDir, name)
			if err != nil {
				errMu.Lock()
				if firstErr == nil {
					firstErr = err
				}
				errMu.Unlock()
				return
			}
			fr.add(FuncUnit(dir, name), o)
		}(j, name)
	}
	wg.Wait()
	return "", firstErr
}

func (fr *funcRecorder) add(unit string, o *Observation) {
	fr.mu.Lock()
	defer fr.mu.Unlock()
	fr.res.Observations[unit] = o
	if o.Status == state.Fail {
		fr.res.ExitCode = max(fr.res.ExitCode, 1)
	}
}

// runFunc runs one test function of the coverage binary and turns what it did into an
// observation.
func (fr *funcRecorder) runFunc(ctx context.Context, gp *goPackage, work, bin, dir, pkgDir, name string) (*Observation, error) {
	rc := fr.rc
	unit := FuncUnit(dir, name)
	covDir := filepath.Join(work, "covdata")
	for _, d := range []string{covDir, trace.V8Dir(work)} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return nil, err
		}
	}
	profile, testLog := filepath.Join(work, "cover.out"), filepath.Join(work, "test.log")
	argv := append(slices.Clone(fr.flags.exec), bin, "-test.run=^"+regexp.QuoteMeta(name)+"$", "-test.coverprofile="+profile,
		"-test.testlogfile="+testLog, "-test.paniconexit0")
	argv = append(append(argv, fr.flags.test...), fr.flags.tail...)
	base := suiteEnv(rc.Suite)
	env := []string{"GOCOVERDIR=" + covDir, "SMATES_TRACE_PARENT=" + trace.GoParentPrefix + pkgDir}
	env = append(env, trace.NodeEnv(work, rc.Repo.Root, rc.Shims(), base)...)
	env = append(env, trace.PythonEnv(work, rc.Repo.Root, rc.Shims(), base)[2:3]...) // PYTHONPATH only
	var buf bytes.Buffer
	start := time.Now()
	code, err := rc.ExecIn(ctx, pkgDir, argv, env, &buf, &buf)
	elapsed := time.Since(start)
	fr.outMu.Lock()
	fmt.Fprintf(rc.Stdout, "── smates: %s (exit %d, %s)\n", unit, code, elapsed.Round(time.Millisecond))
	rc.Stdout.Write(buf.Bytes())
	fr.outMu.Unlock()
	if err != nil {
		return nil, err
	}
	o := &Observation{Files: map[string]bool{}, DurationMs: elapsed.Milliseconds()}
	setUnitExit(o, code)
	if code != 0 {
		msg := buf.Bytes()
		if len(msg) > trace.MaxMessage {
			msg = msg[len(msg)-trace.MaxMessage:]
		}
		o.Failure = string(msg)
	}
	files, envs := trace.ParseTestLog(testLog, pkgDir)
	addTestLog(o, rc.Repo.Root, files, envs)
	if procs, err := trace.Collect(work, rc.Repo.Root); err == nil {
		for f := range trace.GoChildren(procs)[pkgDir] {
			o.AddFile(f)
		}
	}
	certain := addCoverage(o, rc.Repo.Root, gp, profile)
	var known []string
	if rc.Known != nil {
		known = rc.Known(unit)
	}
	for _, k := range known {
		kind, f := depkey.Parse(k)
		if kind != depkey.KindDecls {
			continue
		}
		// Bodies coverage cannot vouch for: packages the binary does not link (built or
		// run by the test as a subprocess), or every package when the profile is missing.
		if !certain || !gp.linked[path.Dir(f)] {
			o.AddFile(f)
		}
	}
	return o, nil
}

// addCoverage adds the files in which a coverage profile counts an executed block. It
// returns false when the profile is missing or unreadable (nothing is known then); a
// block in a file it cannot place makes the whole package of that file a dependency.
func addCoverage(o *Observation, root string, gp *goPackage, profile string) bool {
	f, err := os.Open(profile)
	if err != nil {
		return false
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	seen := map[string]bool{}
	header := false
	for sc.Scan() {
		line := sc.Text()
		if !header {
			header = strings.HasPrefix(line, "mode:")
			if !header {
				return false
			}
			continue
		}
		// name.go:line.col,line.col numStmts count
		i := strings.LastIndexByte(line, ' ')
		if i < 0 || line[i+1:] == "0" {
			continue
		}
		file := line[:i]
		if j := strings.LastIndexByte(file, ':'); j >= 0 {
			file = file[:j]
		}
		if seen[file] {
			continue
		}
		seen[file] = true
		ip, base := path.Split(file)
		ip = strings.TrimSuffix(ip, "/")
		dir, ok := gp.dirs[ip]
		if !ok {
			continue // not a repository package
		}
		abs := filepath.Join(dir, base)
		rel, ok := pathutil.Rel(root, abs)
		if _, err := os.Stat(abs); err == nil && ok {
			if !pathutil.IsVendored(rel) {
				o.AddFile(rel)
			}
			continue
		}
		// A //line directive names another file: depend on all of the package's Go files.
		entries, _ := os.ReadDir(dir)
		for _, e := range entries {
			if n := e.Name(); strings.HasSuffix(n, ".go") && !strings.HasSuffix(n, "_test.go") {
				if rel, ok := pathutil.Rel(root, filepath.Join(dir, n)); ok {
					o.AddFile(rel)
				}
			}
		}
	}
	return header && sc.Err() == nil
}

// recordWhole records the packages that could not be recorded per function: go test
// runs them (with -run for the selected functions) traced as a package, and each
// function gets the package's observation, depending on the content of every Go file
// of its closure.
func (fr *funcRecorder) recordWhole(ctx context.Context, dirs []string, funcs map[string][]string) error {
	rc := fr.rc
	var whole []string
	var names []string
	for _, d := range dirs {
		if why, ok := fr.fallback[d]; ok {
			whole = append(whole, d)
			names = append(names, funcs[d]...)
			fr.res.Warnings = append(fr.res.Warnings, fmt.Sprintf("%s: recorded at package granularity (%s): each of its test functions depends on everything the package does", d, why))
		}
	}
	sub := *rc
	sub.TraceDir = filepath.Join(rc.TraceDir, "gofunc-whole")
	if err := os.MkdirAll(sub.TraceDir, 0o755); err != nil {
		return err
	}
	run := runRegex(names)
	if len(run) > maxRunRegex {
		run = ""
	}
	oc := &goOutcomes{}
	r, err := runPackages(ctx, &sub, whole, false, run, oc)
	if r != nil {
		fr.res.ExitCode = max(fr.res.ExitCode, r.ExitCode)
	}
	if err != nil {
		return err
	}
	for _, d := range whole {
		po := r.Observations[d]
		ip := dirImportPath(rc.Repo.Root, d)
		for _, n := range funcs[d] {
			unit := FuncUnit(d, n)
			st, inferred, ran := oc.status(ip, n)
			if po == nil && !ran {
				continue
			}
			o := &Observation{Files: map[string]bool{}, Status: st, Inferred: inferred, Failure: oc.failure(ip, n)}
			if po != nil {
				for f := range po.Files {
					o.AddFile(f)
				}
				o.DurationMs = po.DurationMs
				if !ran {
					o.Status, o.Inferred, o.Failure = po.Status, po.Status == state.Fail, po.Failure
				}
			}
			if rc.Known != nil {
				for _, k := range rc.Known(unit) {
					if f, ok := depkey.File(k); ok {
						o.AddFile(f)
					} else if depkey.KindOf(k) == depkey.KindConfig {
						o.AddFile(k) // the suite's settings, recorded as they are
					}
				}
			}
			fr.res.Observations[unit] = o
		}
	}
	return nil
}

// ---- go test output ----

// goTestLineRe matches the result line of a top-level test (subtests are indented).
var goTestLineRe = regexp.MustCompile(`^--- (PASS|FAIL|SKIP): (\S+) \(`)

// goOutcomes reads per-function outcomes from go test output: the result lines go test
// prints for failing tests (and, with -v, for every test), the per-package summary
// lines, or the events of go test -json.
type goOutcomes struct {
	mu      sync.Mutex
	buf     []byte
	pending map[string]string    // test -> status, for the package printing now
	failOut map[string][]byte    // test -> its output when it failed, for the package printing now
	pkgs    map[string]*goPkgRes // import path -> outcomes
	cur     string               // the test whose output is being printed (plain output)
	curOut  []byte
}

type goPkgRes struct {
	status string // state.Pass / state.Fail ("" = not seen)
	build  bool   // it did not build
	tests  map[string]string
	fails  map[string]string // test -> failure output
}

func (g *goOutcomes) pkg(ip string) *goPkgRes {
	if g.pkgs == nil {
		g.pkgs = map[string]*goPkgRes{}
	}
	p := g.pkgs[ip]
	if p == nil {
		p = &goPkgRes{tests: map[string]string{}, fails: map[string]string{}}
		g.pkgs[ip] = p
	}
	return p
}

func (g *goOutcomes) Write(p []byte) (int, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.buf = append(g.buf, p...)
	for {
		i := bytes.IndexByte(g.buf, '\n')
		if i < 0 {
			break
		}
		g.line(string(bytes.TrimRight(g.buf[:i], "\r")))
		g.buf = g.buf[i+1:]
	}
	return len(p), nil
}

type goTestEvent struct {
	Action      string
	Package     string
	Test        string
	Output      string
	FailedBuild string
}

func (g *goOutcomes) line(l string) {
	if strings.HasPrefix(l, "{") {
		var ev goTestEvent
		if json.Unmarshal([]byte(l), &ev) == nil && ev.Action != "" {
			g.event(ev)
			return
		}
	}
	if m := buildFailedRe.FindStringSubmatch(l); m != nil {
		p := g.pkg(m[1])
		p.status, p.build = state.Fail, true
		g.pending, g.failOut = nil, nil
		return
	}
	if m := goTestLineRe.FindStringSubmatch(l); m != nil {
		if g.pending == nil {
			g.pending = map[string]string{}
		}
		st := state.Pass
		if m[1] == "FAIL" {
			st = state.Fail
			if g.failOut == nil {
				g.failOut = map[string][]byte{}
			}
			g.cur, g.curOut = m[2], []byte(l+"\n")
		} else {
			g.cur = ""
		}
		g.pending[m[2]] = st
		return
	}
	if m := goSummaryRe.FindStringSubmatch(l); m != nil {
		p := g.pkg(m[2])
		g.flushCur()
		switch m[1] {
		case "FAIL":
			p.status = state.Fail
		default:
			p.status = state.Pass
		}
		for t, st := range g.pending {
			p.tests[t] = st
		}
		for t, out := range g.failOut {
			p.fails[t] = string(out)
		}
		g.pending, g.failOut, g.cur = nil, nil, ""
		return
	}
	if g.cur != "" {
		if strings.HasPrefix(l, "    ") || strings.HasPrefix(l, "\t") || strings.HasPrefix(l, "panic:") || strings.HasPrefix(l, "goroutine ") {
			if len(g.curOut) < trace.MaxMessage {
				g.curOut = append(g.curOut, l+"\n"...)
			}
			return
		}
		if l != "FAIL" {
			g.flushCur()
		}
	}
}

func (g *goOutcomes) flushCur() {
	if g.cur != "" && g.failOut != nil {
		g.failOut[g.cur] = g.curOut
	}
	g.cur, g.curOut = "", nil
}

func (g *goOutcomes) event(ev goTestEvent) {
	if ev.Package == "" {
		return
	}
	p := g.pkg(ev.Package)
	if ev.Test == "" {
		switch ev.Action {
		case "pass", "skip":
			p.status = state.Pass
		case "fail":
			p.status = state.Fail
			p.build = p.build || ev.FailedBuild != ""
		}
		return
	}
	if strings.Contains(ev.Test, "/") {
		return
	}
	switch ev.Action {
	case "output":
		if len(p.fails[ev.Test]) < trace.MaxMessage {
			p.fails[ev.Test] += ev.Output
		}
	case "pass", "skip":
		p.tests[ev.Test] = state.Pass
		delete(p.fails, ev.Test)
	case "fail":
		p.tests[ev.Test] = state.Fail
	}
}

// status returns the outcome of test name in package ip: reported for the test
// itself, or derived from the package (its passing summary covers every test; a failed
// build fails them all; otherwise it is unknown and inferred). ran is false when the
// package was not seen at all.
func (g *goOutcomes) status(ip, name string) (st string, inferred, ran bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	p := g.pkgs[ip]
	if p == nil || ip == "" {
		return "", false, false
	}
	if s := p.tests[name]; s != "" {
		return s, false, true
	}
	switch {
	case p.build:
		return state.Fail, false, true
	case p.status == state.Pass:
		return state.Pass, false, true
	case p.status == state.Fail:
		return state.Unknown, true, true
	}
	return "", false, false
}

// failure returns the output of a failed test (bounded).
func (g *goOutcomes) failure(ip, name string) string {
	g.mu.Lock()
	defer g.mu.Unlock()
	p := g.pkgs[ip]
	if p == nil || p.tests[name] != state.Fail {
		return ""
	}
	out := p.fails[name]
	if len(out) > trace.MaxMessage {
		out = out[len(out)-trace.MaxMessage:]
	}
	return out
}

var _ io.Writer = (*goOutcomes)(nil)

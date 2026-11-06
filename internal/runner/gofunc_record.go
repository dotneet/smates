package runner

// Recording for granularity: func: a coverage test binary per package, one process
// per test function, and attribution of coverage profiles and declarations.

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
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

	"github.com/dotneet/smates/internal/depkey"
	"github.com/dotneet/smates/internal/pathutil"
	"github.com/dotneet/smates/internal/state"
	"github.com/dotneet/smates/internal/static/golang"
	"github.com/dotneet/smates/internal/trace"
)

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
	names  map[string]string // absolute directory -> package name
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
	gp := &goPackage{dirs: map[string]string{}, linked: map[string]bool{}, names: map[string]string{}}
	sc := bufio.NewScanner(&out)
	for sc.Scan() {
		f := strings.Split(sc.Text(), "\t")
		if len(f) != 3 || strings.Contains(f[0], " ") || (f[2] == "main" && strings.HasSuffix(f[0], ".test")) {
			continue
		}
		gp.dirs[f[0]] = f[1]
		gp.names[f[1]] = f[2]
		if rel, ok := repoDir(rc.Repo.Root, f[1]); ok {
			gp.linked[rel] = true
		}
	}
	if len(gp.dirs) == 0 {
		return nil, "go list found no packages to cover"
	}
	return gp, ""
}

// lastLine is the last non-empty line of s, shortened.
func lastLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.LastIndexByte(s, '\n'); i >= 0 {
		s = s[i+1:]
	}
	return firstLine(s)
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
	pkgDir := filepath.Join(rc.Repo.Root, filepath.FromSlash(dir))
	build, err := childCoverFlags(fr.flags.build, Getenv(suiteEnv(rc.Suite), "GOFLAGS"), rc.Dir(), pkgDir, gp.names[pkgDir], work)
	if err != nil {
		return "", err
	}
	argv := append(append([]string{"go", "test"}, build...), "-c", "-cover", "-coverpkg="+strings.Join(ips, ","), "-o", bin, pkg)
	var out bytes.Buffer
	code, err := rc.ExecIn(ctx, rc.Dir(), argv, nil, &out, &out)
	if err != nil || code != 0 {
		return "building the test binary with coverage failed: " + firstLine(out.String()), nil
	}
	if _, err := os.Stat(bin); err != nil {
		return "go test -c wrote no test binary", nil
	}
	// Which functions go test runs, from the test files this configuration compiles:
	// listing them with the binary (-test.list) would run TestMain, which may do
	// suite-wide work or fail when no test ran.
	runs, known := fr.builtFuncs(ctx, pkg, dir)
	if !known {
		var why string
		if runs, why = fr.listFuncs(ctx, work, bin, pkgDir); why != "" {
			return why, nil
		}
	}

	sem := make(chan struct{}, fr.jobs)
	var wg sync.WaitGroup
	var firstErr error
	var errMu sync.Mutex
	for j, name := range names {
		unit := FuncUnit(dir, name)
		if run, built := runs[name]; !run {
			// Compiled but never run (an example without output, on every platform), or
			// not listed by a binary whose test files go list could not tell: a pass. A
			// function of a file this configuration does not compile is not a unit of the
			// suite (excludedTestFiles); one that still gets here (build flags passed only
			// to this run) ran nothing and is not recorded.
			if built || !known {
				fr.add(unit, &Observation{Files: map[string]bool{}, Status: state.Pass})
			}
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

// builtFuncs returns the test functions declared by the test files of package pkg
// (repo-relative directory dir) that go list says this configuration compiles, each
// with whether go test runs it (examples without output are only compiled); ok is
// false when it cannot tell (go list fails, a test file does not parse).
func (fr *funcRecorder) builtFuncs(ctx context.Context, pkg, dir string) (runs map[string]bool, ok bool) {
	rc := fr.rc
	argv := append(append([]string{"go", "list", "-f", `{{range .TestGoFiles}}{{.}}{{"\n"}}{{end}}{{range .XTestGoFiles}}{{.}}{{"\n"}}{{end}}`}, fr.flags.build...), pkg)
	var out, errb bytes.Buffer
	if code, err := rc.ExecIn(ctx, rc.Dir(), argv, nil, &out, &errb); err != nil || code != 0 {
		return nil, false
	}
	runs = map[string]bool{}
	for _, f := range strings.Split(out.String(), "\n") {
		if f = strings.TrimSpace(f); f == "" {
			continue
		}
		tf, err := readTestFile(rc.Repo.Root, path.Join(dir, f))
		if err != nil {
			return nil, false
		}
		for _, n := range tf.Tests {
			runs[n] = runs[n] || !slices.Contains(tf.Silent, n)
		}
	}
	return runs, true
}

// listFuncs lists the functions the test binary bin runs (-test.list), for when the
// test files cannot tell. A non-empty result says why the package must be recorded as
// a whole instead.
func (fr *funcRecorder) listFuncs(ctx context.Context, work, bin, pkgDir string) (map[string]bool, string) {
	covDir := filepath.Join(work, "covdata")
	if err := os.MkdirAll(covDir, 0o755); err != nil {
		return nil, err.Error()
	}
	var out bytes.Buffer
	argv := append(append(slices.Clone(fr.flags.exec), bin, "-test.list", ".*"), fr.flags.tail...)
	code, err := fr.rc.ExecIn(ctx, pkgDir, argv, []string{"GOCOVERDIR=" + covDir}, &out, &out)
	if err != nil || code != 0 {
		// The last line: the names come first, an error (of TestMain) last.
		return nil, fmt.Sprintf("listing the tests failed (exit %d): %s", code, lastLine(out.String()))
	}
	listed := map[string]bool{}
	for _, l := range strings.Split(out.String(), "\n") {
		if l = strings.TrimSpace(l); l != "" {
			listed[l] = true
		}
	}
	return listed, ""
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
	covDir, childDir := filepath.Join(work, "covdata"), filepath.Join(work, "children")
	for _, d := range []string{covDir, childDir, trace.V8Dir(work)} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return nil, err
		}
	}
	profile, testLog := filepath.Join(work, "cover.out"), filepath.Join(work, "test.log")
	argv := append(slices.Clone(fr.flags.exec), bin, "-test.run=^"+regexp.QuoteMeta(name)+"$", "-test.coverprofile="+profile,
		"-test.testlogfile="+testLog, "-test.paniconexit0")
	argv = append(append(argv, verboseFlags(fr.flags.test)...), fr.flags.tail...)
	env := []string{"GOCOVERDIR=" + covDir, childCoverEnv + "=" + covDir, trace.GoChildLogEnv + "=" + childDir, "SMATES_TRACE_PARENT=" + trace.GoParentPrefix + pkgDir}
	env = append(env, childTraceEnv(rc, work)...)
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
		o.Failure = string(lastMessage(buf.Bytes()))
	} else if o.Status == state.Pass {
		// A process that passed proves the function passed only if it ran and did not
		// skip itself.
		o.Status = funcOutcome(buf.Bytes(), name)
	}
	files, envs := trace.GoTestLogs(testLog, pkgDir, bin, childDir)
	addTestLog(o, rc.Repo.Root, files, envs)
	if procs, err := trace.Collect(work, rc.Repo.Root); err == nil {
		for f := range trace.GoChildren(procs)[pkgDir] {
			o.AddFile(f)
		}
	}
	// A re-execution of the binary that wrote no counters ran code no profile shows.
	certain := addCoverage(o, rc.Repo.Root, gp, profile, dir) && childrenCovered(covDir)
	// In its own package, the declarations the bodies it executed reach; every Go file
	// of it when coverage cannot tell which bodies ran.
	var ownFiles, executed []string
	if entries, err := os.ReadDir(pkgDir); err == nil {
		for _, e := range entries {
			if strings.HasSuffix(e.Name(), ".go") && !e.IsDir() {
				ownFiles = append(ownFiles, path.Join(dir, e.Name()))
			}
		}
	}
	for k := range o.Files {
		if kind, f := depkey.Parse(k); kind == depkey.KindDecl && path.Dir(f) == dir {
			executed = append(executed, k)
		}
	}
	for _, k := range funcDeps(rc.Repo.Root, dir, name, ownFiles, executed) {
		o.AddFile(k)
	}
	if !certain {
		for _, f := range ownFiles {
			if !strings.HasSuffix(f, "_test.go") {
				o.AddFile(f)
			}
		}
	}
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

// addCoverage adds what a coverage profile counts an executed block in: the function
// or method whose body holds it (depkey.Decl, with the declarations of its file), the
// declarations of its file for a block outside any function body (a function literal
// initializing a package-level variable, which they include; in the test's own package
// directory own, that declaration: funcDeps adds the rest), and the whole file when
// the block cannot be placed in it (it does not parse, //line directives). It returns
// false when the profile is missing or unreadable (nothing is known then); a block in
// a file it cannot find makes the whole package of that file a dependency.
func addCoverage(o *Observation, root string, gp *goPackage, profile, own string) bool {
	f, err := os.Open(profile)
	if err != nil {
		return false
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	type goFile struct {
		rel   string // "" when not a repository file
		spans []golang.Span
		whole bool // depends on the whole file (or package)
	}
	files := map[string]*goFile{}
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
		j := strings.LastIndexByte(file, ':')
		if j < 0 {
			continue
		}
		file, pos := file[:j], file[j+1:]
		gf := files[file]
		if gf == nil {
			gf = &goFile{}
			files[file] = gf
			ip, base := path.Split(file)
			ip = strings.TrimSuffix(ip, "/")
			dir, ok := gp.dirs[ip]
			if !ok {
				continue // not a repository package
			}
			abs := filepath.Join(dir, base)
			rel, ok := pathutil.Rel(root, abs)
			if _, err := os.Stat(abs); err == nil && ok {
				if pathutil.IsVendored(rel) {
					continue
				}
				gf.rel = rel
				gf.spans, ok = readSpans(abs)
				if gf.whole = !ok; gf.whole {
					o.AddFile(rel)
				}
			} else {
				// A //line directive names another file: depend on all of the package's Go files.
				gf.whole = true
				entries, _ := os.ReadDir(dir)
				for _, e := range entries {
					if n := e.Name(); strings.HasSuffix(n, ".go") && !strings.HasSuffix(n, "_test.go") {
						if rel, ok := pathutil.Rel(root, filepath.Join(dir, n)); ok {
							o.AddFile(rel)
						}
					}
				}
			}
		}
		if gf.rel == "" || gf.whole {
			continue
		}
		line0, col0, ok := blockStart(pos)
		if !ok {
			gf.whole = true
			o.AddFile(gf.rel)
			continue
		}
		var span *golang.Span
		for i := range gf.spans {
			if gf.spans[i].Contains(line0, col0) {
				span = &gf.spans[i]
				break
			}
		}
		switch {
		case span == nil:
			gf.whole = true
			o.AddFile(gf.rel)
		case path.Dir(gf.rel) == own:
			// The test's own package: what else it depends on there, funcDeps finds.
			o.AddFile(depkey.Decl(gf.rel, span.Key))
		case span.Func:
			o.AddFile(depkey.Decl(gf.rel, span.Key))
			o.AddFile(depkey.Decls(gf.rel))
		default:
			o.AddFile(depkey.Decls(gf.rel))
		}
	}
	return header && sc.Err() == nil
}

// blockStart parses the start of a coverage block, "line.col,line.col".
func blockStart(pos string) (line, col int, ok bool) {
	start, _, _ := strings.Cut(pos, ",")
	l, c, found := strings.Cut(start, ".")
	if !found {
		return 0, 0, false
	}
	line, err1 := strconv.Atoi(l)
	col, err2 := strconv.Atoi(c)
	return line, col, err1 == nil && err2 == nil
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
			o := &Observation{Files: map[string]bool{}, Status: st, Inferred: inferred, Failure: oc.failure(ip, n), BuildFailed: st == state.Fail && oc.buildFailed(ip)}
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

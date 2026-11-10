//go:build e2e

// Package e2e runs smates against real projects (fixtures/<name>) with real toolchains.
//
// Every fixture is copied to a temp git repository, its dependencies are installed, and a
// baseline is recorded with `smates run --all`. Then:
//
//   - TestScenarios applies each scenario in fixtures/<name>/scenarios.json (edits to the
//     working tree) and runs `smates run --shadow`, which runs every test while evaluating
//     the selection. A scenario passes when no failing test was left unselected
//     (no escapes) and the expected selected / not-selected tests match.
//     The Markdown summary of each scenario (`--markdown`) must name the selection, the
//     orphans and every selected test (set SMATES_E2E_MARKDOWN_DIR to keep them).
//   - TestMutations breaks every source and data file in turn (a top-level throw/raise,
//     a compile error, corrupted data) and checks that every test that fails because of
//     it was selected. It reports the selection rate for each mutation.
//
// Fixtures, scenarios and mutations run in parallel (bounded by go test -parallel). The
// prepared repositories of a fixture are shared by all its tests: one is prepared per
// test of the fixture running at the same time (at most SMATES_E2E_REPOS, which defaults
// to -parallel up to 4).
//
// Run with: go test -tags e2e ./e2e/ (needs node 20+, npm, pnpm, bun, go, uv, a JDK 17+
// and gradle on PATH;
// fixtures whose tools are missing are skipped). Set SMATES_E2E_FIXTURE to a fixture
// name, or a comma-separated list of them, to run only those; "-name" excludes one.
package e2e

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
)

var (
	smatesBin string
	// reposDir holds the shared prepared repositories (they outlive the test preparing them).
	reposDir string
)

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "smates-e2e-")
	if err != nil {
		panic(err)
	}
	if real, err := filepath.EvalSymlinks(dir); err == nil {
		dir = real
	}
	reposDir = filepath.Join(dir, "repos")
	smatesBin = filepath.Join(dir, "smates")
	if runtime.GOOS == "windows" {
		smatesBin += ".exe"
	}
	build := exec.Command("go", "build", "-o", smatesBin, "./cmd/smates")
	build.Dir = ".."
	build.Stdout, build.Stderr = os.Stdout, os.Stderr
	if err := build.Run(); err != nil {
		panic("building smates: " + err.Error())
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

// fixtureSpec is fixtures/<name>/fixture.json.
type fixtureSpec struct {
	// Requires lists commands that must be on PATH (the fixture is skipped otherwise).
	Requires []string `json:"requires"`
	// Setup commands run in the repository after copying (dependency installation).
	Setup [][]string `json:"setup"`
	// Env is added to every smates invocation.
	Env map[string]string `json:"env"`
	// MutationSkip lists repo-relative globs excluded from the mutation sweep.
	MutationSkip []string `json:"mutation_skip"`
	// MutationOnly, when set, limits the sweep to these globs.
	MutationOnly []string `json:"mutation_only"`
	// MutationNoRecord runs the sweep with --no-record: the sweep discards every state it
	// records, so tracing is wasted when the fixture's runners report per-file status
	// without it (jest, vitest, pytest, go). SMATES_E2E_MUTATION_RECORD=1 records anyway.
	MutationNoRecord bool `json:"mutation_no_record"`
	// MutationConfig names a config file in the fixture directory (not copied into the
	// repository) the sweep runs smates with instead of the repository's smates.yml.
	MutationConfig string `json:"mutation_config"`
}

// scenario is one entry of fixtures/<name>/scenarios.json.
type scenario struct {
	Name  string `json:"name"`
	Edits []edit `json:"edits"`
	// Selected / NotSelected list test IDs (repo-relative test files or Go package dirs).
	Selected    []string `json:"selected"`
	NotSelected []string `json:"not_selected"`
	// Orphans, when non-nil, is the exact set of expected unattributed files.
	Orphans []string `json:"orphans"`
	// Fails lists tests expected to fail after the edit (sanity check of the scenario).
	Fails []string `json:"fails"`
	// Agent runs the scenario with the agent enabled (skipped without an agent API key).
	Agent bool `json:"agent"`
	// Hints checks the failure hints of failing tests (test ID -> expectation). Every
	// test in Fails is also checked to have one of the edited files among its hint's
	// candidates.
	Hints map[string]hintSpec `json:"hints"`
	// Args are extra smates run flags.
	Args []string `json:"args"`
	// Note documents the scenario (ignored).
	Note string `json:"note"`
}

// hintSpec is the expected failure hint of one failing test.
type hintSpec struct {
	// Top lists the expected leading candidates, in order.
	Top []string `json:"top"`
	// Verdict is the expected verdict ("" = any).
	Verdict string `json:"verdict"`
	// Output is a substring expected in the failure output the runner reported.
	Output string `json:"output"`
}

type edit struct {
	File string `json:"file"`
	// Op: write (replace content), append, prepend, replace (Old -> New), delete, mkdir.
	Op      string `json:"op"`
	Content string `json:"content"`
	Old     string `json:"old"`
	New     string `json:"new"`
}

// report mirrors `smates run --report`.
type report struct {
	Mode    string                       `json:"mode"`
	Orphans []string                     `json:"orphans"`
	Tests   map[string][]string          `json:"selected_tests"`
	Results map[string]map[string]string `json:"results"`
	Escapes []string                     `json:"escapes"`
	// Possible are unselected tests marked failed only because the run failed.
	Possible []string     `json:"possible_escapes"`
	Exit     int          `json:"exit_code"`
	Total    int          `json:"total"`
	Sel      int          `json:"selected"`
	Hints    []reportHint `json:"failure_hints"`
}

// reportHint mirrors hints.Hint.
type reportHint struct {
	Suite      string `json:"suite"`
	Test       string `json:"test"`
	Verdict    string `json:"verdict"`
	Candidates []struct {
		Path string `json:"path"`
		Kind string `json:"kind"`
		Link string `json:"link"`
	} `json:"candidates"`
	Output string `json:"output"`
}

func (r *report) hint(test string) *reportHint {
	for i := range r.Hints {
		if r.Hints[i].Test == test {
			return &r.Hints[i]
		}
	}
	return nil
}

// editRank returns the 1-based position of the first candidate explained by one of the
// edits (the file itself, its directory listing, an existence check of it, or a tree
// containing it), 0 when none is.
func (h *reportHint) editRank(edits []edit) int {
	for i, c := range h.Candidates {
		for _, e := range edits {
			f := e.File
			switch {
			case c.Path == f, c.Path == f+"?", c.Path == path.Dir(f)+"/":
				return i + 1
			case strings.HasSuffix(c.Path, "/**") && strings.HasPrefix(f, strings.TrimSuffix(c.Path, "**")):
				return i + 1
			}
		}
	}
	return 0
}

func (r *report) selected() map[string]bool {
	out := map[string]bool{}
	for _, ids := range r.Tests {
		for _, id := range ids {
			out[id] = true
		}
	}
	return out
}

func (r *report) failing() []string {
	var out []string
	for _, units := range r.Results {
		for id, st := range units {
			if st == "fail" {
				out = append(out, id)
			}
		}
	}
	sort.Strings(out)
	return out
}

func fixtures(t *testing.T) []string {
	entries, err := os.ReadDir("fixtures")
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() && fixtureSelected(e.Name()) {
			out = append(out, e.Name())
		}
	}
	return out
}

// fixtureSelected reports whether SMATES_E2E_FIXTURE includes the fixture. It is a
// comma-separated list of fixtures to run; a "-" prefix excludes one instead (a list of
// exclusions alone runs every other fixture). Empty runs them all.
func fixtureSelected(name string) bool {
	only := os.Getenv("SMATES_E2E_FIXTURE")
	if only == "" {
		return true
	}
	included, excluded, onlyExclusions := false, false, true
	for _, f := range strings.Split(only, ",") {
		if x, ok := strings.CutPrefix(f, "-"); ok {
			excluded = excluded || x == name
		} else {
			onlyExclusions = false
			included = included || f == name
		}
	}
	return !excluded && (included || onlyExclusions)
}

// repo is a prepared fixture repository with a recorded baseline.
type repo struct {
	t        *testing.T
	name     string
	dir      string
	spec     fixtureSpec
	baseline []byte // state file content after the baseline run
}

func readSpec(t *testing.T, name string) fixtureSpec {
	t.Helper()
	var spec fixtureSpec
	if data, err := os.ReadFile(filepath.Join("fixtures", name, "fixture.json")); err == nil {
		if err := json.Unmarshal(data, &spec); err != nil {
			t.Fatalf("fixture.json: %v", err)
		}
	}
	return spec
}

// skipUnlessRunnable skips the test when a command the fixture requires is missing.
func skipUnlessRunnable(t *testing.T, spec fixtureSpec) {
	t.Helper()
	for _, r := range spec.Requires {
		if _, err := exec.LookPath(r); err != nil {
			t.Skipf("%s not on PATH", r)
		}
	}
}

// prepare prepares a repository of the fixture of its own for t, with extra env added to
// every command run in it.
func prepare(t *testing.T, name string, env map[string]string) *repo {
	t.Helper()
	spec := readSpec(t, name)
	skipUnlessRunnable(t, spec)
	for k, v := range env {
		if spec.Env == nil {
			spec.Env = map[string]string{}
		}
		spec.Env[k] = v
	}
	dir := t.TempDir()
	if real, err := filepath.EvalSymlinks(dir); err == nil {
		dir = real
	}
	return prepareIn(t, dir, name, spec)
}

// prepareIn copies the fixture to dir, installs its dependencies and records a baseline.
func prepareIn(t *testing.T, dir, name string, spec fixtureSpec) *repo {
	t.Helper()
	if err := copyTree(filepath.Join("fixtures", name), dir, spec.MutationConfig); err != nil {
		t.Fatal(err)
	}
	r := &repo{t: t, name: name, dir: dir, spec: spec}
	for _, c := range spec.Setup {
		r.must(c[0], c[1:]...)
	}
	r.must("git", "init", "-q")
	r.must("git", "add", "-A")
	r.must("git", "-c", "user.email=e2e@example.com", "-c", "user.name=e2e", "commit", "-qm", "fixture")
	out, code := r.smates("run", "--all", "--no-agent", "--report", r.reportPath())
	rep := r.readReport()
	if code != 0 || rep.Exit != 0 {
		t.Fatalf("baseline run failed (exit %d):\n%s", code, out)
	}
	if rep.Total == 0 {
		t.Fatalf("baseline found no tests:\n%s", out)
	}
	state, err := os.ReadFile(filepath.Join(dir, ".smates", "state.json.gz"))
	if err != nil {
		t.Fatalf("no state after baseline: %v\n%s", err, out)
	}
	r.baseline = state
	return r
}

// pool hands out the prepared repositories of one fixture, each to one test at a time.
// Preparing is the costly part (dependency installation and a full baseline run), so the
// repositories are shared by every test of the fixture, and a new one is prepared only
// when all are in use (up to maxRepos).
type pool struct {
	name string
	cond *sync.Cond
	free []*repo
	n    int    // repositories prepared or being prepared
	err  string // why the first repository could not be prepared
}

var (
	poolsMu sync.Mutex
	pools   = map[string]*pool{}
)

func fixturePool(name string) *pool {
	poolsMu.Lock()
	defer poolsMu.Unlock()
	p := pools[name]
	if p == nil {
		p = &pool{name: name, cond: sync.NewCond(&sync.Mutex{})}
		pools[name] = p
	}
	return p
}

// maxRepos is the most repositories prepared per fixture: SMATES_E2E_REPOS, by default
// go test -parallel up to 4. Each one costs a dependency installation and a full run,
// which is more than the parallelism beyond that gains.
func maxRepos() int {
	if n, err := strconv.Atoi(os.Getenv("SMATES_E2E_REPOS")); err == nil && n > 0 {
		return n
	}
	n := runtime.GOMAXPROCS(0)
	if f := flag.Lookup("test.parallel"); f != nil {
		if p, err := strconv.Atoi(f.Value.String()); err == nil && p > 0 {
			n = p
		}
	}
	return min(n, 4)
}

// acquire returns a repository for t's exclusive use (in an unknown state: call reset),
// preparing one if none is free. Release it with release.
func (p *pool) acquire(t *testing.T) *repo {
	t.Helper()
	spec := readSpec(t, p.name)
	skipUnlessRunnable(t, spec)
	p.cond.L.Lock()
	for len(p.free) == 0 && p.n >= maxRepos() && p.err == "" {
		p.cond.Wait()
	}
	if p.err != "" {
		p.cond.L.Unlock()
		t.Fatalf("fixture could not be prepared: %s", p.err)
	}
	if n := len(p.free); n > 0 {
		r := p.free[n-1]
		p.free = p.free[:n-1]
		p.cond.L.Unlock()
		r.t = t
		return r
	}
	p.n++
	first := p.n == 1
	p.cond.L.Unlock()
	ok := false
	defer func() {
		if ok {
			return
		}
		// prepareIn failed (t.Fatal): free the slot for another attempt, unless the
		// fixture itself is broken.
		p.cond.L.Lock()
		p.n--
		if first {
			p.err = "see the test that prepared it first (" + t.Name() + ")"
		}
		p.cond.L.Unlock()
		p.cond.Broadcast()
	}()
	if err := os.MkdirAll(reposDir, 0o755); err != nil {
		t.Fatal(err)
	}
	dir, err := os.MkdirTemp(reposDir, p.name+"-")
	if err != nil {
		t.Fatal(err)
	}
	r := prepareIn(t, dir, p.name, spec)
	ok = true
	return r
}

func (p *pool) release(r *repo) {
	p.cond.L.Lock()
	p.free = append(p.free, r)
	p.cond.L.Unlock()
	p.cond.Signal()
}

// warm makes sure the fixture can be prepared, failing or skipping t otherwise, so that a
// broken fixture is reported once instead of by each of its tests.
func (p *pool) warm(t *testing.T) {
	t.Helper()
	p.release(p.acquire(t))
}

func (r *repo) reportPath() string { return filepath.Join(r.dir, ".smates", "e2e-report.json") }

func (r *repo) readReport() *report {
	data, err := os.ReadFile(r.reportPath())
	if err != nil {
		r.t.Fatalf("reading report (smates failed before writing it?): %v", err)
	}
	var rep report
	if err := json.Unmarshal(data, &rep); err != nil {
		r.t.Fatalf("report: %v", err)
	}
	return &rep
}

func (r *repo) must(name string, args ...string) string {
	r.t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Dir = r.dir
	cmd.Env = r.env()
	out, err := cmd.CombinedOutput()
	if err != nil {
		r.t.Fatalf("%s %s: %v\n%s", name, strings.Join(args, " "), err, out)
	}
	return string(out)
}

func (r *repo) env() []string {
	env := os.Environ()
	for k, v := range r.spec.Env {
		env = append(env, k+"="+v)
	}
	// Keep smates' own CI detection out of the way: runs are explicit. Scenario edits
	// can keep a file's size within the same second, which a cached .pyc would miss.
	return append(env, "CI=", "GITHUB_ACTIONS=", "GITHUB_OUTPUT=", "GITHUB_STEP_SUMMARY=", "PYTHONDONTWRITEBYTECODE=1")
}

func (r *repo) smates(args ...string) (string, int) {
	// A stale report from an earlier run must never be mistaken for this run's.
	os.Remove(r.reportPath())
	cmd := exec.Command(smatesBin, args...)
	cmd.Dir = r.dir
	cmd.Env = r.env()
	var buf bytes.Buffer
	cmd.Stdout, cmd.Stderr = &buf, &buf
	err := cmd.Run()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		r.t.Fatalf("smates %v: %v", args, err)
	}
	return buf.String(), code
}

// reset restores the committed tree and the baseline state.
func (r *repo) reset() {
	r.t.Helper()
	r.must("git", "checkout", "-q", "--", ".")
	r.must("git", "clean", "-fdq") // keeps ignored files (node_modules, .venv, .smates)
	// Runner caches outside smates' view (vitest's fsModuleCache) would let scenarios
	// depend on each other.
	_ = filepath.WalkDir(r.dir, func(p string, d fs.DirEntry, err error) error {
		if err == nil && d.IsDir() && d.Name() == ".vitest-cache" {
			os.RemoveAll(p)
			return filepath.SkipDir
		}
		if err == nil && d.IsDir() && (d.Name() == ".git" || d.Name() == ".venv") {
			return filepath.SkipDir
		}
		return nil
	})
	if err := os.WriteFile(filepath.Join(r.dir, ".smates", "state.json.gz"), r.baseline, 0o644); err != nil {
		r.t.Fatal(err)
	}
}

func (r *repo) apply(e edit) {
	r.t.Helper()
	p := filepath.Join(r.dir, filepath.FromSlash(e.File))
	read := func() string {
		b, err := os.ReadFile(p)
		if err != nil {
			r.t.Fatalf("edit %s: %v", e.File, err)
		}
		return string(b)
	}
	write := func(s string) {
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			r.t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(s), 0o644); err != nil {
			r.t.Fatal(err)
		}
	}
	switch e.Op {
	case "write":
		write(e.Content)
	case "append":
		write(read() + e.Content)
	case "prepend":
		write(e.Content + read())
	case "replace":
		s := read()
		if !strings.Contains(s, e.Old) {
			r.t.Fatalf("edit %s: %q not found", e.File, e.Old)
		}
		write(strings.Replace(s, e.Old, e.New, 1))
	case "delete":
		if err := os.RemoveAll(p); err != nil {
			r.t.Fatal(err)
		}
	case "mkdir":
		if err := os.MkdirAll(p, 0o755); err != nil {
			r.t.Fatal(err)
		}
	default:
		r.t.Fatalf("unknown edit op %q", e.Op)
	}
}

func TestScenarios(t *testing.T) {
	t.Parallel()
	for _, name := range fixtures(t) {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			data, err := os.ReadFile(filepath.Join("fixtures", name, "scenarios.json"))
			if err != nil {
				t.Skip("no scenarios.json")
			}
			var scenarios []scenario
			if err := json.Unmarshal(data, &scenarios); err != nil {
				t.Fatalf("scenarios.json: %v", err)
			}
			p := fixturePool(name)
			p.warm(t)
			for _, sc := range scenarios {
				t.Run(sc.Name, func(t *testing.T) {
					t.Parallel()
					if sc.Agent && os.Getenv("ANTHROPIC_API_KEY") == "" && os.Getenv("OPENAI_API_KEY") == "" && os.Getenv("OPENROUTER_API_KEY") == "" {
						t.Skip("needs ANTHROPIC_API_KEY, OPENAI_API_KEY or OPENROUTER_API_KEY")
					}
					r := p.acquire(t)
					defer p.release(r)
					r.reset()
					for _, e := range sc.Edits {
						r.apply(e)
					}
					md := filepath.Join(r.dir, ".smates", "e2e-summary.md")
					os.Remove(md)
					args := []string{"run", "--shadow", "--report", r.reportPath(), "--markdown", md}
					if !sc.Agent {
						args = append(args, "--no-agent")
					}
					args = append(args, sc.Args...)
					out, _ := r.smates(args...)
					rep := r.readReport()
					sel := rep.selected()
					var problems []string
					if len(rep.Escapes) > 0 {
						problems = append(problems, fmt.Sprintf("ESCAPES (failing but not selected): %v", rep.Escapes))
					}
					for _, id := range sc.Selected {
						if !sel[id] {
							problems = append(problems, "expected selected: "+id)
						}
					}
					for _, id := range sc.NotSelected {
						if sel[id] {
							problems = append(problems, "expected NOT selected: "+id)
						}
					}
					if sc.Orphans != nil && !sameSet(sc.Orphans, rep.Orphans) {
						problems = append(problems, fmt.Sprintf("orphans: got %v, want %v", rep.Orphans, sc.Orphans))
					}
					failing := rep.failing()
					for _, id := range sc.Fails {
						if !contains(failing, id) {
							problems = append(problems, "expected to fail (scenario sanity): "+id)
						}
					}
					problems = append(problems, checkMarkdown(md, rep)...)
					if dir := os.Getenv("SMATES_E2E_MARKDOWN_DIR"); dir != "" {
						data, _ := os.ReadFile(md)
						os.WriteFile(filepath.Join(dir, name+"--"+strings.NewReplacer("/", "_", " ", "_").Replace(sc.Name)+".md"), data, 0o644)
					}
					problems = append(problems, checkHints(t, sc, rep, failing)...)
					t.Logf("selected %d/%d, failing %v, orphans %v", rep.Sel, rep.Total, failing, rep.Orphans)
					if len(problems) > 0 {
						t.Errorf("%s\n--- smates output ---\n%s", strings.Join(problems, "\n"), tail(out, 80))
					}
				})
			}
		})
	}
}

// checkMarkdown checks the run's Markdown summary against its JSON report.
func checkMarkdown(path string, rep *report) []string {
	data, err := os.ReadFile(path)
	if err != nil {
		return []string{"no markdown summary: " + err.Error()}
	}
	md := string(data)
	var problems []string
	head := fmt.Sprintf("### smates (shadow mode): %d of %d test units selected\n", rep.Sel, rep.Total)
	if !strings.HasPrefix(md, head) {
		problems = append(problems, fmt.Sprintf("markdown starts with %q, want %q", strings.SplitN(md, "\n", 2)[0], head))
	}
	for _, o := range rep.Orphans {
		if !strings.Contains(md, "- `"+o+"` — ") {
			problems = append(problems, "markdown does not list orphan "+o)
		}
	}
	// Selected tests are named, unless a file selects a whole suite (shown as the suite).
	if !strings.Contains(md, " test units of `") && !strings.Contains(md, "all of `") && rep.Sel <= 10 {
		for id := range rep.selected() {
			if !strings.Contains(md, "`"+id+"`") {
				problems = append(problems, "markdown does not name selected test "+id)
			}
		}
	}
	if strings.Contains(md, "would **not** have been selected") != (len(rep.Escapes) > 0) {
		problems = append(problems, "markdown and report disagree on escapes")
	}
	return problems
}

// checkHints verifies the failure hints: every expected failure points at an edited
// file, and the scenario's explicit expectations hold.
func checkHints(t *testing.T, sc scenario, rep *report, failing []string) []string {
	var problems, ranks []string
	for _, id := range sc.Fails {
		if !contains(failing, id) {
			continue
		}
		h := rep.hint(id)
		if h == nil {
			problems = append(problems, "no failure hint for "+id)
			continue
		}
		rank := h.editRank(sc.Edits)
		ranks = append(ranks, fmt.Sprintf("%s: edit at %d/%d (%s)", id, rank, len(h.Candidates), h.Verdict))
		if rank == 0 {
			problems = append(problems, fmt.Sprintf("hint for %s does not name an edited file: %s %+v", id, h.Verdict, h.Candidates))
		}
	}
	for id, want := range sc.Hints {
		h := rep.hint(id)
		if h == nil {
			problems = append(problems, "no failure hint for "+id)
			continue
		}
		if want.Verdict != "" && h.Verdict != want.Verdict {
			problems = append(problems, fmt.Sprintf("hint verdict for %s: got %s, want %s", id, h.Verdict, want.Verdict))
		}
		for i, p := range want.Top {
			if i >= len(h.Candidates) || h.Candidates[i].Path != p {
				problems = append(problems, fmt.Sprintf("hint candidates for %s: got %+v, want %v first", id, h.Candidates, want.Top))
				break
			}
		}
		if want.Output != "" && !strings.Contains(h.Output, want.Output) {
			problems = append(problems, fmt.Sprintf("hint output for %s lacks %q: %q", id, want.Output, h.Output))
		}
	}
	if len(ranks) > 0 {
		t.Logf("hints: %s", strings.Join(ranks, "; "))
	}
	return problems
}

func TestMutations(t *testing.T) {
	t.Parallel()
	for _, name := range fixtures(t) {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			p := fixturePool(name)
			r := p.acquire(t)
			files := strings.Fields(r.must("git", "ls-files"))
			p.release(r)
			var mu sync.Mutex
			var stats []string
			escapes, totalSel, totalFail := 0, 0, 0
			// The subtests run in parallel, after this function returns.
			t.Cleanup(func() {
				sort.Strings(stats)
				t.Logf("mutation sweep for %s (%d escapes; %d selections for %d failing tests in total):\n%s", name, escapes, totalSel, totalFail, strings.Join(stats, "\n"))
			})
			for _, f := range files {
				m, ok := mutation(f)
				if !ok || r.skipMutation(f) {
					continue
				}
				t.Run(strings.ReplaceAll(f, "/", "|"), func(t *testing.T) {
					t.Parallel()
					r := p.acquire(t)
					defer p.release(r)
					r.reset()
					p := filepath.Join(r.dir, filepath.FromSlash(f))
					orig, err := os.ReadFile(p)
					if err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(p, m(orig), 0o644); err != nil {
						t.Fatal(err)
					}
					args := []string{"run", "--shadow", "--no-agent", "--report", r.reportPath()}
					noRecord := r.spec.MutationNoRecord && os.Getenv("SMATES_E2E_MUTATION_RECORD") == ""
					if noRecord {
						args = append(args, "--no-record")
					}
					if r.spec.MutationConfig != "" {
						cfg, err := filepath.Abs(filepath.Join("fixtures", name, r.spec.MutationConfig))
						if err != nil {
							t.Fatal(err)
						}
						args = append(args, "--config", cfg)
					}
					out, _ := r.smates(args...)
					rep := r.readReport()
					failing := rep.failing()
					if noRecord && len(rep.Possible) > 0 {
						// Escapes could not be told apart: the runner reported no per-file
						// status for unselected tests of a failing run.
						t.Errorf("mutating %s: without recording, unselected tests have no reported status: %v\n%s", f, rep.Possible, tail(out, 60))
					}
					mu.Lock()
					stats = append(stats, fmt.Sprintf("%-50s selected %3d/%-3d failing %3d  orphans %d", f, rep.Sel, rep.Total, len(failing), len(rep.Orphans)))
					totalSel += rep.Sel
					totalFail += len(failing)
					if len(rep.Escapes) > 0 {
						escapes++
					}
					mu.Unlock()
					if len(rep.Escapes) > 0 {
						t.Errorf("mutating %s: failing tests not selected: %v\n%s", f, rep.Escapes, tail(out, 60))
					}
				})
			}
		})
	}
}

func (r *repo) skipMutation(f string) bool {
	if len(r.spec.MutationOnly) > 0 && !matchAny(r.spec.MutationOnly, f) {
		return true
	}
	return matchAny(r.spec.MutationSkip, f)
}

// mutation returns a function that breaks a file so that anything depending on it fails.
func mutation(f string) (func([]byte) []byte, bool) {
	base := filepath.Base(f)
	if strings.HasPrefix(base, ".") || base == "fixture.json" || base == "scenarios.json" {
		return nil, false
	}
	switch filepath.Ext(f) {
	case ".js", ".mjs", ".cjs", ".ts", ".mts", ".cts", ".tsx", ".jsx":
		if isTestName(base) || strings.Contains(base, ".config.") {
			return nil, false
		}
		return func(b []byte) []byte {
			return append(b, []byte("\n;throw new Error('SMATES_MUTATION');\n")...)
		}, true
	case ".py":
		if strings.HasPrefix(base, "test_") || strings.HasSuffix(base, "_test.py") || base == "conftest.py" {
			return nil, false
		}
		return func(b []byte) []byte { return append(b, []byte("\nraise RuntimeError('SMATES_MUTATION')\n")...) }, true
	case ".go":
		if strings.HasSuffix(base, "_test.go") {
			return nil, false
		}
		return func(b []byte) []byte { return append(b, []byte("\nvar _ = smatesMutationUndefined\n")...) }, true
	case ".java":
		if strings.Contains(f, "/src/test/") && javaTestName(base) {
			return nil, false
		}
		// A static initializer that throws, in the file's last top-level class: loading
		// the class fails, compiling still works (a compile error would fail every test
		// of the build). The fixtures mutate no interface or enum, where it would not
		// compile.
		return func(b []byte) []byte {
			i := bytes.LastIndexByte(b, '}')
			if i < 0 {
				return b
			}
			return slices.Concat(b[:i], []byte("    static { if (true) throw new RuntimeException(\"SMATES_MUTATION\"); }\n"), b[i:])
		}, true
	case ".sh":
		return func(b []byte) []byte { return append(b, []byte("\nexit 3\n")...) }, true
	case ".md":
		if base == "README.md" || strings.HasPrefix(f, "docs/") || strings.Contains(f, "/docs/") || base == "CHANGELOG.md" {
			return nil, false
		}
		return func(b []byte) []byte { return []byte("SMATES_MUTATION\n") }, true
	case ".json", ".yaml", ".yml", ".txt", ".csv", ".sql", ".toml", ".html", ".tmpl", ".env", ".properties":
		switch base {
		case "gradle.properties", "gradle-wrapper.properties",
			"package.json", "package-lock.json", "tsconfig.json", "pyproject.toml", "uv.lock", "go.mod", "go.sum", "pnpm-lock.yaml", "pnpm-workspace.yaml", "bun.lock":
			return nil, false
		}
		return func(b []byte) []byte { return []byte("SMATES_MUTATION{{{\n") }, true
	}
	return nil, false
}

// javaTestName reports whether a Java file is named like a test class.
func javaTestName(base string) bool {
	name := strings.TrimSuffix(base, ".java")
	for _, s := range []string{"Test", "Tests", "TestCase", "IT"} {
		if strings.HasSuffix(name, s) {
			return true
		}
	}
	return false
}

func isTestName(base string) bool {
	for _, s := range []string{".test.", ".spec.", "_test.", "_spec."} {
		if strings.Contains(base, s) {
			return true
		}
	}
	return false
}

func matchAny(globs []string, f string) bool {
	for _, g := range globs {
		if ok, _ := filepath.Match(g, f); ok {
			return true
		}
		if strings.HasSuffix(g, "/**") && strings.HasPrefix(f, strings.TrimSuffix(g, "**")) {
			return true
		}
	}
	return false
}

func sameSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	m := map[string]bool{}
	for _, x := range a {
		m[x] = true
	}
	for _, x := range b {
		if !m[x] {
			return false
		}
	}
	return true
}

func contains(s []string, x string) bool {
	for _, v := range s {
		if v == x {
			return true
		}
	}
	return false
}

func tail(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = append([]string{"..."}, lines[len(lines)-n:]...)
	}
	return strings.Join(lines, "\n")
}

// copyTree copies a fixture to dst, without the harness's own files (fixture.json,
// scenarios.json and the extra ones named).
func copyTree(src, dst string, extra ...string) error {
	skip := map[string]bool{"fixture.json": true, "scenarios.json": true}
	for _, f := range extra {
		if f != "" {
			skip[path.Clean(filepath.ToSlash(f))] = true
		}
	}
	return filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		if d.IsDir() && (d.Name() == "node_modules" || d.Name() == ".venv" || d.Name() == ".smates") {
			return filepath.SkipDir
		}
		if skip[filepath.ToSlash(rel)] {
			return nil
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		return os.WriteFile(target, data, info.Mode().Perm())
	})
}

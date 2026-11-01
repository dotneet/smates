package cli

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/dotneet/smates/internal/config"
	"github.com/dotneet/smates/internal/depkey"
	"github.com/dotneet/smates/internal/detect"
	"github.com/dotneet/smates/internal/gitx"
	"github.com/dotneet/smates/internal/pathutil"
	"github.com/dotneet/smates/internal/runner"
	"github.com/dotneet/smates/internal/selector"
	"github.com/dotneet/smates/internal/state"
	"gopkg.in/yaml.v3"
)

// ---- init ----

func cmdInit(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("init", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dir := fs.String("C", "", "run as if started in `dir`")
	force := fs.Bool("force", false, "overwrite an existing smates.yml")
	print := fs.Bool("stdout", false, "print the config instead of writing it")
	github := fs.Bool("github", false, "also write .github/workflows/smates.yml")
	patch := fs.String("patch", "", "with --github: write the patch that switches existing workflows' test steps to smates to `file` (default: print it)")
	format := formatFlag(fs)
	if err := fs.Parse(args); err != nil {
		return usageError{err}
	}
	if err := checkFormat(*format); err != nil {
		return err
	}
	asJSON := *format == "json"
	d := *dir
	if d == "" {
		d = "."
	}
	repo, err := gitx.Open(d)
	if err != nil {
		return err
	}
	snap, err := repo.Snapshot()
	if err != nil {
		return err
	}
	var files []string
	for f := range snap {
		files = append(files, f)
	}
	sort.Strings(files)
	// An existing config is kept unless --force: with --github only the workflow part
	// runs, on the config's suites.
	var existing *config.Config
	if !*print && !*force {
		if existing, err = config.Load(repo.Root); err != nil {
			return err
		}
		if existing != nil && !*github {
			return fmt.Errorf("%s already exists (use --force to overwrite, or --github to only generate the workflow from it)", existing.Path)
		}
	}
	if existing != nil {
		return initWorkflowOnly(repo, existing, files, *patch, asJSON, stdout, stderr)
	}
	suites := detect.Suites(repo, files)
	mk := makefileHints(repo.Root, suites)
	for _, h := range mk {
		fmt.Fprintf(stderr, "smates: %s target %q runs %q; smates runs the %s suite's runner itself, so those flags/variables are not applied. See the commented command:/env: hint for the suite in smates.yml.\n", h.File, h.Target, h.Line, h.Suite)
	}
	doc := renderConfig(suites, configHints(mk))
	rep := &initReport{Config: "smates.yml", FilesWritten: []string{}, Suites: []initSuite{}, NeedsReview: []reviewItem{}, NextSteps: initNextSteps}
	for _, s := range suites {
		rep.Suites = append(rep.Suites, initSuite{Name: s.Name, Runner: s.Runner, Root: s.Root, Exclude: nonNil(s.Exclude)})
	}
	if *print {
		if !asJSON {
			_, err := io.WriteString(stdout, doc)
			return err
		}
		rep.ConfigYAML = doc
		rep.NeedsReview = initReview(repo, nil, suites, files, mk, *github)
		return writeJSON(stdout, rep)
	}
	path := filepath.Join(repo.Root, "smates.yml")
	if err := os.WriteFile(path, []byte(doc), 0o644); err != nil {
		return err
	}
	rep.Written = true
	rep.FilesWritten = append(rep.FilesWritten, "smates.yml")
	if changed, err := ensureGitignore(repo.Root); err != nil {
		fmt.Fprintf(stderr, "smates: could not update .gitignore: %v\n", err)
	} else if changed {
		rep.FilesWritten = append(rep.FilesWritten, ".gitignore")
	}
	out := stdout
	if asJSON {
		out = io.Discard
	}
	fmt.Fprintf(out, "wrote smates.yml with %d suite(s):\n", len(suites))
	for _, s := range suites {
		fmt.Fprintf(out, "  - %s (%s) in %s\n", s.Name, s.Runner, s.Root)
	}
	if len(suites) == 0 {
		fmt.Fprintln(out, "  (no test suites detected: add them to smates.yml by hand)")
	}
	if *github {
		wf, err := initGitHub(repo, suites, files, *force, *patch, out, stderr)
		if err != nil {
			return err
		}
		rep.Workflow = wf
		if wf.Written {
			rep.FilesWritten = append(rep.FilesWritten, wf.File)
		}
		if wf.PatchFile != "" {
			rep.FilesWritten = append(rep.FilesWritten, wf.PatchFile)
		}
	}
	if asJSON {
		rep.NeedsReview = initReview(repo, nil, suites, files, mk, *github)
		return writeJSON(stdout, rep)
	}
	fmt.Fprint(stdout, initNextStepsText)
	return nil
}

const initNextStepsText = `
Next steps:
  smates run --all     # run everything once and record what each test uses
  smates run           # afterwards: run only what your changes can affect
  smates why <file>    # see which tests depend on a file
  smates hook install  # optional: run the affected tests before every git push
`

// initWorkflowOnly is `smates init --github` over an existing config (without
// --force): the config is left untouched and the workflow (or the patch for existing
// workflows) is generated from its suites.
func initWorkflowOnly(repo *gitx.Repo, cfg *config.Config, files []string, patch string, asJSON bool, stdout, stderr io.Writer) error {
	rep := &initReport{Config: cfg.Path, FilesWritten: []string{}, Suites: []initSuite{}, NeedsReview: []reviewItem{}, NextSteps: initNextSteps}
	for _, s := range cfg.Suites {
		rep.Suites = append(rep.Suites, initSuite{Name: s.Name, Runner: s.Runner, Root: s.Root, Exclude: nonNil(s.Exclude)})
	}
	out := stdout
	if asJSON {
		out = io.Discard
	}
	fmt.Fprintf(out, "%s exists: left unchanged; generating the workflow from its %d suite(s) (use --force to rewrite it)\n", cfg.Path, len(cfg.Suites))
	wf, err := initGitHub(repo, cfg.Suites, files, false, patch, out, stderr)
	if err != nil {
		return err
	}
	rep.Workflow = wf
	if wf.Written {
		rep.FilesWritten = append(rep.FilesWritten, wf.File)
	}
	if wf.PatchFile != "" {
		rep.FilesWritten = append(rep.FilesWritten, wf.PatchFile)
	}
	if asJSON {
		rep.NeedsReview = initReview(repo, cfg, cfg.Suites, files, nil, true)
		return writeJSON(stdout, rep)
	}
	return nil
}

// renderConfig renders smates.yml; hints are commented lines written under a suite
// (by suite name).
func renderConfig(suites []config.Suite, hints map[string][]string) string {
	var b strings.Builder
	b.WriteString(`# smates configuration. Docs: https://github.com/dotneet/smates
version: 1

# Test suites: one runner invocation each. Paths are relative to the repository root.
# Optional per suite: tests/exclude (globs relative to root), command, inputs, rules,
# depends_on/serves (code of a server the tests talk to: e.g. depends_on: [api]), env,
# isolate (jest/bun: one process per file while recording), junit.
suites:
`)
	if len(suites) == 0 {
		b.WriteString("  []\n")
	}
	for _, s := range suites {
		fmt.Fprintf(&b, "  - name: %s\n    runner: %s\n    root: %s\n", yamlStr(s.Name), s.Runner, yamlStr(s.Root))
		if len(s.Exclude) > 0 {
			b.WriteString("    exclude:\n")
			for _, e := range s.Exclude {
				fmt.Fprintf(&b, "      - %s\n", yamlStr(e))
			}
		}
		for _, l := range hints[s.Name] {
			b.WriteString("    " + l + "\n")
		}
	}
	b.WriteString(`
# Changed files no test is known to depend on ("orphans"): agent | all | ignore.
# "agent" asks an LLM (needs ANTHROPIC_API_KEY, OPENAI_API_KEY or OPENROUTER_API_KEY)
# and falls back to "all".
orphans: agent

# Changes that never require tests unless a test reads them (added to built-in defaults
# for docs, editor and lint configuration).
ignore: []

# Files every test depends on, e.g. CI toolchain pins.
inputs: []

agent:
  # provider: auto      # auto (picked from the API key that is set) | anthropic | openai | openrouter
  # model: claude-haiku-5-5   # default depends on the provider
  # instructions: "Integration tests in e2e/ talk to the API server built from server/."
`)
	return b.String()
}

func yamlStr(s string) string {
	out, err := yaml.Marshal(s)
	if err != nil {
		return fmt.Sprintf("%q", s)
	}
	return strings.TrimSpace(string(out))
}

// ensureGitignore adds .smates/ to the root .gitignore unless it is there, and
// reports whether it did.
func ensureGitignore(root string) (bool, error) {
	p := filepath.Join(root, ".gitignore")
	data, _ := os.ReadFile(p)
	for _, l := range strings.Split(string(data), "\n") {
		if t := strings.TrimSpace(l); t == ".smates/" || t == ".smates" || t == "/.smates/" || t == "/.smates" {
			return false, nil
		}
	}
	prefix := ""
	if len(data) > 0 && !bytes.HasSuffix(data, []byte("\n")) {
		prefix = "\n"
	}
	f, err := os.OpenFile(p, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return false, err
	}
	defer f.Close()
	_, err = io.WriteString(f, prefix+".smates/\n")
	return err == nil, err
}

// ---- why / deps / list ----

// whyReport is the JSON schema of `smates why --format json`.
type whyReport struct {
	// Target is the repo-relative file (or $VAR) asked about.
	Target string    `json:"target"`
	Tests  []whyTest `json:"tests"`
}

// whyTest is a test unit depending on the target.
type whyTest struct {
	Suite string `json:"suite"`
	Test  string `json:"test"`
	// Kind of dependency: content | listing | existence | tree.
	Kind string `json:"kind"`
	// How: the explanations the text output shows ("static/inputs", "recorded", a
	// depends_on/serves declaration, ...).
	How []string `json:"how"`
}

func cmdWhy(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("why", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var cf commonFlags
	cf.register(fs)
	format := formatFlag(fs)
	if err := fs.Parse(args); err != nil {
		return usageError{err}
	}
	if err := checkFormat(*format); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return usageError{errors.New("usage: smates why <file or $ENV_VAR>")}
	}
	p, err := load(&cf, stderr)
	if err != nil {
		return err
	}
	target := fs.Arg(0)
	if !depkey.IsEnv(target) {
		target = p.relArg(target)
	}
	res, err := selector.Select(&selector.Input{Repo: p.repo, Config: p.cfg, Suites: p.suites, Others: p.others, State: p.state, Snap: p.snap, Graph: p.graph, All: true})
	if err != nil {
		return err
	}
	rep := whyReport{Target: target, Tests: []whyTest{}}
	for _, s := range res.Suites {
		st := p.state.Suites[s.Name]
		for _, t := range s.Tests {
			deps := map[string]bool{}
			for _, d := range s.Deps[t.ID] {
				deps[d] = true
			}
			// The same rule the selector uses; existence changes are assumed so that
			// listings and existence checks are shown too.
			kind := selector.Covers(deps, target, true)
			// depends_on/serves: one key stands for the declaration's files.
			service := s.ServiceVia(t.ID, target)
			if kind == "" && service != "" {
				kind = "content"
			}
			if kind == "" {
				continue
			}
			var how []string
			switch kind {
			case "content":
				if service != "" {
					how = append(how, service)
				} else if slices.Contains(s.Known[t.ID], target) || slices.Contains(s.Known[t.ID], selector.ConfigKey(target)) {
					how = append(how, "static/inputs")
				}
				if st != nil && st.Tests[t.ID] != nil {
					_, ok := st.Tests[t.ID].Deps[target]
					_, okCfg := st.Tests[t.ID].Deps[selector.ConfigKey(target)]
					if ok || okCfg {
						how = append(how, "recorded")
					}
				}
			case "declarations":
				how = append(how, "static: its declarations (edits inside function bodies count only where the test executed them)")
				if st != nil && st.Tests[t.ID] != nil {
					if _, ok := st.Tests[t.ID].Deps[target]; ok {
						how = append(how, "executed")
					}
				}
			case "listing":
				how = append(how, "lists its directory (only additions/removals count)")
			case "existence":
				how = append(how, "checks whether it exists (only additions/removals count)")
			case "tree":
				how = append(how, "runs a command below its directory that smates cannot see into")
			}
			rep.Tests = append(rep.Tests, whyTest{Suite: s.Name, Test: t.ID, Kind: kind, How: nonNil(how)})
		}
	}
	if *format == "json" {
		return writeJSON(stdout, rep)
	}
	for _, t := range rep.Tests {
		fmt.Fprintf(stdout, "%s\t%s\t(%s)\n", t.Suite, t.Test, strings.Join(t.How, ", "))
	}
	if len(rep.Tests) == 0 {
		fmt.Fprintf(stderr, "smates: no test is known to depend on %s\n", target)
	}
	return nil
}

func (p *project) relArg(a string) string {
	abs := absPath(a)
	if r, ok := pathutil.Rel(p.repo.Root, abs); ok {
		return r
	}
	return filepath.ToSlash(filepath.Clean(a))
}

// depsReport is the JSON schema of `smates deps --format json`.
type depsReport struct {
	Suite string     `json:"suite"`
	Test  string     `json:"test"`
	Deps  []depEntry `json:"deps"`
}

// depEntry is one dependency of a test: a repo-relative file, or a dependency key such
// as "$VAR" (an environment variable).
type depEntry struct {
	Dep string `json:"dep"`
	// Sources: "static" (static analysis, inputs, rules, depends_on) and/or "recorded"
	// (seen while the test ran); empty for dependencies of neither kind.
	Sources []string `json:"sources"`
}

func cmdDeps(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("deps", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var cf commonFlags
	cf.register(fs)
	format := formatFlag(fs)
	if err := fs.Parse(args); err != nil {
		return usageError{err}
	}
	if err := checkFormat(*format); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return usageError{errors.New("usage: smates deps <test file or Go package dir>")}
	}
	p, err := load(&cf, stderr)
	if err != nil {
		return err
	}
	target := p.relArg(fs.Arg(0))
	res, err := selector.Select(&selector.Input{Repo: p.repo, Config: p.cfg, Suites: p.suites, Others: p.others, State: p.state, Snap: p.snap, Graph: p.graph, All: true})
	if err != nil {
		return err
	}
	for _, s := range res.Suites {
		deps, ok := s.Deps[target]
		if !ok {
			continue
		}
		var traced map[string]string
		if st := p.state.Suites[s.Name]; st != nil && st.Tests[target] != nil {
			traced = st.Tests[target].Deps
		}
		known := map[string]bool{}
		for _, d := range s.Known[target] {
			known[d] = true
		}
		// The suite's settings key and the file itself print as one line.
		type source struct{ static, recorded bool }
		var order []string
		srcs := map[string]*source{}
		for _, d := range deps {
			name := selector.DisplayKey(d)
			if srcs[name] == nil {
				order = append(order, name)
				srcs[name] = &source{}
			}
			_, rec := traced[d]
			srcs[name].static = srcs[name].static || known[d]
			srcs[name].recorded = srcs[name].recorded || rec
		}
		rep := depsReport{Suite: s.Name, Test: target, Deps: []depEntry{}}
		for _, name := range order {
			src := []string{}
			if srcs[name].static {
				src = append(src, "static")
			}
			if srcs[name].recorded {
				src = append(src, "recorded")
			}
			rep.Deps = append(rep.Deps, depEntry{Dep: name, Sources: src})
		}
		if *format == "json" {
			return writeJSON(stdout, rep)
		}
		for _, d := range rep.Deps {
			fmt.Fprintf(stdout, "%s\t%s\n", d.Dep, strings.Join(d.Sources, ","))
		}
		return nil
	}
	// A Go package directory whose units are its test functions.
	var funcs []string
	for _, s := range res.Suites {
		for _, t := range s.Tests {
			if d, _, ok := runner.SplitFuncUnit(t.ID); ok && d == target {
				funcs = append(funcs, t.ID)
			}
		}
	}
	if len(funcs) > 0 {
		return fmt.Errorf("%s is tested per function; pass one of: %s", target, strings.Join(limit(funcs, 20), ", "))
	}
	return fmt.Errorf("%s is not a test unit of any suite (see `smates list`)", target)
}

// listReport is the JSON schema of `smates list --format json`.
type listReport struct {
	// Config is the config file the suites come from ("" when auto-detected).
	Config string      `json:"config"`
	Suites []listSuite `json:"suites"`
}

type listSuite struct {
	Name    string `json:"name"`
	Runner  string `json:"runner"`
	Root    string `json:"root"`
	Command string `json:"command"`
	// UnitCount and Units: the suite's test units (test files, or Go packages).
	UnitCount int      `json:"unit_count"`
	Units     []string `json:"units"`
	// TestsSource says where the test patterns come from: "smates.yml tests",
	// "runner defaults", or the runner configuration they were read from (e.g.
	// "jest.config.js testMatch"); TestPatterns describes them (globs relative to the
	// root, "!" for exclusions, "regex ..." for jest regexes).
	TestsSource  string   `json:"tests_source"`
	TestPatterns []string `json:"test_patterns"`
	// DiscoveryWarning says why the runner configuration could not be read (the
	// default patterns are used then).
	DiscoveryWarning string `json:"discovery_warning,omitempty"`
}

func cmdList(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("list", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var cf commonFlags
	cf.register(fs)
	format := formatFlag(fs)
	if err := fs.Parse(args); err != nil {
		return usageError{err}
	}
	if err := checkFormat(*format); err != nil {
		return err
	}
	p, err := load(&cf, stderr)
	if err != nil {
		return err
	}
	if *format == "json" {
		rep := listReport{Config: p.cfg.Path, Suites: []listSuite{}}
		for _, s := range p.suites {
			units := s.Adapter.Units(s, s.TestFiles(p.files))
			rep.Suites = append(rep.Suites, listSuite{Name: s.Name, Runner: s.Runner, Root: s.Root, Command: s.Command, UnitCount: len(units), Units: nonNil(units),
				TestsSource: s.TestsSource, TestPatterns: nonNil(s.TestPatterns()), DiscoveryWarning: s.DiscoveryWarning})
		}
		return writeJSON(stdout, rep)
	}
	for _, s := range p.suites {
		units := s.Adapter.Units(s, s.TestFiles(p.files))
		fmt.Fprintf(stdout, "%s\trunner=%s\troot=%s\tunits=%d\tcommand=%s\ttests=%s\n", s.Name, s.Runner, s.Root, len(units), s.Command, s.TestsSource)
		if s.DiscoveryWarning != "" {
			fmt.Fprintf(stderr, "smates: suite %s: %s\n", s.Name, s.DiscoveryWarning)
		}
		if cf.verbose {
			fmt.Fprintf(stdout, "  tests from %s: %s\n", s.TestsSource, strings.Join(s.TestPatterns(), " "))
			for _, u := range units {
				fmt.Fprintf(stdout, "  %s\n", u)
			}
		}
	}
	return nil
}

// ---- state ----

const stateRefPrefix = "refs/smates/state/"

func cmdState(args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		return usageError{errors.New("usage: smates state show|path|pull|push|merge|clear")}
	}
	sub, rest := args[0], args[1:]
	fs := flag.NewFlagSet("state "+sub, flag.ContinueOnError)
	fs.SetOutput(stderr)
	dir := fs.String("C", "", "run as if started in `dir`")
	statePath := fs.String("state", "", "state file")
	remote := fs.String("remote", "origin", "git remote for pull/push")
	ref := fs.String("ref", "", "git ref for pull/push (default refs/smates/state/<branch>)")
	format := formatFlag(fs)
	asJSON := fs.Bool("json", false, "show: print JSON (deprecated: use --format json)")
	out := fs.String("o", "", "merge: output file (default: the state file)")
	if err := fs.Parse(rest); err != nil {
		return usageError{err}
	}
	if err := checkFormat(*format); err != nil {
		return err
	}
	if *asJSON {
		*format = "json"
	}
	store, err := resolveStore(*dir, *statePath, stderr)
	if err != nil {
		return err
	}
	repo, path := store.repo, store.path
	switch sub {
	case "path":
		fmt.Fprintln(stdout, path)
	case "clear":
		return store.Clear()
	case "show":
		st, err := store.Load()
		if err != nil {
			return err
		}
		return showState(st, store, *format == "json", stdout)
	case "merge":
		st, err := store.Load()
		if err != nil {
			return err
		}
		for _, f := range fs.Args() {
			o, err := state.Load(f)
			if err != nil {
				return fmt.Errorf("%s: %w", f, err)
			}
			st.Merge(o)
		}
		if *out != "" {
			return st.Save(*out)
		}
		return store.Save(st)
	case "push":
		if store.shared {
			// Push the entries with this worktree's snapshot.
			st, err := store.Load()
			if err != nil {
				return err
			}
			tmp := filepath.Join(filepath.Dir(store.snapPath), ".push-"+filepath.Base(store.path))
			defer os.Remove(tmp)
			if err := st.Save(tmp); err != nil {
				return err
			}
			path = tmp
		}
		r := *ref
		if r == "" {
			b := branchName(repo)
			if b == "" {
				return fmt.Errorf("cannot determine the branch (detached HEAD): pass --ref or set SMATES_BRANCH")
			}
			r = stateRefPrefix + b
		}
		return pushState(repo, path, *remote, r, stderr)
	case "pull":
		refs := []string{*ref}
		if *ref == "" {
			refs = stateRefs(repo)
		}
		if !store.shared {
			return pullState(repo, path, *remote, refs, stderr)
		}
		// Shared: merge into the entries other worktrees use instead of replacing them.
		tmp := filepath.Join(filepath.Dir(store.snapPath), ".pull-"+filepath.Base(store.path))
		defer os.Remove(tmp)
		if err := pullState(repo, tmp, *remote, refs, stderr); err != nil {
			return err
		}
		if _, err := os.Stat(tmp); err != nil {
			return nil
		}
		pulled, err := state.Load(tmp)
		if err != nil {
			return err
		}
		st, err := store.Load()
		if err != nil {
			return err
		}
		st.Merge(pulled)
		return store.Save(st)
	default:
		return usageError{fmt.Errorf("unknown state command %q", sub)}
	}
	return nil
}

// stateRefs are the refs `smates state pull` tries: the branch's, then the default branch's.
func stateRefs(repo *gitx.Repo) []string {
	ci := detectCI()
	b := first(os.Getenv("SMATES_BRANCH"), ci.Branch, repo.CurrentBranch())
	def := first(ci.DefaultBranch, repo.DefaultBranch())
	var refs []string
	if b != "" {
		refs = append(refs, stateRefPrefix+b)
	}
	if def != b {
		refs = append(refs, stateRefPrefix+def)
	}
	return refs
}

func branchName(repo *gitx.Repo) string {
	ci := detectCI()
	return first(os.Getenv("SMATES_BRANCH"), ci.Branch, repo.CurrentBranch())
}

func showState(st *state.State, store *stateStore, asJSON bool, w io.Writer) error {
	path := store.path
	type suiteInfo struct {
		Tests  int            `json:"tests"`
		Status map[string]int `json:"status"`
		Deps   int            `json:"avg_deps"`
	}
	info := struct {
		Path    string               `json:"path"`
		Commit  string               `json:"commit"`
		Created time.Time            `json:"created"`
		Tool    string               `json:"tool"`
		Files   int                  `json:"snapshot_files"`
		Suites  map[string]suiteInfo `json:"suites"`
		Agent   int                  `json:"agent_cache_entries"`
		Shared  bool                 `json:"shared"`
		// SnapshotPath is this worktree's snapshot file in shared mode.
		SnapshotPath string     `json:"snapshot_path,omitempty"`
		SeededFrom   string     `json:"seeded_from,omitempty"`
		SeededAt     *time.Time `json:"seeded_at,omitempty"`
	}{Path: path, Commit: st.Commit, Created: st.Created, Tool: st.Tool, Files: len(st.Files), Suites: map[string]suiteInfo{}, Agent: len(st.AgentCache),
		Shared: store.shared, SnapshotPath: store.snapPath}
	if from, at := store.Seeded(); from != "" {
		info.SeededFrom = from
		if !at.IsZero() {
			info.SeededAt = &at
		}
	}
	for name, su := range st.Suites {
		si := suiteInfo{Tests: len(su.Tests), Status: map[string]int{}}
		total := 0
		for _, e := range su.Tests {
			si.Status[e.Status]++
			total += len(e.Deps)
		}
		if len(su.Tests) > 0 {
			si.Deps = total / len(su.Tests)
		}
		info.Suites[name] = si
	}
	if asJSON {
		return writeJSON(w, info)
	}
	fmt.Fprintf(w, "state:    %s\nlocation: %s\n", path, store.describe())
	if info.SeededFrom != "" {
		when := ""
		if info.SeededAt != nil {
			when = " on " + info.SeededAt.Format(time.RFC3339)
		}
		fmt.Fprintf(w, "seeded:   from %s%s\n", info.SeededFrom, when)
	}
	if st.Empty() {
		fmt.Fprintf(w, "no recorded state\n")
		return nil
	}
	if st.Created.IsZero() {
		fmt.Fprintf(w, "recorded: no snapshot in this worktree yet (%s)\nsnapshot: %d files\n", st.Tool, len(st.Files))
	} else {
		fmt.Fprintf(w, "recorded: %s (commit %.12s, %s)\nsnapshot: %d files\n", st.Created.Format(time.RFC3339), st.Commit, st.Tool, len(st.Files))
	}
	for _, name := range slices.Sorted(maps.Keys(info.Suites)) {
		si := info.Suites[name]
		fmt.Fprintf(w, "suite %s: %d tests (pass %d, fail %d, unknown %d), ~%d deps each\n", name, si.Tests, si.Status["pass"], si.Status["fail"], si.Status["unknown"], si.Deps)
	}
	return nil
}

func pushState(repo *gitx.Repo, path, remote, ref string, log io.Writer) error {
	if _, err := os.Stat(path); err != nil {
		return fmt.Errorf("no state to push at %s", path)
	}
	blob, err := repo.Git("hash-object", "-w", path)
	if err != nil {
		return err
	}
	tree, err := gitStdin(repo, fmt.Sprintf("100644 blob %s\tstate.json.gz\n", strings.TrimSpace(blob)), "mktree")
	if err != nil {
		return err
	}
	msg := "smates state for " + repo.Head()
	commit, err := gitStdin(repo, msg+"\n", "-c", "user.name=smates", "-c", "user.email=smates@localhost", "commit-tree", strings.TrimSpace(tree))
	if err != nil {
		return err
	}
	if _, err := repo.Git("push", "--quiet", "--force", "--no-verify", remote, strings.TrimSpace(commit)+":"+ref); err != nil {
		return err
	}
	fmt.Fprintf(log, "smates: pushed state to %s %s\n", remote, ref)
	return nil
}

func pullState(repo *gitx.Repo, path, remote string, refs []string, log io.Writer) error {
	for _, ref := range refs {
		if _, err := repo.Git("fetch", "--quiet", "--no-tags", remote, "+"+ref+":"+ref); err != nil {
			continue
		}
		cmd := exec.Command("git", "cat-file", "blob", ref+":state.json.gz")
		cmd.Dir = repo.Root
		data, err := cmd.Output()
		if err != nil {
			continue
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(path, data, 0o644); err != nil {
			return err
		}
		fmt.Fprintf(log, "smates: pulled state from %s %s\n", remote, ref)
		return nil
	}
	fmt.Fprintf(log, "smates: no remote state found (%s); the next run will start fresh\n", strings.Join(refs, ", "))
	return nil
}

func gitStdin(repo *gitx.Repo, stdin string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = repo.Root
	cmd.Stdin = strings.NewReader(stdin)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git %s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(errb.String()))
	}
	return out.String(), nil
}

// serverSuiteHint says why suite s looks like end-to-end tests of a server running in
// another process ("" when it does not, or when it already declares depends_on, serves
// or rules): its name or root mentions e2e, its command runs Playwright or Cypress, or
// one of its first test files calls a local HTTP address.
func serverSuiteHint(repoRoot string, s *runner.Suite, files []string) string {
	if len(s.DependsOn) > 0 || len(s.Serves) > 0 || len(s.Rules) > 0 {
		return ""
	}
	isE2E := func(v string) bool {
		v = strings.ToLower(v)
		return strings.Contains(v, "e2e") || strings.Contains(v, "end-to-end") || strings.Contains(v, "end2end")
	}
	switch cmd := strings.ToLower(s.Command); {
	case isE2E(s.Name):
		return "name " + s.Name
	case isE2E(s.Root):
		return "root " + s.Root
	case strings.Contains(cmd, "playwright"):
		return "runs playwright"
	case strings.Contains(cmd, "cypress"):
		return "runs cypress"
	}
	const maxScanned = 20
	for i, f := range s.TestFiles(files) {
		if i == maxScanned {
			break
		}
		data, err := os.ReadFile(filepath.Join(repoRoot, filepath.FromSlash(f)))
		if err != nil {
			continue
		}
		for _, needle := range []string{"http://localhost", "http://127.0.0.1", "http://0.0.0.0"} {
			if bytes.Contains(data, []byte(needle)) {
				return f + " calls " + needle
			}
		}
	}
	return ""
}

package cli

import (
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
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
	c := newCommand("init", stderr)
	dir := c.String("C", "", "run as if started in `dir`")
	force := c.Bool("force", false, "overwrite an existing smates.yml")
	print := c.Bool("stdout", false, "print the config instead of writing it")
	github := c.Bool("github", false, "also write .github/workflows/smates.yml")
	patch := c.String("patch", "", "with --github: write the patch that switches existing workflows' test steps to smates to `file` (default: print it)")
	agentsMD := c.Bool("agents-md", false, "add a short smates section to AGENTS.md (or CLAUDE.md) for coding agents")
	c.withFormat("text", "json")
	if err := c.parse(args); err != nil {
		return err
	}
	asJSON := c.asJSON()
	d := *dir
	if d == "" {
		d = "."
	}
	repo, err := gitx.Open(d)
	if err != nil {
		return asEnvError(err)
	}
	snap, err := repo.Snapshot()
	if err != nil {
		return asEnvError(err)
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
			return asConfigError(err)
		}
		if existing != nil && !*github && !*agentsMD {
			return usageError{fmt.Errorf("%s already exists (use --force to overwrite, --github to only generate the workflow from it, or --agents-md to only add the section for coding agents)", existing.Path)}
		}
	}
	if existing != nil {
		return initExisting(repo, existing, files, *patch, *github, *agentsMD, asJSON, stdout, stderr)
	}
	suites, notes := detect.Detect(repo, files)
	for _, n := range notes {
		fmt.Fprintf(stderr, "smates: %s\n", n)
	}
	mk := makefileHints(repo.Root, suites)
	// The workflows running the tests tell which go test flags a suite needs (-p 1 for a
	// shared database) and what environment they set.
	wfSteps := workflowTestSteps(repo.Root)
	carried := carryGoFlags(suites, mk, makeGoVotes(repo.Root, suites), wfSteps, stderr)
	for _, h := range mk {
		if c := carried[h.Suite]; c != "" {
			fmt.Fprintf(stderr, "smates: %s target %q runs %q; the %s suite's command in smates.yml keeps %s, but smates runs the runner itself, so its other flags/variables are not applied. See the commented command:/env: hint for the suite in smates.yml.\n", h.File, h.Target, h.Line, h.Suite, c)
		} else {
			fmt.Fprintf(stderr, "smates: %s target %q runs %q; smates runs the %s suite's runner itself, so those flags/variables are not applied. See the commented command:/env: hint for the suite in smates.yml.\n", h.File, h.Target, h.Line, h.Suite)
		}
	}
	doc := renderConfig(suites, configHints(mk, workflowEnvHints(wfSteps, suites), carried))
	// --force overwrites the config file smates reads, whichever of its names it has
	// (a new smates.yml next to a .smates.yml would shadow it).
	name := configFileName(repo.Root)
	rep := &initReport{Config: name, FilesWritten: []string{}, Suites: []initSuite{}, NeedsReview: []reviewItem{}, NextSteps: initNextSteps}
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
	path := filepath.Join(repo.Root, name)
	if err := os.WriteFile(path, []byte(doc), 0o644); err != nil {
		return err
	}
	rep.Written = true
	rep.FilesWritten = append(rep.FilesWritten, name)
	if changed, err := ensureGitignore(repo.Root); err != nil {
		fmt.Fprintf(stderr, "smates: could not update .gitignore: %v\n", err)
	} else if changed {
		rep.FilesWritten = append(rep.FilesWritten, ".gitignore")
	}
	out := stdout
	if asJSON {
		out = io.Discard
	}
	fmt.Fprintf(out, "wrote %s with %d suite(s):\n", name, len(suites))
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
	if err := rep.addAgentsMD(repo.Root, *agentsMD); err != nil {
		return err
	}
	if asJSON {
		rep.NeedsReview = initReview(repo, nil, suites, files, mk, *github)
		return writeJSON(stdout, rep)
	}
	fmt.Fprint(stdout, initNextStepsText)
	rep.AgentsMD.print(stdout)
	return nil
}

const initNextStepsText = `
Next steps:
  smates run --all     # run everything once and record what each test uses
  smates run           # afterwards: run only what your changes can affect
  smates why <file>    # see which tests depend on a file
  smates hook install  # optional: run the affected tests before every git push
`

// initExisting is `smates init --github` and/or `--agents-md` over an existing config
// (without --force): the config is left untouched; --github generates the workflow (or
// the patch for existing workflows) from its suites, --agents-md adds the section for
// coding agents.
func initExisting(repo *gitx.Repo, cfg *config.Config, files []string, patch string, github, agentsMD, asJSON bool, stdout, stderr io.Writer) error {
	rep := &initReport{Config: cfg.Path, FilesWritten: []string{}, Suites: []initSuite{}, NeedsReview: []reviewItem{}, NextSteps: initNextSteps}
	for _, s := range cfg.Suites {
		rep.Suites = append(rep.Suites, initSuite{Name: s.Name, Runner: s.Runner, Root: s.Root, Exclude: nonNil(s.Exclude)})
	}
	out := stdout
	if asJSON {
		out = io.Discard
	}
	if github {
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
	} else {
		fmt.Fprintf(out, "%s exists: left unchanged\n", cfg.Path)
	}
	if err := rep.addAgentsMD(repo.Root, agentsMD); err != nil {
		return err
	}
	if asJSON {
		rep.NeedsReview = initReview(repo, cfg, cfg.Suites, files, nil, github)
		return writeJSON(stdout, rep)
	}
	rep.AgentsMD.print(stdout)
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
		// What detection found besides the defaults (Create React App's test patterns,
		// the script's command and environment) is kept, or the suite would change.
		if len(s.Tests) > 0 {
			b.WriteString("    tests:\n")
			for _, t := range s.Tests {
				fmt.Fprintf(&b, "      - %s\n", yamlStr(t))
			}
		}
		if len(s.Exclude) > 0 {
			b.WriteString("    exclude:\n")
			for _, e := range s.Exclude {
				fmt.Fprintf(&b, "      - %s\n", yamlStr(e))
			}
		}
		if s.Command != "" {
			fmt.Fprintf(&b, "    command: %s\n", yamlStr(s.Command))
		}
		if len(s.Env) > 0 {
			b.WriteString("    env:\n")
			keys := make([]string, 0, len(s.Env))
			for k := range s.Env {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				fmt.Fprintf(&b, "      %s: %s\n", yamlStr(k), yamlStr(s.Env[k]))
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

// yamlStr renders s as a YAML scalar for one line after "key: " or "- ". A string with
// a line break is double-quoted ("a\nb"): the block scalar yaml.Marshal picks for it
// spans lines and would not parse spliced into a line.
func yamlStr(s string) string {
	var v any = s
	if strings.ContainsAny(s, "\n\r") {
		v = &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: s, Style: yaml.DoubleQuotedStyle}
	}
	out, err := yaml.Marshal(v)
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
	// Kind of dependency: content | declarations | listing | existence | tree.
	Kind string `json:"kind"`
	// How: the explanations the text output shows ("static/inputs", "recorded", a
	// depends_on/serves declaration, ...).
	How []string `json:"how"`
}

func cmdWhy(args []string, stdout, stderr io.Writer) error {
	c := newCommand("why", stderr)
	var cf commonFlags
	c.common(&cf)
	c.withFormat("text", "json")
	if _, err := c.parseInterspersed(args); err != nil {
		return err
	}
	target, err := c.oneArg("usage: smates why <file or $ENV_VAR>")
	if err != nil {
		return err
	}
	p, err := load(&cf, stderr)
	if err != nil {
		return err
	}
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
			kind := depkey.Covers(deps, target, true)
			// depends_on/serves: one key stands for the declaration's files.
			service := s.ServiceVia(t.ID, target)
			if kind == depkey.CoverNone && service != "" {
				kind = depkey.CoverContent
			}
			if kind == depkey.CoverNone {
				continue
			}
			var how []string
			switch kind {
			case depkey.CoverContent:
				if service != "" {
					how = append(how, service)
				} else if slices.Contains(s.Known[t.ID], target) || slices.Contains(s.Known[t.ID], depkey.Config(target)) {
					how = append(how, "static/inputs")
				}
				if st != nil && st.Tests[t.ID] != nil {
					_, ok := st.Tests[t.ID].Deps[target]
					_, okCfg := st.Tests[t.ID].Deps[depkey.Config(target)]
					if ok || okCfg {
						how = append(how, "recorded")
					}
				}
			case depkey.CoverDecls:
				how = append(how, "static: its declarations (edits inside function bodies count only where the test executed or reaches them)")
				var names []string
				for d := range deps {
					if f, name, ok := depkey.DeclName(d); ok && f == target {
						names = append(names, name)
					}
				}
				if len(names) > 0 {
					slices.Sort(names)
					how = append(how, "bodies: "+strings.Join(names, ", "))
				}
				if st != nil && st.Tests[t.ID] != nil {
					if _, ok := st.Tests[t.ID].Deps[target]; ok {
						how = append(how, "executed")
					}
				}
			case depkey.CoverListing:
				how = append(how, "lists its directory (only additions/removals count)")
			case depkey.CoverExistence:
				how = append(how, "checks whether it exists (only additions/removals count)")
			case depkey.CoverTree:
				how = append(how, "runs a command below its directory that smates cannot see into")
			}
			rep.Tests = append(rep.Tests, whyTest{Suite: s.Name, Test: t.ID, Kind: string(kind), How: nonNil(how)})
		}
	}
	return c.emit(stdout, rep, func() {
		for _, t := range rep.Tests {
			fmt.Fprintf(stdout, "%s\t%s\t(%s)\n", t.Suite, t.Test, strings.Join(t.How, ", "))
		}
		if len(rep.Tests) == 0 {
			fmt.Fprintf(stderr, "smates: no test is known to depend on %s\n", target)
		}
	})
}

// goTestFunc is the name of a Go test, benchmark, fuzz test or example function.
var goTestFunc = regexp.MustCompile(`^(Test|Benchmark|Fuzz|Example)\w*$`)

func (p *project) relArg(a string) string {
	// A Go test function unit (dir#TestName): resolve the directory, keep the name
	// (pathutil.Rel would keep "#TestName" as part of a file name). A subtest
	// (dir#TestName/sub) runs in its function's unit, so it names that unit.
	if dir, name, ok := runner.SplitFuncUnit(a); ok && dir != "" && name != "" {
		if top, _, sub := strings.Cut(name, "/"); sub && goTestFunc.MatchString(top) {
			name = top
		}
		if !strings.ContainsAny(name, `/\.`) {
			return p.relArg(dir) + "#" + name
		}
	}
	abs := absPath(a)
	// The repository root itself (pathutil.Rel rejects "."): the root package, whose
	// Go test function units are ".#TestName".
	if root := filepath.Clean(p.repo.Root); filepath.Clean(abs) == root {
		return "."
	} else if r, err := filepath.EvalSymlinks(abs); err == nil && r == root {
		return "."
	}
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
	c := newCommand("deps", stderr)
	var cf commonFlags
	c.common(&cf)
	c.withFormat("text", "json")
	if _, err := c.parseInterspersed(args); err != nil {
		return err
	}
	arg, err := c.oneArg("usage: smates deps <test file or Go package dir>")
	if err != nil {
		return err
	}
	p, err := load(&cf, stderr)
	if err != nil {
		return err
	}
	target := p.relArg(arg)
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
			name := depkey.Display(d)
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
		return c.emit(stdout, rep, func() {
			for _, d := range rep.Deps {
				fmt.Fprintf(stdout, "%s\t%s\n", d.Dep, strings.Join(d.Sources, ","))
			}
		})
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
		return configError{fmt.Errorf("%s is tested per function; pass one of: %s", target, strings.Join(limit(funcs, 20), ", "))}
	}
	return configError{fmt.Errorf("%s is not a test unit of any suite (see `smates list`)", target)}
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
	c := newCommand("list", stderr)
	var cf commonFlags
	c.common(&cf)
	c.withFormat("text", "json")
	if err := c.parse(args); err != nil {
		return err
	}
	p, err := load(&cf, stderr)
	if err != nil {
		return err
	}
	rep := listReport{Config: p.cfg.Path, Suites: []listSuite{}}
	for _, s := range p.suites {
		units := s.Adapter.Units(s, s.TestFiles(p.files))
		rep.Suites = append(rep.Suites, listSuite{Name: s.Name, Runner: s.Runner, Root: s.Root, Command: s.Command, UnitCount: len(units), Units: nonNil(units),
			TestsSource: s.TestsSource, TestPatterns: nonNil(s.TestPatterns()), DiscoveryWarning: s.DiscoveryWarning})
	}
	return c.emit(stdout, rep, func() {
		for _, s := range rep.Suites {
			fmt.Fprintf(stdout, "%s\trunner=%s\troot=%s\tunits=%d\tcommand=%s\ttests=%s\n", s.Name, s.Runner, s.Root, s.UnitCount, s.Command, s.TestsSource)
			if s.DiscoveryWarning != "" {
				fmt.Fprintf(stderr, "smates: suite %s: %s\n", s.Name, s.DiscoveryWarning)
			}
			if cf.verbose {
				fmt.Fprintf(stdout, "  tests from %s: %s\n", s.TestsSource, strings.Join(s.TestPatterns, " "))
				for _, u := range s.Units {
					fmt.Fprintf(stdout, "  %s\n", u)
				}
			}
		}
	})
}

// ---- state ----

const stateRefPrefix = "refs/smates/state/"

const stateCommands = `usage: smates state show [--format json]          print the recorded state
       smates state path                        print the state file path
       smates state pull|push [--remote] [--ref] fetch or publish the state
       smates state merge <file>... [-o file]   merge other state files into the state
       smates state clear                       delete the state`

func cmdState(args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		return usageError{errors.New("usage: smates state show|path|pull|push|merge|clear")}
	}
	sub, rest := args[0], args[1:]
	c := newCommand("state "+sub, stderr)
	dir := c.String("C", "", "run as if started in `dir`")
	statePath := c.String("state", "", "state file")
	remote := c.String("remote", "origin", "git remote for pull/push")
	ref := c.String("ref", "", "git ref for pull/push (default refs/smates/state/<branch>)")
	c.withFormat("text", "json")
	c.deprecatedJSON("show: print JSON (deprecated: use --format json)")
	out := c.String("o", "", "merge: output file (default: the state file)")
	if isHelp(sub) {
		printGroupHelp(stdout, stateCommands, c)
		return nil
	}
	files, err := c.parseInterspersed(rest)
	if err != nil {
		return err
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
		return showState(st, store, c.asJSON(), stdout)
	case "merge":
		st, err := store.Load()
		if err != nil {
			return err
		}
		for _, f := range files {
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
		return usageError{fmt.Errorf("unknown state command %q\n%s", sub, stateCommands)}
	}
	return nil
}

// stateRefs are the refs `smates state pull` tries: the branch's, then the base
// branch's of a pull request, then the default branch's.
func stateRefs(repo *gitx.Repo) []string {
	ci := detectCI()
	return stateRefsFor(first(os.Getenv("SMATES_BRANCH"), ci.Branch, repo.CurrentBranch()), ci.BaseRef, first(ci.DefaultBranch, repo.DefaultBranch()))
}

func stateRefsFor(branch, base, def string) []string {
	var refs []string
	for _, b := range []string{branch, base, def} {
		if b != "" && !slices.Contains(refs, stateRefPrefix+b) {
			refs = append(refs, stateRefPrefix+b)
		}
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
		fmt.Fprintf(w, "suite %s: %d tests (pass %d, fail %d, skip %d, unknown %d), ~%d deps each\n", name, si.Tests, si.Status[state.Pass], si.Status[state.Fail], si.Status[state.Skip], si.Status[state.Unknown], si.Deps)
	}
	return nil
}

// pushState pushes the state file as state.json.gz in a commit of its own; a plain
// .json state is compressed first, so the blob always matches its name.
func pushState(repo *gitx.Repo, path, remote, ref string, log io.Writer) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("no state to push at %s", path)
	}
	if !isGzip(data) {
		var b bytes.Buffer
		zw := gzip.NewWriter(&b)
		zw.Write(data)
		if err := zw.Close(); err != nil {
			return err
		}
		data = b.Bytes()
	}
	blob, err := gitStdin(repo, string(data), "hash-object", "-w", "--stdin")
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

// pullState fetches the first of refs that holds a state and saves it at path, in the
// format path's name asks for (gzip for .gz). A pushed blob is decoded whatever its
// encoding: older versions pushed a plain .json state under the same name. A state this
// version cannot read (one a newer smates pushed) is skipped for the next ref, the
// default branch's: it is an error only when no ref has a state to use.
func pullState(repo *gitx.Repo, path, remote string, refs []string, log io.Writer) error {
	var unreadable error
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
		st, err := state.Read(bytes.NewReader(data), isGzip(data))
		if err != nil {
			fmt.Fprintf(log, "smates: skipping the state on %s %s: %v\n", remote, ref, err)
			if unreadable == nil {
				unreadable = fmt.Errorf("state on %s %s: %w", remote, ref, err)
			}
			continue
		}
		if err := st.Save(path); err != nil {
			return err
		}
		fmt.Fprintf(log, "smates: pulled state from %s %s\n", remote, ref)
		return nil
	}
	if unreadable != nil {
		return unreadable
	}
	fmt.Fprintf(log, "smates: no remote state found (%s); the next run will start fresh\n", strings.Join(refs, ", "))
	return nil
}

// isGzip reports whether data starts with the gzip magic number.
func isGzip(data []byte) bool { return len(data) >= 2 && data[0] == 0x1f && data[1] == 0x8b }

// configFileName is the name of the config file at root that config.Load reads (the
// first of config.FileNames there), or smates.yml when there is none.
func configFileName(root string) string {
	for _, n := range config.FileNames {
		if _, err := os.Stat(filepath.Join(root, n)); !errors.Is(err, os.ErrNotExist) {
			return n
		}
	}
	return "smates.yml"
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

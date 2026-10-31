package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/dotneet/smates/internal/agent"
	"github.com/dotneet/smates/internal/config"
	"github.com/dotneet/smates/internal/detect"
	"github.com/dotneet/smates/internal/gitx"
	"github.com/dotneet/smates/internal/pathutil"
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
	if err := fs.Parse(args); err != nil {
		return usageError{err}
	}
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
	suites := detect.Suites(repo, files)
	doc := renderConfig(suites)
	if *print {
		_, err := io.WriteString(stdout, doc)
		return err
	}
	path := filepath.Join(repo.Root, "smates.yml")
	if _, err := os.Stat(path); err == nil && !*force {
		return fmt.Errorf("smates.yml already exists (use --force to overwrite)")
	}
	if err := os.WriteFile(path, []byte(doc), 0o644); err != nil {
		return err
	}
	if err := ensureGitignore(repo.Root); err != nil {
		fmt.Fprintf(stderr, "smates: could not update .gitignore: %v\n", err)
	}
	if *github {
		wf := filepath.Join(repo.Root, ".github", "workflows", "smates.yml")
		if _, err := os.Stat(wf); err == nil && !*force {
			fmt.Fprintf(stderr, "smates: %s exists, not overwritten (use --force)\n", ".github/workflows/smates.yml")
		} else {
			if err := os.MkdirAll(filepath.Dir(wf), 0o755); err != nil {
				return err
			}
			if err := os.WriteFile(wf, []byte(renderWorkflow(repo, suites)), 0o644); err != nil {
				return err
			}
			fmt.Fprintln(stdout, "wrote .github/workflows/smates.yml (add an ANTHROPIC_API_KEY secret to enable the agent)")
		}
	}
	fmt.Fprintf(stdout, "wrote smates.yml with %d suite(s):\n", len(suites))
	for _, s := range suites {
		fmt.Fprintf(stdout, "  - %s (%s) in %s\n", s.Name, s.Runner, s.Root)
	}
	if len(suites) == 0 {
		fmt.Fprintln(stdout, "  (no test suites detected: add them to smates.yml by hand)")
	}
	fmt.Fprint(stdout, `
Next steps:
  smates run --all     # run everything once and record what each test uses
  smates run           # afterwards: run only what your changes can affect
  smates why <file>    # see which tests depend on a file
`)
	return nil
}

func renderConfig(suites []config.Suite) string {
	var b strings.Builder
	b.WriteString(`# smates configuration. Docs: https://github.com/dotneet/smates
version: 1

# Test suites: one runner invocation each. Paths are relative to the repository root.
# Optional per suite: tests/exclude (globs relative to root), command, inputs, rules, env,
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
	}
	b.WriteString(`
# Changed files no test is known to depend on ("orphans"): agent | all | ignore.
# "agent" asks Claude (needs ANTHROPIC_API_KEY) and falls back to "all".
orphans: agent

# Changes that never require tests unless a test reads them (added to built-in defaults
# for docs, editor and lint configuration).
ignore: []

# Files every test depends on, e.g. CI toolchain pins.
inputs: []

agent:
  model: claude-haiku-5-5
  # instructions: "Integration tests in e2e/ talk to the API server built from server/."
`)
	return b.String()
}

// renderWorkflow writes a GitHub Actions workflow that sets up the toolchains the
// detected suites need, installs dependencies and runs smates.
func renderWorkflow(repo *gitx.Repo, suites []config.Suite) string {
	has := func(name string) bool {
		_, err := os.Stat(filepath.Join(repo.Root, filepath.FromSlash(name)))
		return err == nil
	}
	var setup, install []string
	seen := map[string]bool{}
	once := func(key string, lines ...string) {
		if !seen[key] {
			seen[key] = true
			setup = append(setup, lines...)
		}
	}
	for _, s := range suites {
		root := s.Root
		at := func(f string) string {
			if root == "." {
				return f
			}
			return root + "/" + f
		}
		switch s.Runner {
		case "go":
			once("go:"+root, "      - uses: actions/setup-go@v5", "        with:", "          go-version-file: "+at("go.mod"))
		case "pytest":
			once("uv", "      - uses: astral-sh/setup-uv@v6")
		case "bun":
			once("bun", "      - uses: oven-sh/setup-bun@v2")
			if !seen["install:"+root] {
				seen["install:"+root] = true
				install = append(install, "      - run: bun install --frozen-lockfile", "        working-directory: "+root)
			}
		default: // vitest, jest, node
			version := "22"
			for _, f := range []string{".nvmrc", ".node-version"} {
				if data, err := os.ReadFile(filepath.Join(repo.Root, f)); err == nil && strings.TrimSpace(string(data)) != "" {
					version = strings.TrimPrefix(strings.TrimSpace(string(data)), "v")
				}
			}
			cmd := "npm ci"
			for d := root; ; d = filepath.ToSlash(filepath.Dir(d)) {
				rel := func(f string) string {
					if d == "." {
						return f
					}
					return d + "/" + f
				}
				switch {
				case has(rel("pnpm-lock.yaml")):
					once("pnpm", "      - uses: pnpm/action-setup@v4")
					cmd = "pnpm install --frozen-lockfile"
				case has(rel("bun.lock")) || has(rel("bun.lockb")):
					once("bun", "      - uses: oven-sh/setup-bun@v2")
					cmd = "bun install --frozen-lockfile"
				case has(rel("yarn.lock")):
					cmd = "yarn install --immutable"
				case has(rel("package-lock.json")):
					cmd = "npm ci"
				default:
					if d != "." {
						continue
					}
				}
				if !seen["install:"+d] {
					seen["install:"+d] = true
					install = append(install, "      - run: "+cmd, "        working-directory: "+d)
				}
				break
			}
			once("node", "      - uses: actions/setup-node@v4", "        with:", "          node-version: "+strconv.Quote(version))
		}
	}
	var b strings.Builder
	b.WriteString(`# Runs only the tests a change can affect. Generated by "smates init --github".
name: tests
on:
  push:
    branches: [` + repo.DefaultBranch() + `]
  pull_request:

jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
`)
	for _, l := range setup {
		b.WriteString(l + "\n")
	}
	for _, l := range install {
		b.WriteString(l + "\n")
	}
	b.WriteString(`      # On pushes to the default branch every test runs and the recorded state is
      # refreshed; on pull requests only affected tests run.
      - uses: dotneet/smates@v1
        with:
          anthropic-api-key: ${{ secrets.ANTHROPIC_API_KEY }}   # optional: narrows unattributed changes
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

func ensureGitignore(root string) error {
	p := filepath.Join(root, ".gitignore")
	data, _ := os.ReadFile(p)
	for _, l := range strings.Split(string(data), "\n") {
		if t := strings.TrimSpace(l); t == ".smates/" || t == ".smates" || t == "/.smates/" || t == "/.smates" {
			return nil
		}
	}
	prefix := ""
	if len(data) > 0 && !bytes.HasSuffix(data, []byte("\n")) {
		prefix = "\n"
	}
	f, err := os.OpenFile(p, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = io.WriteString(f, prefix+".smates/\n")
	return err
}

// ---- why / deps / list ----

func cmdWhy(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("why", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var cf commonFlags
	cf.register(fs)
	if err := fs.Parse(args); err != nil {
		return usageError{err}
	}
	if fs.NArg() != 1 {
		return usageError{errors.New("usage: smates why <file>")}
	}
	p, err := load(&cf, stderr)
	if err != nil {
		return err
	}
	target := p.relArg(fs.Arg(0))
	res, err := selector.Select(&selector.Input{Repo: p.repo, Config: p.cfg, Suites: p.suites, State: p.state, Snap: p.snap, Graph: p.graph, All: true})
	if err != nil {
		return err
	}
	n := 0
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
			if kind == "" {
				continue
			}
			var how []string
			switch kind {
			case "content":
				for _, d := range s.Known[t.ID] {
					if d == target {
						how = append(how, "static/inputs")
					}
				}
				if st != nil && st.Tests[t.ID] != nil {
					if _, ok := st.Tests[t.ID].Deps[target]; ok {
						how = append(how, "recorded")
					}
				}
			case "listing":
				how = append(how, "lists its directory (only additions/removals count)")
			case "existence":
				how = append(how, "checks whether it exists (only additions/removals count)")
			case "tree":
				how = append(how, "runs a command below its directory that smates cannot see into")
			}
			fmt.Fprintf(stdout, "%s\t%s\t(%s)\n", s.Name, t.ID, strings.Join(how, ", "))
			n++
		}
	}
	if n == 0 {
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

func cmdDeps(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("deps", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var cf commonFlags
	cf.register(fs)
	if err := fs.Parse(args); err != nil {
		return usageError{err}
	}
	if fs.NArg() != 1 {
		return usageError{errors.New("usage: smates deps <test file or Go package dir>")}
	}
	p, err := load(&cf, stderr)
	if err != nil {
		return err
	}
	target := p.relArg(fs.Arg(0))
	res, err := selector.Select(&selector.Input{Repo: p.repo, Config: p.cfg, Suites: p.suites, State: p.state, Snap: p.snap, Graph: p.graph, All: true})
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
		for _, d := range deps {
			var src []string
			if known[d] {
				src = append(src, "static")
			}
			if _, ok := traced[d]; ok {
				src = append(src, "recorded")
			}
			fmt.Fprintf(stdout, "%s\t%s\n", d, strings.Join(src, ","))
		}
		return nil
	}
	return fmt.Errorf("%s is not a test unit of any suite (see `smates list`)", target)
}

func cmdList(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("list", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var cf commonFlags
	cf.register(fs)
	if err := fs.Parse(args); err != nil {
		return usageError{err}
	}
	p, err := load(&cf, stderr)
	if err != nil {
		return err
	}
	for _, s := range p.suites {
		units := s.Adapter.Units(s, s.TestFiles(p.files))
		fmt.Fprintf(stdout, "%s\trunner=%s\troot=%s\tunits=%d\tcommand=%s\n", s.Name, s.Runner, s.Root, len(units), s.Command)
		if cf.verbose {
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
	asJSON := fs.Bool("json", false, "show: print JSON")
	out := fs.String("o", "", "merge: output file (default: the state file)")
	if err := fs.Parse(rest); err != nil {
		return usageError{err}
	}
	d := *dir
	if d == "" {
		d = "."
	}
	repo, err := gitx.Open(d)
	if err != nil {
		return err
	}
	cfg, err := config.Load(repo.Root)
	if err != nil {
		return err
	}
	if cfg == nil {
		cfg = &config.Config{}
		_ = cfg.Normalize()
	}
	path := *statePath
	if path == "" {
		path = filepath.Join(repo.Root, filepath.FromSlash(cfg.State))
	}
	switch sub {
	case "path":
		fmt.Fprintln(stdout, path)
	case "clear":
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	case "show":
		st, err := state.Load(path)
		if err != nil {
			return err
		}
		return showState(st, path, *asJSON, stdout)
	case "merge":
		st, err := state.Load(path)
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
		dst := *out
		if dst == "" {
			dst = path
		}
		return st.Save(dst)
	case "push":
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
			ci := detectCI()
			b := first(os.Getenv("SMATES_BRANCH"), ci.Branch, repo.CurrentBranch())
			def := first(ci.DefaultBranch, repo.DefaultBranch())
			refs = nil
			if b != "" {
				refs = append(refs, stateRefPrefix+b)
			}
			if def != b {
				refs = append(refs, stateRefPrefix+def)
			}
		}
		return pullState(repo, path, *remote, refs, stderr)
	default:
		return usageError{fmt.Errorf("unknown state command %q", sub)}
	}
	return nil
}

func branchName(repo *gitx.Repo) string {
	ci := detectCI()
	return first(os.Getenv("SMATES_BRANCH"), ci.Branch, repo.CurrentBranch())
}

func showState(st *state.State, path string, asJSON bool, w io.Writer) error {
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
	}{Path: path, Commit: st.Commit, Created: st.Created, Tool: st.Tool, Files: len(st.Files), Suites: map[string]suiteInfo{}, Agent: len(st.AgentCache)}
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
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(info)
	}
	if st.Empty() {
		fmt.Fprintf(w, "%s: no recorded state\n", path)
		return nil
	}
	fmt.Fprintf(w, "state:    %s\nrecorded: %s (commit %.12s, %s)\nsnapshot: %d files\n", path, st.Created.Format(time.RFC3339), st.Commit, st.Tool, len(st.Files))
	for _, name := range sortedKeys(info.Suites) {
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

// ---- doctor ----

func cmdDoctor(args []string, stdout, stderr io.Writer) (int, error) {
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var cf commonFlags
	cf.register(fs)
	if err := fs.Parse(args); err != nil {
		return 2, usageError{err}
	}
	problems := 0
	ok := func(f string, a ...any) { fmt.Fprintf(stdout, "  ok    "+f+"\n", a...) }
	warn := func(f string, a ...any) { fmt.Fprintf(stdout, "  warn  "+f+"\n", a...) }
	bad := func(f string, a ...any) { problems++; fmt.Fprintf(stdout, "  FAIL  "+f+"\n", a...) }

	fmt.Fprintf(stdout, "smates %s\n", Version)
	if _, err := exec.LookPath("git"); err != nil {
		bad("git not found on PATH")
		return 1, nil
	}
	p, err := load(&cf, io.Discard)
	if err != nil {
		bad("%v", err)
		return 1, nil
	}
	ok("repository %s", p.repo.Root)
	if p.cfg.Path != "" {
		ok("config %s", p.cfg.Path)
	} else {
		warn("no smates.yml: using auto-detected suites (run `smates init` to pin them)")
	}
	if len(p.suites) == 0 {
		bad("no test suites detected")
	}
	runners := map[string]bool{}
	for _, s := range p.suites {
		units := s.Adapter.Units(s, s.TestFiles(p.files))
		if len(units) == 0 {
			warn("suite %s (%s): no test files match %v", s.Name, s.Runner, s.Tests)
		} else {
			ok("suite %s (%s, root %s): %d test units; command: %s", s.Name, s.Runner, s.Root, len(units), s.Command)
		}
		runners[s.Runner] = true
	}
	tool := func(name string, args ...string) string {
		out, err := exec.Command(name, args...).Output()
		if err != nil {
			return ""
		}
		return strings.TrimSpace(strings.SplitN(string(out), "\n", 2)[0])
	}
	if runners["vitest"] || runners["jest"] || runners["node"] {
		if v := tool("node", "--version"); v == "" {
			bad("node not found on PATH")
		} else if strings.HasPrefix(v, "v1") && v < "v18" {
			bad("node %s is too old (tracing needs Node 18+)", v)
		} else {
			ok("node %s", v)
		}
	}
	if runners["bun"] {
		if v := tool("bun", "--version"); v == "" {
			bad("bun not found on PATH")
		} else {
			ok("bun %s", v)
		}
	}
	if runners["go"] {
		if v := tool("go", "version"); v == "" {
			bad("go not found on PATH")
		} else {
			ok("%s", v)
		}
	}
	if runners["pytest"] {
		py := tool("python3", "-c", "import sys;print('%d.%d'%sys.version_info[:2])")
		if py == "" {
			warn("python3 not found on PATH (fine if the suite command provides Python, e.g. uv)")
		} else if py < "3.12" && len(py) == 4 {
			warn("python %s: tracing uses sys.setprofile (slower); Python 3.12+ uses sys.monitoring", py)
		} else {
			ok("python %s", py)
		}
	}
	if p.state.Empty() {
		warn("no recorded state at %s: the first `smates run --all` (or a run on the default branch in CI) records it", p.statePath)
	} else {
		age := time.Since(p.state.Created).Round(time.Minute)
		n := 0
		for _, s := range p.state.Suites {
			n += len(s.Tests)
		}
		ok("state %s: %d tests recorded, snapshot from commit %.12s (%s ago)", p.statePath, n, p.state.Commit, age)
	}
	if okA, why := agent.Available(p.cfg.Agent); okA {
		ok("agent: %s available for unattributed changes (orphans: %s)", first(p.cfg.Agent.Command, p.cfg.Agent.Model), p.cfg.Orphans)
	} else if p.cfg.Orphans == "agent" {
		warn("agent unavailable (%s): unattributed changes run the affected suites in full", why)
	}
	if ci := detectCI(); ci.Name != "" {
		full, b := fullRunBranch(ci, p.repo, p.cfg.FullRunBranches)
		ok("CI %s, branch %q, pull request %t, full run by default: %t", ci.Name, b, ci.PullRequest, full)
	}
	if problems > 0 {
		return 1, nil
	}
	return 0, nil
}

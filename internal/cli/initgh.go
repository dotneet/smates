package cli

// Project inspection behind `smates init --github` and `smates doctor`: toolchain pins
// (mise, asdf, .nvmrc, package.json, .python-version) and the generated workflow. See
// inspect.go (services, environment), workflows.go (existing workflows) and makefile.go.

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/dotneet/smates/internal/config"
	"github.com/dotneet/smates/internal/gitx"
)

// Action versions used in generated workflows (keep docs/ in sync).
const (
	actCheckout    = "actions/checkout@v5"
	actSetupNode   = "actions/setup-node@v4"
	actSetupGo     = "actions/setup-go@v5"
	actSetupPython = "actions/setup-python@v5"
	actSetupUV     = "astral-sh/setup-uv@v6"
	actSetupBun    = "oven-sh/setup-bun@v2"
	actSetupPnpm   = "pnpm/action-setup@v4"
	actMise        = "jdx/mise-action@v2"
	actSmates      = "dotneet/smates@v1"
)

func fileExists(root, rel string) bool {
	_, err := os.Stat(filepath.Join(root, filepath.FromSlash(rel)))
	return err == nil
}

// joinRel joins a repo-relative directory and a name ("." stays out of the result).
func joinRel(dir, name string) string {
	if dir == "." || dir == "" {
		return name
	}
	return dir + "/" + name
}

// upDirs lists dir and its parents up to the repository root ("."), nearest first.
func upDirs(dir string) []string {
	dir = path.Clean(filepath.ToSlash(dir))
	var out []string
	for {
		out = append(out, dir)
		if dir == "." || dir == "/" || dir == "" {
			return out
		}
		dir = path.Dir(dir)
	}
}

// ---- toolchains ----

// miseConfigs are the files jdx/mise-action installs tools from.
var miseConfigs = []string{"mise.toml", ".mise.toml", ".tool-versions"}

// miseTools returns the tools pinned by mise or asdf at the repository root (names
// normalized: nodejs -> node, golang -> go, npm:pnpm -> pnpm) and the file(s) they
// came from.
func miseTools(root string) (map[string]bool, []string) {
	tools := map[string]bool{}
	var from []string
	add := func(name string) {
		name = strings.Trim(strings.TrimSpace(name), `"'`)
		if i := strings.LastIndex(name, ":"); i >= 0 { // npm:pnpm, core:node, aqua:...
			name = name[i+1:]
		}
		if i := strings.LastIndex(name, "/"); i >= 0 {
			name = name[i+1:]
		}
		switch name {
		case "nodejs":
			name = "node"
		case "golang":
			name = "go"
		}
		if name != "" {
			tools[name] = true
		}
	}
	for _, f := range miseConfigs {
		data, err := os.ReadFile(filepath.Join(root, f))
		if err != nil {
			continue
		}
		from = append(from, f)
		if f == ".tool-versions" {
			for _, l := range strings.Split(string(data), "\n") {
				if l = strings.TrimSpace(l); l != "" && !strings.HasPrefix(l, "#") {
					add(strings.Fields(l)[0])
				}
			}
			continue
		}
		section := ""
		for _, l := range strings.Split(string(data), "\n") {
			l = strings.TrimSpace(l)
			if l == "" || strings.HasPrefix(l, "#") {
				continue
			}
			if strings.HasPrefix(l, "[") {
				section = strings.Trim(l, "[] ")
				if t, ok := strings.CutPrefix(section, "tools."); ok { // [tools.node]
					add(t)
				}
				continue
			}
			if section == "tools" {
				if k, _, ok := strings.Cut(l, "="); ok {
					add(k)
				}
			}
		}
	}
	return tools, from
}

// nodeVersion picks the Node version for a suite: the nearest .nvmrc/.node-version
// walking up from the suite root, else volta.node / engines.node of the nearest
// package.json declaring one, else 22.
func nodeVersion(root, suiteRoot string) string {
	for _, d := range upDirs(suiteRoot) {
		for _, f := range []string{".nvmrc", ".node-version"} {
			if data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(joinRel(d, f)))); err == nil {
				if v := strings.TrimSpace(strings.SplitN(string(data), "\n", 2)[0]); v != "" {
					return strings.TrimPrefix(v, "v")
				}
			}
		}
	}
	for _, d := range upDirs(suiteRoot) {
		volta, engines := packageNode(root, joinRel(d, "package.json"))
		if volta != "" {
			return volta
		}
		if engines != "" {
			return engines
		}
	}
	return "22"
}

// packageNode reads volta.node and engines.node from a package.json.
func packageNode(root, rel string) (volta, engines string) {
	data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
	if err != nil {
		return "", ""
	}
	var pj struct {
		Engines map[string]string `json:"engines"`
		Volta   map[string]string `json:"volta"`
	}
	if json.Unmarshal(data, &pj) != nil {
		return "", ""
	}
	return strings.TrimSpace(pj.Volta["node"]), strings.TrimSpace(pj.Engines["node"])
}

// enginesNode returns the engines.node range of the nearest package.json declaring
// one, and that file.
func enginesNode(root, suiteRoot string) (rng, file string) {
	for _, d := range upDirs(suiteRoot) {
		if _, e := packageNode(root, joinRel(d, "package.json")); e != "" {
			return e, joinRel(d, "package.json")
		}
	}
	return "", ""
}

var requiresPythonRe = regexp.MustCompile(`(?m)^\s*requires-python\s*=`)

// pythonVersionFile returns the file actions/setup-python should read the Python
// version from: the nearest .python-version, else the nearest pyproject.toml with
// requires-python, else "".
func pythonVersionFile(root, suiteRoot string) string {
	for _, d := range upDirs(suiteRoot) {
		if f := joinRel(d, ".python-version"); fileExists(root, f) {
			return f
		}
	}
	for _, d := range upDirs(suiteRoot) {
		f := joinRel(d, "pyproject.toml")
		if data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(f))); err == nil && requiresPythonRe.Match(data) {
			return f
		}
	}
	return ""
}

// ---- workflow ----

// renderWorkflow writes a GitHub Actions workflow that sets up the toolchains the
// detected suites need, installs dependencies and runs smates. Tools pinned in
// mise.toml/.tool-versions are installed with mise-action; services and environment
// variables the tests appear to need are written as commented-out blocks.
func renderWorkflow(repo *gitx.Repo, suites []config.Suite, in inspection) string {
	has := func(name string) bool { return fileExists(repo.Root, name) }
	mise, miseFrom := miseTools(repo.Root)
	var setup, install []string
	seen := map[string]bool{}
	once := func(key string, lines ...string) {
		if !seen[key] {
			seen[key] = true
			setup = append(setup, lines...)
		}
	}
	usesMise := false
	// tool sets up a language unless mise pins it.
	tool := func(name, key string, lines ...string) {
		if mise[name] {
			usesMise = true
			return
		}
		once(key, lines...)
	}
	for _, s := range suites {
		root := s.Root
		switch s.Runner {
		case "go":
			tool("go", "go:"+root, "      - uses: "+actSetupGo, "        with:", "          go-version-file: "+yamlStr(joinRel(root, "go.mod")))
		case "pytest", "unittest":
			if f := pythonVersionFile(repo.Root, root); f != "" {
				tool("python", "python:"+f, "      - uses: "+actSetupPython, "        with:", "          python-version-file: "+yamlStr(f))
			} else if mise["python"] {
				usesMise = true
			}
			tool("uv", "uv", "      - uses: "+actSetupUV)
		case "bun":
			tool("bun", "bun", "      - uses: "+actSetupBun)
			if !seen["install:"+root] {
				seen["install:"+root] = true
				install = append(install, "      - run: bun install --frozen-lockfile", "        working-directory: "+yamlStr(root))
			}
		default: // vitest, jest, node
			cmd := "npm ci"
			for _, d := range upDirs(root) {
				switch {
				case has(joinRel(d, "pnpm-lock.yaml")):
					tool("pnpm", "pnpm", "      - uses: "+actSetupPnpm)
					cmd = "pnpm install --frozen-lockfile"
				case has(joinRel(d, "bun.lock")) || has(joinRel(d, "bun.lockb")):
					tool("bun", "bun", "      - uses: "+actSetupBun)
					cmd = "bun install --frozen-lockfile"
				case has(joinRel(d, "yarn.lock")):
					cmd = "yarn install --immutable"
				case has(joinRel(d, "package-lock.json")):
					cmd = "npm ci"
				default:
					if d != "." {
						continue
					}
				}
				if !seen["install:"+d] {
					seen["install:"+d] = true
					install = append(install, "      - run: "+cmd, "        working-directory: "+yamlStr(d))
				}
				break
			}
			tool("node", "node", "      - uses: "+actSetupNode, "        with:", "          node-version: "+strconv.Quote(nodeVersion(repo.Root, root)))
		}
	}
	var b strings.Builder
	b.WriteString(`# Runs only the tests a change can affect. Generated by "smates init --github".
name: tests
on:
  push:
    branches: [` + yamlFlowStr(defaultBranch(repo)) + `]
  pull_request:

jobs:
  test:
    runs-on: ubuntu-latest
`)
	renderServices(&b, in)
	b.WriteString("    steps:\n      - uses: " + actCheckout + "\n")
	if usesMise {
		b.WriteString("      - uses: " + actMise + "   # installs the tools pinned in " + strings.Join(miseFrom, ", ") + "\n")
	}
	for _, l := range setup {
		b.WriteString(l + "\n")
	}
	for _, l := range install {
		b.WriteString(l + "\n")
	}
	for _, r := range in.Local {
		fmt.Fprintf(&b, "      # %s calls %s: start that server before the tests, e.g.\n", r.File, r.URL)
		fmt.Fprintf(&b, "      # - run: (make serve &) && timeout 60 sh -c 'until curl -s localhost:%d >/dev/null; do sleep 1; done'\n", r.Port)
	}
	b.WriteString(`      # On pushes to the default branch every test runs and the recorded state is
      # refreshed; on pull requests only affected tests run.
      - uses: ` + actSmates + `
        with:
          anthropic-api-key: ${{ secrets.ANTHROPIC_API_KEY }}   # optional: narrows unattributed changes (or openai-api-key / openrouter-api-key)
`)
	return b.String()
}

// initWorkflow is what `smates init --github` did with the workflow (the "workflow"
// object of `smates init --format json`).
type initWorkflow struct {
	File    string `json:"file"`
	Written bool   `json:"written"`
	// Skipped says why File was not written.
	Skipped string `json:"skipped,omitempty"`
	// Patch replaces the test steps of existing workflows with the smates action
	// (written to PatchFile when --patch was given).
	Patch     string `json:"patch,omitempty"`
	PatchFile string `json:"patch_file,omitempty"`
}

// initGitHub is the --github part of init. When existing workflows already run the
// tests, it does not add a second workflow running them again (unless force): it
// proposes a patch replacing those steps with the smates action instead, printed or
// written to patchFile, and never edits the user's workflows itself.
func initGitHub(repo *gitx.Repo, suites []config.Suite, files []string, force bool, patchFile string, stdout, stderr io.Writer) (*initWorkflow, error) {
	in := inspect(repo.Root, suites, files)
	for _, h := range in.Services {
		fmt.Fprintf(stderr, "smates: the tests appear to need %s (%s, from %s): see the commented services:/env: blocks in the workflow\n", h.Name, h.Image, h.Source)
	}
	for _, r := range in.Local {
		fmt.Fprintf(stderr, "smates: %s calls %s: the workflow needs to start that server before the tests\n", r.File, r.URL)
	}
	for _, e := range in.Env {
		fmt.Fprintf(stderr, "smates: %s reads %s (listed in %s): set it in the workflow's env: (commented out)\n", e.ReadBy, e.Name, e.Source)
	}
	const wfRel = ".github/workflows/smates.yml"
	res := &initWorkflow{File: wfRel}
	wf := filepath.Join(repo.Root, filepath.FromSlash(wfRel))
	var steps []testStep
	for _, st := range workflowTestSteps(repo.Root) {
		if st.File != wfRel {
			steps = append(steps, st)
		}
	}
	// Workflows that already run smates, but only on pull requests, get the triggers
	// that record the state their runs start from.
	def := defaultBranch(repo)
	var seedOnly []string
	for _, g := range smatesSeedGaps(repo.Root, def) {
		seedOnly = append(seedOnly, g.File)
	}
	// A workflow already running smates (on pull requests only, say) would run the same
	// tests as a new one, under the same default cache-key.
	var running []string
	for _, f := range smatesWorkflows(repo.Root) {
		if f != wfRel {
			running = append(running, f)
		}
	}
	if len(steps) > 0 || len(seedOnly) > 0 {
		if len(steps) > 0 {
			fmt.Fprintln(stderr, "smates: existing workflows already run tests:")
		}
		duplicate := false
		for _, st := range steps {
			fmt.Fprintf(stderr, "  %s: job %q, step %q: %s\n", st.File, st.Job, st.Step, st.Command)
			names, why := stepPatch(st, suites)
			if why != "" {
				fmt.Fprintf(stderr, "    not patched: %s\n", why)
			}
			duplicate = duplicate || (!st.Other && len(names) > 0)
		}
		patch, notes := workflowPatch(repo.Root, steps, suites, def, seedOnly...)
		if duplicate || patch != "" {
			res.Patch = patch
		}
		for _, n := range notes {
			fmt.Fprintf(stderr, "  note: %s\n", n)
		}
		what := "adds the triggers that record the state to those workflows"
		if duplicate {
			what = "replaces those steps with the smates action"
		}
		switch {
		case patch == "" && duplicate:
			fmt.Fprintf(stderr, "  replace those steps with `uses: %s` (with `args: --suite <name>` and a distinct `cache-key` per job when the suites run in separate jobs)\n", actSmates)
		case patch == "":
		case patchFile != "":
			if err := os.WriteFile(patchFile, []byte(patch), 0o644); err != nil {
				return nil, asEnvError(err)
			}
			res.PatchFile = patchFile
			fmt.Fprintf(stderr, "  wrote %s: it %s; review it, then `git apply %s`\n", patchFile, what, patchFile)
		default:
			fmt.Fprintf(stderr, "  this patch %s (review it, then save it and `git apply` it, or rerun with --patch FILE):\n\n%s\n", what, patch)
		}
		if duplicate && !force {
			fmt.Fprintf(stderr, "smates: not writing %s, which would run the same tests a second time (use --force to write it anyway)\n", wfRel)
			res.Skipped = "existing workflows already run the tests: apply the patch instead (or rerun with --force)"
			return res, nil
		}
	}
	if len(running) > 0 && !force {
		fmt.Fprintf(stderr, "smates: not writing %s: %s already runs smates, and a second workflow would run the same tests again (use --force to write it anyway)\n", wfRel, strings.Join(running, ", "))
		res.Skipped = strings.Join(running, ", ") + " already runs smates (rerun with --force to write it anyway)"
		return res, nil
	}
	if _, err := os.Stat(wf); err == nil && !force {
		fmt.Fprintf(stderr, "smates: %s exists, not overwritten (use --force)\n", wfRel)
		res.Skipped = "it exists (rerun with --force to overwrite it)"
		return res, nil
	}
	if err := os.MkdirAll(filepath.Dir(wf), 0o755); err != nil {
		return nil, err
	}
	if err := os.WriteFile(wf, []byte(renderWorkflow(repo, suites, in)), 0o644); err != nil {
		return nil, err
	}
	res.Written = true
	fmt.Fprintln(stdout, "wrote "+wfRel+" (add an ANTHROPIC_API_KEY, OPENAI_API_KEY or OPENROUTER_API_KEY secret to enable the agent)")
	return res, nil
}

// ---- combined ----

// inspection is what init --github and doctor learn about the services and the
// environment the tests need.
type inspection struct {
	Services []serviceHint // compose services, and databases the code connects to on localhost
	Local    []localRef    // other servers the code calls on localhost
	Env      []envReq      // variables from .env.example the code reads
}

func inspect(root string, suites []config.Suite, files []string) inspection {
	var in inspection
	in.Services = composeServices(root)
	sc := scanSources(root, files)
	for _, r := range sc.Local {
		if r.Kind == "" {
			in.Local = append(in.Local, r)
			continue
		}
		if !slices.ContainsFunc(in.Services, func(h serviceHint) bool { return h.Kind == r.Kind }) {
			in.Services = append(in.Services, serviceHint{Kind: r.Kind, Name: r.Kind, Image: serviceDefaults[r.Kind].image, Source: r.File + " (" + r.URL + ")"})
		}
	}
	in.Env = envRequirements(root, suites, sc.EnvReads)
	return in
}

// serviceLines is the GitHub Actions services: entry for a service (indented relative
// to services:).
func serviceLines(h serviceHint) []string {
	d := serviceDefaults[h.Kind]
	out := []string{h.Name + ":", "  image: " + h.Image}
	if env := serviceEnv(h); len(env) > 0 {
		out = append(out, "  env:")
		for _, kv := range env {
			out = append(out, "    "+kv[0]+": "+yamlStr(kv[1]))
		}
	}
	out = append(out, fmt.Sprintf("  ports: [\"%d:%d\"]", d.port, d.port))
	health := d.health
	if h.Kind == "postgres" {
		health = "pg_isready -U " + envValue(serviceEnv(h), "POSTGRES_USER", "postgres")
	}
	return append(out, "  options: >-", "    --health-cmd "+strconv.Quote(health)+" --health-interval 5s --health-timeout 5s --health-retries 10")
}

// renderServices writes the commented services: and env: blocks of the test job.
func renderServices(b *strings.Builder, in inspection) {
	if len(in.Services) > 0 {
		var srcs []string
		for _, h := range in.Services {
			if !slices.Contains(srcs, h.Source) {
				srcs = append(srcs, h.Source)
			}
		}
		b.WriteString("    # Services the tests appear to need (from " + strings.Join(srcs, ", ") + ").\n")
		b.WriteString("    # Commented out on purpose: image tags and credentials are copied from the compose\n")
		b.WriteString("    # file or are the images' usual defaults, not verified. Check them, then uncomment.\n")
		b.WriteString("    # services:\n")
		for _, h := range in.Services {
			for _, l := range serviceLines(h) {
				b.WriteString("    #   " + l + "\n")
			}
		}
	}
	var env [][2]string
	have := map[string]bool{}
	for _, e := range in.Env {
		v := e.Example
		if secretNameRe.MatchString(e.Name) || v == "" {
			v = "${{ secrets." + e.Name + " }}"
		}
		env = append(env, [2]string{e.Name, v})
		have[e.Name] = true
	}
	for _, h := range in.Services {
		if k, v := serviceURL(h); k != "" && !have[k] {
			have[k] = true
			env = append(env, [2]string{k, v})
		}
	}
	if len(env) > 0 {
		var srcs []string
		for _, e := range in.Env {
			if !slices.Contains(srcs, e.Source) {
				srcs = append(srcs, e.Source)
			}
		}
		what := "Connection URLs for the services above"
		if len(srcs) > 0 {
			what = "Variables the code reads (listed in " + strings.Join(srcs, ", ") + ")"
			if len(in.Services) > 0 {
				what += " and service URLs"
			}
		}
		b.WriteString("    # " + what + ": point them at the CI services or secrets, then uncomment.\n")
		b.WriteString("    # env:\n")
		for _, kv := range env {
			v := kv[1]
			if !strings.HasPrefix(v, "${{") {
				v = yamlStr(v)
			}
			fmt.Fprintf(b, "    #   %s: %s\n", kv[0], v)
		}
	}
}

// yamlFlowStr is yamlStr for an item of a flow sequence ([a, b]), where a comma or a
// bracket also ends a plain scalar.
func yamlFlowStr(s string) string {
	if strings.ContainsAny(s, ",[]{}") {
		return strconv.Quote(s)
	}
	return yamlStr(s)
}

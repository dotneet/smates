package cli

// `smates doctor`: setup checks as structured results, printed as text or JSON.

import (
	"flag"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"time"

	"github.com/dotneet/smates/internal/agent"
	"github.com/dotneet/smates/internal/config"
	"github.com/dotneet/smates/internal/runner"
)

// Doctor check statuses.
const (
	checkOK   = "ok"
	checkWarn = "warn"
	checkFail = "fail"
)

const docURL = "https://github.com/dotneet/smates"

// doctorReport is the JSON schema of `smates doctor --format json`.
type doctorReport struct {
	Version string `json:"version"`
	// OK is false when a check failed (the exit code is then 1).
	OK     bool           `json:"ok"`
	Checks []*doctorCheck `json:"checks"`
}

// doctorCheck is one check. ID is stable (kebab-case); checks repeated per suite,
// service, variable or workflow step carry what they are about in Subject.
type doctorCheck struct {
	ID      string     `json:"id"`
	Status  string     `json:"status"` // ok | warn | fail
	Subject string     `json:"subject,omitempty"`
	Message string     `json:"message"`
	Fix     *doctorFix `json:"fix,omitempty"`
}

// doctorFix says how to resolve a warning or failure: a command to run, a smates.yml
// change (Key is a dotted path, e.g. "orphans" or "suites.web.depends_on"; top-level
// scalars can be set with `smates config set`), and/or documentation.
type doctorFix struct {
	Command string     `json:"command,omitempty"`
	Config  *configFix `json:"config,omitempty"`
	Doc     string     `json:"doc,omitempty"`
}

type configFix struct {
	Key   string `json:"key"`
	Value any    `json:"value"`
}

func (r *doctorReport) add(status, id, format string, a ...any) *doctorCheck {
	c := &doctorCheck{ID: id, Status: status, Message: fmt.Sprintf(format, a...)}
	r.Checks = append(r.Checks, c)
	if status == checkFail {
		r.OK = false
	}
	return c
}

func (c *doctorCheck) about(subject string) *doctorCheck {
	c.Subject = subject
	return c
}

func (c *doctorCheck) fix(f doctorFix) *doctorCheck {
	c.Fix = &f
	return c
}

// printText prints the report as `smates doctor` always has.
func (r *doctorReport) printText(w io.Writer) {
	fmt.Fprintf(w, "smates %s\n", r.Version)
	for _, c := range r.Checks {
		label := map[string]string{checkOK: "ok   ", checkWarn: "warn ", checkFail: "FAIL "}[c.Status]
		fmt.Fprintf(w, "  %s %s\n", label, c.Message)
	}
}

func cmdDoctor(args []string, stdout, stderr io.Writer) (int, error) {
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var cf commonFlags
	cf.register(fs)
	format := formatFlag(fs)
	if err := fs.Parse(args); err != nil {
		return 2, usageError{err}
	}
	if err := checkFormat(*format); err != nil {
		return 2, err
	}
	r := runDoctor(&cf)
	if *format == "json" {
		if err := writeJSON(stdout, r); err != nil {
			return 1, err
		}
	} else {
		r.printText(stdout)
	}
	if !r.OK {
		return 1, nil
	}
	return 0, nil
}

func runDoctor(cf *commonFlags) *doctorReport {
	r := &doctorReport{Version: Version, OK: true, Checks: []*doctorCheck{}}
	if _, err := exec.LookPath("git"); err != nil {
		r.add(checkFail, "git-installed", "git not found on PATH").fix(doctorFix{Doc: "https://git-scm.com/downloads"})
		return r
	}
	p, err := load(cf, io.Discard)
	if err != nil {
		r.add(checkFail, "project-load", "%v", err).fix(doctorFix{Doc: docURL + "#configuration-smatesyml"})
		return r
	}
	r.add(checkOK, "repository", "repository %s", p.repo.Root)
	if p.cfg.Path != "" {
		r.add(checkOK, "config", "config %s", p.cfg.Path)
	} else {
		r.add(checkWarn, "config", "no smates.yml: using auto-detected suites (run `smates init` to pin them)").fix(doctorFix{Command: "smates init"})
	}
	if len(p.suites) == 0 {
		r.add(checkFail, "suites-detected", "no test suites detected").fix(doctorFix{Command: "smates init", Doc: docURL + "#configuration-smatesyml"})
	}
	runners := map[string]bool{}
	for _, s := range p.suites {
		units := s.Adapter.Units(s, s.TestFiles(p.files))
		if len(units) == 0 {
			r.add(checkWarn, "suite-units", "suite %s (%s): no test files match %v", s.Name, s.Runner, s.Tests).about(s.Name).
				fix(doctorFix{Config: &configFix{Key: "suites." + s.Name + ".tests", Value: []string{"<glob relative to " + s.Root + ">"}}, Doc: docURL + "#configuration-smatesyml"})
		} else {
			r.add(checkOK, "suite-units", "suite %s (%s, root %s): %d test units; command: %s", s.Name, s.Runner, s.Root, len(units), s.Command).about(s.Name)
		}
		if why := serverSuiteHint(p.repo.Root, s, p.files); why != "" {
			r.add(checkWarn, "suite-server-dependency", "suite %s looks like end-to-end tests (%s) but declares no depends_on, serves or rules: tracing cannot follow requests into a server process, so changes to the server's code select none of its tests. Declare it, e.g. depends_on: [<server suite>] or serves: [\"backend/**\"]", s.Name, why).about(s.Name).
				fix(doctorFix{Config: &configFix{Key: "suites." + s.Name + ".depends_on", Value: []string{"<server suite>"}}, Doc: docURL + "#tests-that-exercise-a-server"})
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
		const id = "toolchain-node"
		nodeDoc := doctorFix{Doc: "https://nodejs.org/en/download"}
		if v := tool("node", "--version"); v == "" {
			r.add(checkFail, id, "node not found on PATH").fix(nodeDoc)
		} else if strings.HasPrefix(v, "v1") && v < "v18" {
			r.add(checkFail, id, "node %s is too old (tracing needs Node 18+)", v).fix(nodeDoc)
		} else {
			r.add(checkOK, id, "node %s", v)
		}
	}
	if runners["bun"] {
		if v := tool("bun", "--version"); v == "" {
			r.add(checkFail, "toolchain-bun", "bun not found on PATH").fix(doctorFix{Doc: "https://bun.sh/docs/installation"})
		} else {
			r.add(checkOK, "toolchain-bun", "bun %s", v)
		}
	}
	if runners["go"] {
		if v := tool("go", "version"); v == "" {
			r.add(checkFail, "toolchain-go", "go not found on PATH").fix(doctorFix{Doc: "https://go.dev/doc/install"})
		} else {
			r.add(checkOK, "toolchain-go", "%s", v)
		}
	}
	if runners["pytest"] || runners["unittest"] {
		py := tool("python3", "-c", "import sys;print('%d.%d'%sys.version_info[:2])")
		if py == "" {
			r.add(checkWarn, "toolchain-python", "python3 not found on PATH (fine if the suite command provides Python, e.g. uv)").fix(doctorFix{Doc: "https://docs.astral.sh/uv/"})
		} else if py < "3.12" && len(py) == 4 {
			r.add(checkWarn, "toolchain-python", "python %s: tracing uses sys.setprofile (slower); Python 3.12+ uses sys.monitoring", py)
		} else {
			r.add(checkOK, "toolchain-python", "python %s", py)
		}
	}
	doctorProject(p, func() string { return tool("node", "--version") }, r)
	if p.state.Empty() {
		r.add(checkWarn, "state-recorded", "no recorded state at %s: the first `smates run --all` (or a run on the default branch in CI) records it", p.statePath).
			fix(doctorFix{Command: "smates run --all", Doc: docURL + "#quick-start"})
	} else {
		n := 0
		for _, s := range p.state.Suites {
			n += len(s.Tests)
		}
		if p.state.Created.IsZero() {
			// Shared state without a snapshot of this worktree yet.
			r.add(checkOK, "state-recorded", "state %s: %d tests recorded, no snapshot in this worktree yet (the next run records one)", p.statePath, n)
		} else {
			age := time.Since(p.state.Created).Round(time.Minute)
			r.add(checkOK, "state-recorded", "state %s: %d tests recorded, snapshot from commit %.12s (%s ago)", p.statePath, n, p.state.Commit, age)
		}
	}
	if okA, why := agent.Available(p.cfg.Agent); okA {
		r.add(checkOK, "agent", "agent: %s available for unattributed changes (orphans: %s)", agent.Describe(p.cfg.Agent), p.cfg.Orphans)
	} else if p.cfg.Orphans == config.OrphansAgent {
		r.add(checkWarn, "agent", "agent unavailable (%s): unattributed changes run the affected suites in full", why).
			fix(doctorFix{Command: "smates config set orphans all", Config: &configFix{Key: "orphans", Value: config.OrphansAll}, Doc: docURL + "#the-agent"})
	}
	if ci := detectCI(); ci.Name != "" {
		full, b := fullRunBranch(ci, p.repo, p.cfg.FullRunBranches)
		r.add(checkOK, "ci", "CI %s, branch %q, pull request %t, full run by default: %t", ci.Name, b, ci.PullRequest, full)
	}
	return r
}

// installCommand is the command installing the JS dependencies of a suite rooted at
// suiteRoot, run from the directory of the nearest lockfile.
func installCommand(root, suiteRoot string) string {
	for _, d := range upDirs(suiteRoot) {
		cmd := ""
		switch {
		case fileExists(root, joinRel(d, "pnpm-lock.yaml")):
			cmd = "pnpm install --frozen-lockfile"
		case fileExists(root, joinRel(d, "bun.lock")) || fileExists(root, joinRel(d, "bun.lockb")):
			cmd = "bun install"
		case fileExists(root, joinRel(d, "yarn.lock")):
			cmd = "yarn install"
		case fileExists(root, joinRel(d, "package-lock.json")):
			cmd = "npm ci"
		default:
			continue
		}
		if d != "." {
			cmd = "cd " + shellQuote(d) + " && " + cmd
		}
		return cmd
	}
	if suiteRoot != "." && suiteRoot != "" {
		return "cd " + shellQuote(suiteRoot) + " && npm install"
	}
	return "npm install"
}

// doctorProject checks what the suites need beyond their runners: installed JS
// dependencies, no implicit npx/bunx downloads, the engines.node range, services and
// environment variables, and workflows that run the tests without smates.
func doctorProject(p *project, nodeVersion func() string, r *doctorReport) {
	var cs []config.Suite
	nodeV := ""
	for _, s := range p.suites {
		cs = append(cs, s.Suite)
		js := s.Runner == "vitest" || s.Runner == "jest" || s.Runner == "node" || s.Runner == "bun"
		if !js {
			continue
		}
		if (s.Runner == "vitest" || s.Runner == "jest") && !runner.HasNodeModules(p.repo.Root, s.Root) {
			r.add(checkFail, "suite-dependencies-installed", "suite %s: no node_modules (or Yarn PnP loader) from %s up to the repository root: install the dependencies first (npm ci / pnpm install / yarn / bun install)", s.Name, s.Root).about(s.Name).
				fix(doctorFix{Command: installCommand(p.repo.Root, s.Root)})
		} else if hard, err := runner.LauncherProblem(p.repo.Root, s); err != nil && hard {
			r.add(checkFail, "suite-launcher", "suite %s: %v", s.Name, err).about(s.Name).fix(doctorFix{Command: installCommand(p.repo.Root, s.Root)})
		} else if err != nil {
			r.add(checkWarn, "suite-launcher", "suite %s: %v", s.Name, err).about(s.Name).fix(doctorFix{Command: installCommand(p.repo.Root, s.Root)})
		}
		if s.Runner == "bun" {
			continue
		}
		rng, file := enginesNode(p.repo.Root, s.Root)
		if rng == "" {
			continue
		}
		if nodeV == "" {
			if nodeV = nodeVersion(); nodeV == "" {
				continue
			}
		}
		if sat, known := nodeSatisfies(nodeV, rng); known && !sat {
			r.add(checkWarn, "suite-engines-node", "suite %s: node %s does not satisfy engines.node %q in %s", s.Name, nodeV, rng, file).about(s.Name).
				fix(doctorFix{Doc: "install a Node version matching " + rng + " (e.g. with nvm, fnm or mise)"})
		} else if known {
			r.add(checkOK, "suite-engines-node", "suite %s: node %s satisfies engines.node %q (%s)", s.Name, nodeV, rng, file).about(s.Name)
		}
	}
	in := inspect(p.repo.Root, cs, p.files)
	for _, h := range in.Services {
		port := serviceDefaults[h.Kind].port
		if reachable(port) {
			r.add(checkOK, "service-reachable", "service %s (%s, from %s): localhost:%d is reachable", h.Name, h.Image, h.Source, port).about(h.Name)
		} else {
			f := doctorFix{Command: "docker run -d -p " + fmt.Sprintf("%d:%d", port, port) + " " + h.Image, Doc: docURL + "#github-actions"}
			if strings.HasPrefix(h.Source, "docker-compose") || strings.HasPrefix(h.Source, "compose") {
				f.Command = "docker compose up -d " + h.Name
			}
			r.add(checkWarn, "service-reachable", "service %s (%s, from %s): nothing listens on localhost:%d; start it before the tests (e.g. docker compose up -d %s); in CI add a services: block (the workflow `smates init --github` generates has one, commented out)", h.Name, h.Image, h.Source, port, h.Name).about(h.Name).fix(f)
		}
	}
	for _, l := range in.Local {
		if reachable(l.Port) {
			r.add(checkOK, "local-server-reachable", "%s calls %s: reachable", l.File, l.URL).about(l.URL)
		} else {
			r.add(checkWarn, "local-server-reachable", "%s calls %s, but nothing listens on port %d: tests using it need that server running", l.File, l.URL, l.Port).about(l.URL).
				fix(doctorFix{Doc: docURL + "#tests-that-exercise-a-server"})
		}
	}
	for _, e := range in.Env {
		set := false
		for _, s := range p.suites {
			if _, found := s.LookupEnv(e.Name); found {
				set = true
			}
		}
		if !set {
			r.add(checkWarn, "env-var-set", "%s is not set: %s reads it and %s lists it (set it in the shell or in the suite's env: in smates.yml)", e.Name, e.ReadBy, e.Source).about(e.Name).
				fix(doctorFix{Config: &configFix{Key: "suites." + suiteOf(cs, e.ReadBy) + ".env." + e.Name, Value: e.Example}})
		}
	}
	for _, st := range workflowTestSteps(p.repo.Root) {
		subject := st.File + ":" + st.Job + ":" + st.Step
		if st.Skip != "" {
			r.add(checkWarn, "workflow-test-step", "%s: job %q, step %q runs tests without smates (%s); %s", st.File, st.Job, st.Step, st.Command, st.Skip).about(subject).
				fix(doctorFix{Doc: docURL + "#github-actions"})
			continue
		}
		r.add(checkWarn, "workflow-test-step", "%s: job %q, step %q runs the tests without smates (%s): `smates init --github --patch smates.patch` writes a patch replacing it with `uses: %s`", st.File, st.Job, st.Step, st.Command, actSmates).about(subject).
			fix(doctorFix{Command: "smates init --github --patch smates.patch", Doc: docURL + "#github-actions"})
	}
}

// suiteOf names the suite whose root holds file (the deepest), or the first suite.
func suiteOf(suites []config.Suite, file string) string {
	best, depth := "", -1
	for _, s := range suites {
		if s.Root == "." || s.Root == "" || file == s.Root || strings.HasPrefix(file, s.Root+"/") {
			d := len(s.Root)
			if s.Root == "." {
				d = 0
			}
			if d > depth {
				best, depth = s.Name, d
			}
		}
	}
	if best == "" && len(suites) > 0 {
		best = suites[0].Name
	}
	return best
}

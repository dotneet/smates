package cli

// `smates init --format json`: what init decided, and the items that need a person's
// (or an agent's) decision, each with its evidence and a recommended action.

import (
	"fmt"
	"slices"
	"strings"

	"github.com/dotneet/smates/internal/agent"
	"github.com/dotneet/smates/internal/config"
	"github.com/dotneet/smates/internal/gitx"
	"github.com/dotneet/smates/internal/runner"
)

// initReport is the JSON schema of `smates init --format json`.
type initReport struct {
	// Config is the config file; Written is false with --stdout, which puts the
	// document in ConfigYAML instead.
	Config     string `json:"config"`
	Written    bool   `json:"written"`
	ConfigYAML string `json:"config_yaml,omitempty"`
	// FilesWritten: smates.yml, .gitignore (when .smates/ was added), the workflow and
	// the patch file of --github --patch.
	FilesWritten []string    `json:"files_written"`
	Suites       []initSuite `json:"suites"`
	// Workflow: what --github did (absent without --github).
	Workflow    *initWorkflow `json:"workflow,omitempty"`
	NeedsReview []reviewItem  `json:"needs_review"`
	NextSteps   []string      `json:"next_steps"`
}

type initSuite struct {
	Name    string   `json:"name"`
	Runner  string   `json:"runner"`
	Root    string   `json:"root"`
	Exclude []string `json:"exclude"`
}

// reviewItem is something init could not decide: a service or variable the tests
// appear to need, a Makefile command whose flags smates would drop, end-to-end tests
// without a declared server, ...
type reviewItem struct {
	// ID is stable for the same finding: "<kind>-<subject>" in kebab-case.
	ID string `json:"id"`
	// Kind: service | env | local-server | makefile-command | server-dependency |
	// workflow-test-step | orphans-policy | no-suites.
	Kind     string       `json:"kind"`
	Reason   string       `json:"reason"`
	Evidence []string     `json:"evidence"`
	Action   reviewAction `json:"action"`
}

// reviewAction is the recommended action: a description, and whichever apply of a
// command to run, a smates.yml change (dotted key; top-level scalars can be set with
// `smates config set`) and a snippet to paste into SnippetFile.
type reviewAction struct {
	Description string     `json:"description"`
	Command     string     `json:"command,omitempty"`
	Config      *configFix `json:"config,omitempty"`
	Snippet     string     `json:"snippet,omitempty"`
	SnippetFile string     `json:"snippet_file,omitempty"`
}

var initNextSteps = []string{
	"smates run --all",
	"smates run",
	"smates why <file>",
	"smates hook install",
}

// slug turns s into a kebab-case ID part.
func slug(s string) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(s) {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			b.WriteRune(r)
			dash = false
		} else if !dash && b.Len() > 0 {
			b.WriteByte('-')
			dash = true
		}
	}
	return strings.TrimSuffix(b.String(), "-")
}

// initReview collects the items of init that need a decision. existing is the config
// init --github runs over (nil for a fresh init, judged with the defaults it writes):
// its orphans policy, agent settings and suite env count.
func initReview(repo *gitx.Repo, existing *config.Config, suites []config.Suite, files []string, mk []makeHint, github bool) []reviewItem {
	out := []reviewItem{}
	cfg := existing
	if cfg == nil {
		cfg = &config.Config{}
		_ = cfg.Normalize()
	}
	if len(suites) == 0 {
		out = append(out, reviewItem{
			ID: "no-suites", Kind: "no-suites", Reason: "no test suite was detected", Evidence: []string{},
			Action: reviewAction{Description: "add the suites to smates.yml by hand (name, runner, root, command)", Snippet: "suites:\n  - name: tests\n    runner: custom\n    root: .\n    command: <test command>\n", SnippetFile: "smates.yml"},
		})
	}
	for _, h := range mk {
		snippet := "command: " + yamlStr(h.Command) + "\n"
		if len(h.Env) > 0 {
			snippet += "env:\n"
			for _, kv := range h.Env {
				snippet += "  " + kv[0] + ": " + yamlStr(kv[1]) + "\n"
			}
		}
		out = append(out, reviewItem{
			ID: "makefile-command-" + slug(h.Suite), Kind: "makefile-command",
			Reason:   fmt.Sprintf("%s target %q runs the %s suite's runner with flags or variables smates' own command does not pass", h.File, h.Target, h.Suite),
			Evidence: []string{fmt.Sprintf("%s: %s: %s", h.File, h.Target, h.Line)},
			Action: reviewAction{
				Description: "to keep those flags/variables, set the suite's command (and env) in smates.yml; otherwise leave it",
				Config:      &configFix{Key: "suites." + h.Suite + ".command", Value: h.Command},
				Snippet:     snippet, SnippetFile: "smates.yml",
			},
		})
	}
	if rs, err := runner.Resolve(repo, suites); err == nil {
		for _, s := range rs {
			if why := serverSuiteHint(repo.Root, s, files); why != "" {
				out = append(out, reviewItem{
					ID: "server-dependency-" + slug(s.Name), Kind: "server-dependency",
					Reason:   fmt.Sprintf("suite %s looks like end-to-end tests of a server in another process; tracing cannot follow requests into it, so changes to the server's code would select none of its tests", s.Name),
					Evidence: []string{why},
					Action: reviewAction{
						Description: "declare the server's code: depends_on: [<server suite>] or serves: [<globs of the server's code>]",
						Config:      &configFix{Key: "suites." + s.Name + ".depends_on", Value: []string{"<server suite>"}},
						Snippet:     "depends_on: [<server suite>]   # or: serves: [\"backend/**\"]\n", SnippetFile: "smates.yml",
					},
				})
			}
		}
	}
	in := inspect(repo.Root, suites, files)
	for _, h := range in.Services {
		reason := fmt.Sprintf("%s service %s found in %s", h.Kind, h.Name, h.Source)
		cmd := "docker compose up -d " + h.Name
		if !strings.HasPrefix(h.Source, "docker-compose") && !strings.HasPrefix(h.Source, "compose") {
			reason = fmt.Sprintf("the code connects to %s on localhost: %s", h.Kind, h.Source)
			port := serviceDefaults[h.Kind].port
			cmd = fmt.Sprintf("docker run -d -p %d:%d %s", port, port, h.Image)
		}
		var b strings.Builder
		b.WriteString("services:\n")
		for _, l := range serviceLines(h) {
			b.WriteString("  " + l + "\n")
		}
		if k, v := serviceURL(h); k != "" {
			b.WriteString("env:\n  " + k + ": " + yamlStr(v) + "\n")
		}
		out = append(out, reviewItem{
			ID: "service-" + slug(h.Name), Kind: "service", Reason: reason, Evidence: []string{h.Source},
			Action: reviewAction{
				Description: "start it before the tests locally; in CI add this to the test job (image tag and credentials are copied from the compose file or are the image's defaults: check them)",
				Command:     cmd, Snippet: b.String(), SnippetFile: ".github/workflows/smates.yml",
			},
		})
	}
	for _, l := range in.Local {
		out = append(out, reviewItem{
			ID: fmt.Sprintf("local-server-%d", l.Port), Kind: "local-server",
			Reason:   fmt.Sprintf("%s calls %s: tests using it need that server running", l.File, l.URL),
			Evidence: []string{l.File + ": " + l.URL},
			Action: reviewAction{
				Description: "start that server before the tests; when its code is in this repository, declare it so changes to it select these tests",
				Config:      &configFix{Key: "suites." + suiteOf(suites, l.File) + ".depends_on", Value: []string{"<server suite>"}},
			},
		})
	}
	for _, e := range in.Env {
		if slices.ContainsFunc(suites, func(s config.Suite) bool { _, ok := s.Env[e.Name]; return ok }) {
			continue // a suite's env: sets it
		}
		act := reviewAction{
			Description: "set it in the shell, in the suite's env: in smates.yml, or in the CI workflow's env:",
			Config:      &configFix{Key: "suites." + suiteOf(suites, e.ReadBy) + ".env." + e.Name, Value: e.Example},
		}
		if secretNameRe.MatchString(e.Name) || e.Example == "" {
			act.Description = "looks like a secret: set it in the shell locally and from a CI secret in the workflow's env:, not in smates.yml"
			act.Config = nil
			act.Snippet = "env:\n  " + e.Name + ": ${{ secrets." + e.Name + " }}\n"
			act.SnippetFile = ".github/workflows/smates.yml"
		}
		out = append(out, reviewItem{
			ID: "env-" + slug(e.Name), Kind: "env",
			Reason:   fmt.Sprintf("%s reads %s, which %s lists", e.ReadBy, e.Name, e.Source),
			Evidence: []string{e.ReadBy, e.Source},
			Action:   act,
		})
	}
	for _, st := range workflowTestSteps(repo.Root) {
		if st.File == ".github/workflows/smates.yml" {
			continue
		}
		it := reviewItem{
			ID: "workflow-test-step-" + slug(st.File+" "+st.Job+" "+st.Step), Kind: "workflow-test-step",
			Reason:   fmt.Sprintf("%s: job %q, step %q runs the tests without smates", st.File, st.Job, st.Step),
			Evidence: []string{fmt.Sprintf("%s:%d: %s", st.File, st.Line, st.Command)},
			Action: reviewAction{
				Description: "replace the step with `uses: " + actSmates + "`: write a patch doing it (smates.yml stays as it is), review it, then git apply it",
				Command:     "smates init --github --patch smates.patch",
			},
		}
		if github {
			it.Action.Description = "replace the step with `uses: " + actSmates + "`: review workflow.patch (or the --patch file), then git apply it"
			it.Action.Command = ""
		}
		if st.Skip != "" {
			it.Action = reviewAction{Description: "not patched automatically: " + st.Skip}
		}
		out = append(out, it)
	}
	if cfg.Orphans == config.OrphansAgent {
		if ok, why := agent.Available(cfg.Agent); !ok {
			out = append(out, reviewItem{
				ID: "orphans-policy", Kind: "orphans-policy",
				Reason:   "orphans: agent asks an LLM about changes no test is known to depend on, but no agent is available (" + why + "): such changes run the suites they may affect in full",
				Evidence: []string{why},
				Action: reviewAction{
					Description: "keep orphans: agent and set ANTHROPIC_API_KEY, OPENAI_API_KEY or OPENROUTER_API_KEY (locally and as a CI secret), or choose all explicitly",
					Command:     "smates config set orphans all",
					Config:      &configFix{Key: "orphans", Value: config.OrphansAll},
				},
			})
		}
	}
	return out
}

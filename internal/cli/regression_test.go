package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/dotneet/smates/internal/config"
	"github.com/dotneet/smates/internal/gitx"
	"github.com/dotneet/smates/internal/selector"
	"github.com/dotneet/smates/internal/state"
	"github.com/dotneet/smates/internal/testutil"
	"gopkg.in/yaml.v3"
)

// An agent answer naming only tests that do not exist is an agent failure: the orphan
// policy runs the affected suite, and the answer is not cached.
func TestAgentUnknownTestsFallBackToPolicy(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses sh")
	}
	clearCI(t)
	noAgentCredentials(t)
	t.Setenv("GITHUB_OUTPUT", "")
	t.Setenv("GITHUB_STEP_SUMMARY", "")
	g := newGitRepo(t)
	script := filepath.Join(t.TempDir(), "agent.sh")
	if err := os.WriteFile(script, []byte(`#!/bin/sh
cat >/dev/null
echo '{"decision":"tests","tests":[{"suite":"a","test":"ghost_check.sh","why":"reads data.cfg"}],"no_impact":[],"summary":"data.cfg affects ghost"}'
`), 0o755); err != nil {
		t.Fatal(err)
	}
	testutil.Write(t, g.dir, "smates.yml", `version: 1
orphans: agent
agent:
  command: `+script+`
suites:
  - name: a
    runner: custom
    root: a
    tests: ["*_check.sh"]
    command: sh -c 'for f; do sh "$f" || exit 1; done' sh
    rules: [{tests: ["*"], deps: ["src/a/**"]}]
`)
	testutil.Write(t, g.dir, ".gitignore", ".smates/\n")
	testutil.Write(t, g.dir, "a/x_check.sh", "true\n")
	testutil.Write(t, g.dir, "a/y_check.sh", "true\n")
	testutil.Write(t, g.dir, "src/a/lib", "1")
	g.git("add", "-A")
	g.commit("fixture")
	var out, errb bytes.Buffer
	if code := Main([]string{"run", "-C", g.dir, "--all"}, &out, &errb); code != 0 {
		t.Fatalf("run --all: %d %s", code, errb.String())
	}
	testutil.Write(t, g.dir, "a/data.cfg", "x")

	out.Reset()
	errb.Reset()
	if code := Main([]string{"select", "-C", g.dir, "--format", "json"}, &out, &errb); code != 0 {
		t.Fatalf("select: %d %s", code, errb.String())
	}
	var js callerSelection
	if err := json.Unmarshal(out.Bytes(), &js); err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	if got := js.Tests["a"]; !slices.Equal(got, []string{"a/x_check.sh", "a/y_check.sh"}) {
		t.Errorf("selected %v, want the whole suite", got)
	}
	if js.Handling["a/data.cfg"] != selector.OrphanPolicy || !strings.Contains(js.AgentNote, "agent failed") || !strings.Contains(js.AgentNote, "ghost_check.sh") {
		t.Errorf("handling %v, note %q", js.Handling, js.AgentNote)
	}
	st, err := state.Load(filepath.Join(g.dir, ".smates", "state.json.gz"))
	if err != nil {
		t.Fatal(err)
	}
	if len(st.AgentCache) != 0 {
		t.Errorf("the rejected answer was cached: %v", st.AgentCache)
	}
}

// Paths and the branch written into the generated workflow are quoted where YAML
// needs it: " #" would start a comment, a comma would split a flow sequence item.
func TestRenderWorkflowQuotesPaths(t *testing.T) {
	dir := treeDir(t, map[string]string{
		"py #2/.python-version":    "3.12\n",
		"web #3/package-lock.json": "{}",
	})
	suites := []config.Suite{
		{Name: "go", Runner: "go", Root: "svc #1"},
		{Name: "py", Runner: "pytest", Root: "py #2"},
		{Name: "web", Runner: "vitest", Root: "web #3"},
	}
	out := renderWorkflow(&gitx.Repo{Root: dir}, suites, inspect(dir, suites, nil))
	var doc struct {
		Jobs map[string]struct {
			Steps []struct {
				With map[string]string `yaml:"with"`
				Dir  string            `yaml:"working-directory"`
			} `yaml:"steps"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("workflow does not parse: %v\n%s", err, out)
	}
	got := map[string]string{}
	for _, st := range doc.Jobs["test"].Steps {
		for k, v := range st.With {
			got[k] = v
		}
		if st.Dir != "" {
			got["working-directory"] = st.Dir
		}
	}
	for k, want := range map[string]string{"go-version-file": "svc #1/go.mod", "python-version-file": "py #2/.python-version", "working-directory": "web #3"} {
		if got[k] != want {
			t.Errorf("%s = %q, want %q\n%s", k, got[k], want, out)
		}
	}
	for _, b := range []string{"main", "release,1", "x[y]", "a #b"} {
		var v []string
		if err := yaml.Unmarshal([]byte("["+yamlFlowStr(b)+"]"), &v); err != nil || !slices.Equal(v, []string{b}) {
			t.Errorf("branch %q in a flow sequence reads as %q (%v)", b, v, err)
		}
	}
}

// A last line without a newline is marked, so that git apply accepts the patch.
func TestUnifiedDiffNoFinalNewline(t *testing.T) {
	want := "--- a/f\n+++ b/f\n@@ -1,2 +1,2 @@\n a\n-b\n\\ No newline at end of file\n+c\n\\ No newline at end of file\n"
	if got := unifiedDiff("f", "a\nb", "a\nc"); got != want {
		t.Errorf("got\n%s\nwant\n%s", got, want)
	}
	want = "--- a/f\n+++ b/f\n@@ -1,2 +1,2 @@\n a\n-b\n\\ No newline at end of file\n+b\n"
	if got := unifiedDiff("f", "a\nb", "a\nb\n"); got != want {
		t.Errorf("adding the newline: got\n%s\nwant\n%s", got, want)
	}
}

// The workflow patch applies to a workflow without a final newline and keeps CRLF
// line endings.
func TestWorkflowPatchLineEndings(t *testing.T) {
	const wf = "on: push\njobs:\n  t:\n    runs-on: ubuntu-latest\n    steps:\n      - uses: actions/checkout@v4\n      - name: Go tests\n        run: go test ./..."
	for name, content := range map[string]string{"no final newline": wf, "crlf": strings.ReplaceAll(wf, "\n", "\r\n") + "\r\n"} {
		t.Run(name, func(t *testing.T) {
			g := newGitRepo(t)
			writeTree(t, g.dir, map[string]string{".github/workflows/ci.yml": content})
			g.git("add", ".")
			g.commit("ci")
			patch, _ := workflowPatch(g.dir, workflowTestSteps(g.dir), []config.Suite{{Name: "go", Runner: "go", Root: "."}}, "main")
			if patch == "" {
				t.Fatal("no patch")
			}
			pf := filepath.Join(t.TempDir(), "p.patch")
			if err := os.WriteFile(pf, []byte(patch), 0o644); err != nil {
				t.Fatal(err)
			}
			g.git("apply", "--check", pf)
			g.git("apply", pf)
			data := readFile(t, filepath.Join(g.dir, ".github/workflows/ci.yml"))
			if !strings.Contains(data, "uses: dotneet/smates@v1") {
				t.Fatalf("not patched:\n%s", data)
			}
			if name == "crlf" && strings.Count(data, "\n") != strings.Count(data, "\r\n") {
				t.Errorf("mixed line endings:\n%q", data)
			}
		})
	}
}

// Text from the agent cannot mention people or add links and images to the PR comment.
func TestHintsMarkdownMentions(t *testing.T) {
	got := mdEscape("ping @octocat and @org/team, see [docs](https://evil.example) ![x](https://evil.example/t.png) | a\nb `c`")
	want := `ping @` + "​" + `octocat and @` + "​" + `org/team, see \[docs\](https://evil.example) \!\[x\](https://evil.example/t.png) \| a b 'c'`
	if got != want {
		t.Errorf("agent explanation keeps mentions/links in the PR comment: %s", got)
	}
}

// Setup errors exit 4 (environment) or 3 (configuration) in every command, a test
// that is not a unit 3, and init over an existing config 2 (usage).
func TestSetupErrorExitCodes(t *testing.T) {
	clearCI(t)
	noAgentCredentials(t)
	t.Setenv("SMATES_SKIP_HOOK", "")
	main := func(args ...string) (int, string) {
		var out, errb bytes.Buffer
		code := Main(args, &out, &errb)
		return code, errb.String()
	}
	plain := t.TempDir()
	bad := newGitRepo(t)
	testutil.Write(t, bad.dir, "smates.yml", "version: 1\nbogus: [\n")
	for _, c := range []struct {
		dir  string
		want int
	}{{plain, ExitEnv}, {bad.dir, ExitConfig}} {
		for _, args := range [][]string{
			{"init", "-C", c.dir},
			{"stats", "-C", c.dir},
			{"state", "show", "-C", c.dir},
			{"state", "path", "-C", c.dir},
			{"state", "push", "-C", c.dir},
			{"state", "pull", "-C", c.dir},
		} {
			if code, errs := main(args...); code != c.want {
				t.Errorf("%v: exit %d, want %d\n%s", args, code, c.want, errs)
			}
		}
	}
	for _, args := range [][]string{
		{"hook", "install", "-C", plain},
		{"hook", "status", "-C", plain},
		{"hook", "install", "--claude", "-C", plain},
		{"hook", "status", "--claude", "-C", plain},
		{"hook", "run", "pre-push", "--", "-C", plain},
	} {
		if code, errs := main(args...); code != ExitEnv {
			t.Errorf("%v: exit %d, want %d\n%s", args, code, ExitEnv, errs)
		}
	}
	g := worktreeFixture(t, "")
	if code, errs := main("deps", "-C", g.dir, "t/nope_check.py"); code != ExitConfig || !strings.Contains(errs, "not a test unit") {
		t.Errorf("deps of an unknown test: exit %d\n%s", code, errs)
	}
	if code, errs := main("init", "-C", g.dir); code != ExitUsage || !strings.Contains(errs, "already exists") {
		t.Errorf("init over a config: exit %d\n%s", code, errs)
	}
}

// An unknown --format is rejected before selecting, which can call the agent, write
// --markdown and save the state.
func TestSelectFormatCheckedFirst(t *testing.T) {
	g := worktreeFixture(t, "")
	md := filepath.Join(t.TempDir(), "summary.md")
	var out, errb bytes.Buffer
	if code := Main([]string{"select", "-C", g.dir, "--format", "bogus", "--markdown", md}, &out, &errb); code != ExitUsage || !strings.Contains(errb.String(), `unknown format "bogus"`) {
		t.Fatalf("exit %d\n%s", code, errb.String())
	}
	if _, err := os.Stat(md); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("--markdown written before the format was checked: %v", err)
	}
}

// state push always pushes a gzip blob (its name says .gz), and state pull saves the
// state in the local file's format.
func TestStatePushPullFormats(t *testing.T) {
	g := worktreeFixture(t, "state: .smates/state.json\n")
	bare := t.TempDir()
	g.git("init", "-q", "--bare", bare)
	g.git("remote", "add", "origin", bare)
	g.git("push", "-q", "origin", "main")
	mustRun(t, "run", "-C", g.dir, "--no-agent", "--all")
	local := filepath.Join(g.dir, ".smates", "state.json")
	if data := readFile(t, local); isGzip([]byte(data)) {
		t.Fatal("the plain state is compressed")
	}
	mustRun(t, "state", "push", "-C", g.dir)
	cmd := exec.Command("git", "cat-file", "blob", "refs/smates/state/main:state.json.gz")
	cmd.Dir = bare
	blob, err := cmd.Output()
	if err != nil || !isGzip(blob) {
		t.Fatalf("pushed blob is not gzip: %v %q", err, blob[:min(len(blob), 20)])
	}
	if err := os.Remove(local); err != nil {
		t.Fatal(err)
	}
	mustRun(t, "state", "pull", "-C", g.dir)
	if data := readFile(t, local); isGzip([]byte(data)) || loadState(t, local).Empty() {
		t.Errorf("pulled state: %q", data[:min(len(data), 40)])
	}
	gz := filepath.Join(t.TempDir(), "other.json.gz")
	mustRun(t, "state", "pull", "-C", g.dir, "--state", gz)
	if st := loadState(t, gz); st.Empty() {
		t.Error("pulled .gz state is empty")
	}
}

// init --force rewrites the config file smates reads instead of adding a smates.yml
// next to it.
func TestInitForceOverwritesExistingName(t *testing.T) {
	for _, content := range []string{"version: 1\norphans: all\n", "not: [valid\n"} {
		g := newGitRepo(t)
		testutil.Write(t, g.dir, ".smates.yaml", content)
		testutil.Write(t, g.dir, "go.mod", "module x\n")
		testutil.Write(t, g.dir, "x_test.go", "package x\n")
		g.git("add", "-A")
		g.commit("files")
		code, out, errs := smatesCmd(t, g.dir, "init", "--force")
		if code != 0 || !strings.Contains(out, "wrote .smates.yaml") {
			t.Fatalf("init --force: %d\n%s%s", code, out, errs)
		}
		if fileExists(g.dir, "smates.yml") {
			t.Error("wrote smates.yml next to .smates.yaml")
		}
		if cfg, err := config.Load(g.dir); err != nil || cfg == nil || cfg.Path != ".smates.yaml" || len(cfg.Suites) != 1 {
			t.Errorf("config after init --force: %+v %v", cfg, err)
		}
	}
}

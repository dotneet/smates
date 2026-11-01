package cli

import (
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/dotneet/smates/internal/config"
)

// TestInitGitHubKeepsConfig: with smates.yml present, init --github (no --force) only
// generates the workflow or the patch, from the config's suites, and leaves the config
// byte-identical; without --github it refuses; --force rewrites it.
func TestInitGitHubKeepsConfig(t *testing.T) {
	clearCI(t)
	noAgentCredentials(t)
	const cfg = "# hand-written, keep me\nversion: 1\nsuites:\n  - name: unit   # custom name\n    runner: go\n    root: .\n"
	setup := func(files map[string]string) *gitRepo {
		g := newGitRepo(t)
		files["smates.yml"] = cfg
		files["go.mod"] = "module example.com/x\n\ngo 1.22\n"
		files["x_test.go"] = "package x\n"
		writeTree(t, g.dir, files)
		g.git("add", ".")
		g.commit("files")
		return g
	}

	// No workflow yet: the workflow is written, smates.yml is not touched.
	g := setup(map[string]string{})
	code, out, errs := smatesCmd(t, g.dir, "init", "--github")
	if code != 0 {
		t.Fatalf("%d %s", code, errs)
	}
	if readFile(t, filepath.Join(g.dir, "smates.yml")) != cfg {
		t.Errorf("smates.yml changed")
	}
	if !fileExists(g.dir, ".github/workflows/smates.yml") || !strings.Contains(out, "smates.yml exists: left unchanged") || !strings.Contains(out, "wrote .github/workflows/smates.yml") {
		t.Errorf("output:\n%s", out)
	}
	if fileExists(g.dir, ".gitignore") {
		t.Errorf("workflow-only init touched .gitignore")
	}
	if code, _, errs := smatesCmd(t, g.dir, "init"); code == 0 || !strings.Contains(errs, "--github") {
		t.Errorf("init over an existing config: %d %s", code, errs)
	}

	// An existing workflow runs the tests: --patch writes the patch only, JSON reports it.
	g = setup(map[string]string{".github/workflows/ci.yml": ciWorkflow})
	pf := filepath.Join(t.TempDir(), "smates.patch")
	code, out, errs = smatesCmd(t, g.dir, "init", "--github", "--patch", pf, "--format", "json")
	if code != 0 {
		t.Fatalf("%d %s", code, errs)
	}
	rep := decodeJSON[initReport](t, out)
	if rep.Written || rep.Config != "smates.yml" || len(rep.Suites) != 1 || rep.Suites[0].Name != "unit" ||
		rep.Workflow == nil || rep.Workflow.Written || rep.Workflow.Patch == "" || !reflect.DeepEqual(rep.FilesWritten, []string{pf}) {
		t.Errorf("report %+v %+v", rep, rep.Workflow)
	}
	if readFile(t, filepath.Join(g.dir, "smates.yml")) != cfg || fileExists(g.dir, ".github/workflows/smates.yml") {
		t.Errorf("wrote more than the patch")
	}
	cmd := exec.Command("git", "apply", "--check", pf)
	cmd.Dir = g.dir
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Errorf("patch does not apply: %v %s", err, b)
	}

	// --force still rewrites smates.yml.
	if code, _, errs := smatesCmd(t, g.dir, "init", "--force"); code != 0 {
		t.Fatal(errs)
	}
	if readFile(t, filepath.Join(g.dir, "smates.yml")) == cfg {
		t.Errorf("--force did not rewrite smates.yml")
	}
}

func TestDoctorWorkflowFixCommand(t *testing.T) {
	clearCI(t)
	noAgentCredentials(t)
	g := newGitRepo(t)
	writeTree(t, g.dir, map[string]string{
		".github/workflows/ci.yml": ciWorkflow,
		"go.mod":                   "module example.com/x\n\ngo 1.22\n",
		"x_test.go":                "package x\n",
		"smates.yml":               "version: 1\nsuites:\n  - name: go\n    runner: go\n    root: .\n",
	})
	g.git("add", ".")
	g.commit("files")
	_, out, _ := smatesCmd(t, g.dir, "doctor", "--format", "json")
	rep := decodeJSON[doctorReport](t, out)
	found := false
	for _, c := range rep.Checks {
		if c.ID == "workflow-test-step" {
			found = true
			if c.Fix == nil || c.Fix.Command != "smates init --github --patch smates.patch" || !strings.Contains(c.Message, "`smates init --github --patch smates.patch`") {
				t.Errorf("fix %+v", c)
			}
		}
	}
	if !found {
		t.Errorf("no workflow-test-step check:\n%s", out)
	}
	// The suggested command works over the existing config.
	if code, _, errs := smatesCmd(t, g.dir, "init", "--github", "--patch", filepath.Join(t.TempDir(), "p")); code != 0 {
		t.Errorf("suggested command failed: %s", errs)
	}
	// init's needs_review suggests it too.
	g2 := newGitRepo(t)
	writeTree(t, g2.dir, map[string]string{".github/workflows/ci.yml": ciWorkflow, "go.mod": "module example.com/x\n\ngo 1.22\n", "x_test.go": "package x\n"})
	_, out, _ = smatesCmd(t, g2.dir, "init", "--format", "json")
	ir := decodeJSON[initReport](t, out)
	if !slices.ContainsFunc(ir.NeedsReview, func(it reviewItem) bool {
		return it.Kind == "workflow-test-step" && it.Action.Command == "smates init --github --patch smates.patch"
	}) {
		t.Errorf("needs_review: %+v", ir.NeedsReview)
	}
}

func TestConfigSetUntested(t *testing.T) {
	g, path := configRepo(t)
	if code, _, errs := smatesCmd(t, g.dir, "config", "set", "untested", "orphan"); code != 0 {
		t.Fatal(errs)
	}
	c, err := config.Load(g.dir)
	if err != nil || c.Untested != config.UntestedOrphan || !strings.HasSuffix(readFile(t, path), "\nuntested: orphan\n") {
		t.Errorf("%v %+v", err, c)
	}
	before := readFile(t, path)
	if code, _, _ := smatesCmd(t, g.dir, "config", "set", "untested", "sometimes"); code == 0 || readFile(t, path) != before {
		t.Errorf("untested: sometimes accepted")
	}
}

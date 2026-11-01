package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A symlinked smates.yml stays a symlink: the edit goes to its target.
func TestConfigEditSymlink(t *testing.T) {
	g, path := configRepo(t)
	target := filepath.Join(g.dir, "conf", "smates.yml")
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(path, target); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join("conf", "smates.yml"), path); err != nil {
		t.Skip("symlinks unsupported:", err)
	}
	if code, _, errs := smatesCmd(t, g.dir, "config", "add-ignore", "tools/**"); code != 0 {
		t.Fatal(errs)
	}
	if code, _, errs := smatesCmd(t, g.dir, "config", "set", "orphans", "ignore"); code != 0 {
		t.Fatal(errs)
	}
	fi, err := os.Lstat(path)
	if err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("smates.yml is no longer a symlink: %v %v", fi.Mode(), err)
	}
	got := readFile(t, target)
	if !strings.Contains(got, `  - "tools/**"`) || !strings.Contains(got, "orphans: ignore   # policy comment") || !strings.Contains(got, "(keep this comment)") {
		t.Errorf("target:\n%s", got)
	}
}

// init --github over an existing config judges needs_review by that config: its
// orphans policy and the variables its suites' env: sets.
func TestInitReviewUsesExistingConfig(t *testing.T) {
	clearCI(t)
	noAgentCredentials(t)
	files := map[string]string{
		"go.mod":       "module example.com/x\n\ngo 1.22\n",
		"x_test.go":    "package x\nimport \"os\"\nvar _ = os.Getenv(\"APP_MODE\") + os.Getenv(\"APP_LEVEL\")\n",
		".env.example": "APP_MODE=dev\nAPP_LEVEL=1\n",
	}
	ids := func(cfg string) map[string]bool {
		g := newGitRepo(t)
		fs := map[string]string{}
		for k, v := range files {
			fs[k] = v
		}
		if cfg != "" {
			fs["smates.yml"] = cfg
		}
		writeTree(t, g.dir, fs)
		g.git("add", ".")
		g.commit("files")
		args := []string{"--format", "json"}
		if cfg != "" {
			args = append(args, "--github")
		}
		code, out, errs := smatesCmd(t, g.dir, "init", args...)
		if code != 0 {
			t.Fatalf("%d %s", code, errs)
		}
		rep := decodeJSON[initReport](t, out)
		m := map[string]bool{}
		for _, it := range rep.NeedsReview {
			m[it.ID] = true
		}
		return m
	}
	fresh := ids("")
	if !fresh["orphans-policy"] || !fresh["env-app-mode"] || !fresh["env-app-level"] {
		t.Errorf("fresh init: %v", fresh)
	}
	kept := ids("version: 1\norphans: all\nsuites:\n  - name: go\n    runner: go\n    root: .\n    env: {APP_MODE: test}\n")
	if kept["orphans-policy"] || kept["env-app-mode"] || !kept["env-app-level"] {
		t.Errorf("existing config: %v", kept)
	}
}

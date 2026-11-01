package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestInitAgentsMDExistingConfig(t *testing.T) {
	g := newGitRepo(t)
	// Any accepted config name counts as an existing config.
	os.WriteFile(filepath.Join(g.dir, ".smates.yaml"), []byte("version: 1\nsuites: []\n"), 0o644)
	os.WriteFile(filepath.Join(g.dir, "CLAUDE.md"), []byte("# notes\n"), 0o644)
	var out, errb bytes.Buffer
	// Without --agents-md (or --github) an existing config is an error, as before.
	if err := cmdInit([]string{"-C", g.dir}, &out, &errb); err == nil || !strings.Contains(err.Error(), "--agents-md") {
		t.Errorf("existing config without flags: %v", err)
	}
	out.Reset()
	if err := cmdInit([]string{"-C", g.dir, "--agents-md", "--format", "json"}, &out, &errb); err != nil {
		t.Fatalf("--agents-md --format json: %v\n%s", err, errb.String())
	}
	type report struct {
		Config       string   `json:"config"`
		Written      bool     `json:"written"`
		FilesWritten []string `json:"files_written"`
		AgentsMD     *struct {
			File, Status, Snippet string
		} `json:"agents_md"`
	}
	var rep report
	if err := json.Unmarshal(out.Bytes(), &rep); err != nil {
		t.Fatalf("json: %v\n%s", err, out.String())
	}
	if rep.Config != ".smates.yaml" || rep.Written || rep.AgentsMD == nil || rep.AgentsMD.File != "CLAUDE.md" || rep.AgentsMD.Status != "added" ||
		len(rep.FilesWritten) != 1 || rep.FilesWritten[0] != "CLAUDE.md" {
		t.Errorf("report: %s", out.String())
	}
	if c := readFile(t, filepath.Join(g.dir, ".smates.yaml")); c != "version: 1\nsuites: []\n" {
		t.Errorf("config rewritten: %q", c)
	}
	if _, err := os.Stat(filepath.Join(g.dir, "smates.yml")); err == nil {
		t.Error("wrote smates.yml next to .smates.yaml")
	}
	// Again: present, nothing written.
	out.Reset()
	rep = report{}
	cmdInit([]string{"-C", g.dir, "--agents-md", "--format", "json"}, &out, &errb)
	if err := json.Unmarshal(out.Bytes(), &rep); err != nil || rep.AgentsMD == nil || rep.AgentsMD.Status != "present" || len(rep.FilesWritten) != 0 {
		t.Errorf("second run: %v %s", err, out.String())
	}
	if c := readFile(t, filepath.Join(g.dir, "CLAUDE.md")); strings.Count(c, agentsBegin) != 1 {
		t.Errorf("CLAUDE.md:\n%s", c)
	}

	// --github over an existing config: the workflow only, then the suggestion.
	h := newGitRepo(t)
	os.WriteFile(filepath.Join(h.dir, "smates.yml"), []byte("version: 1\nsuites: []\n"), 0o644)
	os.WriteFile(filepath.Join(h.dir, "AGENTS.md"), []byte("x\n"), 0o644)
	out.Reset()
	if err := cmdInit([]string{"-C", h.dir, "--github"}, &out, &errb); err != nil {
		t.Fatalf("--github: %v\n%s", err, errb.String())
	}
	if s := out.String(); !strings.Contains(s, "left unchanged; generating the workflow") || !strings.Contains(s, "add this to AGENTS.md") {
		t.Errorf("--github over a config:\n%s", s)
	}
	if readFile(t, filepath.Join(h.dir, "AGENTS.md")) != "x\n" {
		t.Error("AGENTS.md written without --agents-md")
	}
}

func TestClaudeStopHookSharedState(t *testing.T) {
	g := worktreeFixture(t, "state_shared: true\n")
	wt := addWorktree(t, g, "feat")
	mustRun(t, "run", "-C", g.dir, "--no-agent", "--all")
	mustRun(t, "run", "-C", wt, "--no-agent")
	snapshot := func() map[string]string {
		m := map[string]string{}
		for _, dir := range []string{filepath.Join(g.dir, ".git", "smates"), filepath.Join(wt, ".smates"), filepath.Join(g.dir, ".smates")} {
			entries, _ := os.ReadDir(dir)
			for _, e := range entries {
				p := filepath.Join(dir, e.Name())
				fi, err := os.Stat(p)
				if err != nil || fi.IsDir() {
					continue
				}
				m[p] = fi.ModTime().String() + readFile(t, p)
			}
		}
		return m
	}
	stop := func() string {
		t.Helper()
		in, _ := json.Marshal(map[string]any{"stop_hook_active": false, "cwd": wt})
		var out, errb bytes.Buffer
		if code, err := cmdHook(context.Background(), []string{"claude-stop", "--", "--no-agent"}, bytes.NewReader(in), &out, &errb); code != 0 || err != nil {
			t.Fatalf("claude-stop: %d %v %s", code, err, errb.String())
		}
		return out.String()
	}
	before := snapshot()
	if len(before) == 0 {
		t.Fatal("no state files found")
	}
	time.Sleep(20 * time.Millisecond)
	if out := stop(); out != "" {
		t.Errorf("nothing selected: %q", out)
	}
	if after := snapshot(); !reflect.DeepEqual(before, after) {
		t.Errorf("the fast path wrote state:\nbefore %v\nafter  %v", before, after)
	}
	// A failing test in the linked worktree blocks.
	os.WriteFile(filepath.Join(wt, "src", "lib"), nil, 0o644)
	if out := stop(); !strings.Contains(out, `"decision":"block"`) || !strings.Contains(out, "t/a_check.py") {
		t.Errorf("failing in the worktree: %q", out)
	}
}

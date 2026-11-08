package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// `smates hook install --codex` merges a Stop hook into .codex/hooks.json like the
// Claude Code one: other hooks stay byte for byte, installing again changes nothing,
// and the command finds smates on PATH (no absolute path, so it works in every
// worktree).
func TestCodexHookInstall(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	g := newGitRepo(t)
	path := filepath.Join(g.dir, ".codex", "hooks.json")
	claude := filepath.Join(g.dir, ".claude", "settings.json")
	type hooksFile struct {
		Hooks struct {
			Stop []struct {
				Hooks []struct {
					Type    string `json:"type"`
					Command string `json:"command"`
					Timeout int    `json:"timeout"`
				} `json:"hooks"`
			} `json:"Stop"`
		} `json:"hooks"`
	}
	for name, doc := range map[string]string{
		"no file":    "",
		"empty":      "{}\n",
		"other hook": "{\n  \"hooks\": {\n    \"Stop\": [\n      {\"hooks\": [{\"type\": \"command\", \"command\": \"./scripts/lint.sh\", \"timeout\": 30}]}\n    ],\n    \"PostToolUse\": []\n  }\n}\n",
	} {
		t.Run(name, func(t *testing.T) {
			os.RemoveAll(filepath.Dir(path))
			if doc != "" {
				os.MkdirAll(filepath.Dir(path), 0o755)
				os.WriteFile(path, []byte(doc), 0o644)
			}
			code, out, errs := hookCmd(t, g.dir, "install", "--codex", "--", "--suite", "api")
			if code != 0 || !strings.Contains(out, "installed the smates Codex Stop hook") || !strings.Contains(out, "When Codex ends its turn") {
				t.Fatalf("install: %d %s %s", code, out, errs)
			}
			installed := readFile(t, path)
			var v hooksFile
			if err := json.Unmarshal([]byte(installed), &v); err != nil || len(v.Hooks.Stop) == 0 {
				t.Fatalf("hooks.json (%v):\n%s", err, installed)
			}
			h := v.Hooks.Stop[len(v.Hooks.Stop)-1].Hooks[0]
			if h.Type != "command" || h.Timeout != claudeHookTimeout ||
				h.Command != "if command -v smates >/dev/null 2>&1; then smates hook codex-stop -- --no-agent --suite api; fi" {
				t.Errorf("entry: %+v", h)
			}
			if name == "other hook" && (len(v.Hooks.Stop) != 2 || v.Hooks.Stop[0].Hooks[0].Command != "./scripts/lint.sh") {
				t.Errorf("other hooks not kept:\n%s", installed)
			}
			if _, err := os.Stat(claude); err == nil {
				t.Errorf("--codex wrote %s", claude)
			}
			if code, out, _ := hookCmd(t, g.dir, "install", "--codex", "--", "--suite", "api"); code != 0 || !strings.Contains(out, "already installed") || readFile(t, path) != installed {
				t.Errorf("second install: %d %s", code, out)
			}
			if _, out, _ := hookCmd(t, g.dir, "status"); !strings.Contains(out, "Codex Stop hook (project): installed") || !strings.Contains(out, "Claude Code Stop hook (project): not installed") {
				t.Errorf("status:\n%s", out)
			}
			if code, out, _ := hookCmd(t, g.dir, "uninstall", "--codex"); code != 0 || !strings.Contains(out, "removed") {
				t.Fatalf("uninstall: %d %s", code, out)
			}
			want := doc
			if doc == "" {
				want = "{}\n"
			}
			if got := readFile(t, path); got != want {
				t.Errorf("after uninstall\n got %q\nwant %q", got, want)
			}
		})
	}

	// The Claude Code and Codex entries are told apart: each command removes its own.
	os.RemoveAll(filepath.Dir(path))
	both := `{"hooks": {"Stop": [{"hooks": [{"type": "command", "command": "smates hook claude-stop"}]}]}}`
	os.MkdirAll(filepath.Dir(path), 0o755)
	os.WriteFile(path, []byte(both), 0o644)
	if code, _, errs := hookCmd(t, g.dir, "install", "--codex"); code != 0 {
		t.Fatalf("install next to a claude-stop entry: %s", errs)
	}
	if c := readFile(t, path); !strings.Contains(c, "smates hook claude-stop") || !strings.Contains(c, codexHookMarker) {
		t.Errorf("both entries:\n%s", c)
	}
	hookCmd(t, g.dir, "uninstall", "--codex")
	if got := readFile(t, path); got != both {
		t.Errorf("uninstall --codex removed more than its entry: %q", got)
	}

	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"install", "--codex", "--user"}, "--user applies to --claude"},
		{[]string{"install", "--codex", "--claude"}, "cannot be combined"},
		{[]string{"install", "--codex", "--hook", "pre-commit"}, "not --codex"},
		{[]string{"install", "--codex", "--append"}, "not --codex"},
	} {
		if code, _, errs := hookCmd(t, g.dir, tc.args...); code != ExitUsage || !strings.Contains(errs, tc.want) {
			t.Errorf("%v: exit %d %q", tc.args, code, errs)
		}
	}
}

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dotneet/smates/docs"
	"github.com/dotneet/smates/internal/selector"
)

func TestGuide(t *testing.T) {
	var out, errb bytes.Buffer
	if code := Main([]string{"guide"}, &out, &errb); code != 0 || out.String() != docs.Agents {
		t.Fatalf("guide: exit %d, output differs from docs/AGENTS.md (%d vs %d bytes)", code, out.Len(), len(docs.Agents))
	}
	data, err := os.ReadFile(filepath.Join("..", "..", "docs", "AGENTS.md"))
	if err != nil || string(data) != docs.Agents {
		t.Fatalf("embedded guide differs from docs/AGENTS.md: %v", err)
	}
	if n := strings.Count(docs.Agents, "\n"); n > 90 {
		t.Errorf("docs/AGENTS.md has %d lines; keep it short", n)
	}
}

func TestInitAgentsMD(t *testing.T) {
	g := newGitRepo(t)
	agents := filepath.Join(g.dir, "AGENTS.md")
	os.WriteFile(agents, []byte("# Agents\n\nBe nice.\n"), 0o644)
	init := func(args ...string) string {
		t.Helper()
		var out, errb bytes.Buffer
		if err := cmdInit(append([]string{"-C", g.dir}, args...), &out, &errb); err != nil {
			t.Fatalf("init %v: %v\n%s", args, err, errb.String())
		}
		return out.String()
	}
	// Without --agents-md the section is only suggested.
	if out := init(); !strings.Contains(out, "add this to AGENTS.md") || !strings.Contains(out, "smates run --output failures") {
		t.Errorf("suggestion:\n%s", out)
	}
	if readFile(t, agents) != "# Agents\n\nBe nice.\n" {
		t.Error("init without --agents-md wrote AGENTS.md")
	}
	// --agents-md appends it once (smates.yml exists already: left alone).
	if out := init("--agents-md"); !strings.Contains(out, "added the smates section to AGENTS.md") {
		t.Errorf("--agents-md:\n%s", out)
	}
	first := readFile(t, agents)
	if !strings.HasPrefix(first, "# Agents\n\nBe nice.\n\n"+agentsBegin) || !strings.HasSuffix(first, agentsEnd+"\n") {
		t.Errorf("AGENTS.md:\n%s", first)
	}
	if out := init("--agents-md"); !strings.Contains(out, "already has the smates section") {
		t.Errorf("second --agents-md:\n%s", out)
	}
	if readFile(t, agents) != first {
		t.Error("second --agents-md changed AGENTS.md")
	}
	if out := init("--force"); strings.Contains(out, "add this to") {
		t.Errorf("suggested a section that is there:\n%s", out)
	}

	// Only CLAUDE.md: the section goes there.
	h := newGitRepo(t)
	os.WriteFile(filepath.Join(h.dir, "CLAUDE.md"), []byte("notes"), 0o644)
	var out bytes.Buffer
	if err := cmdInit([]string{"-C", h.dir, "--agents-md"}, &out, &out); err != nil {
		t.Fatal(err)
	}
	if c := readFile(t, filepath.Join(h.dir, "CLAUDE.md")); !strings.HasPrefix(c, "notes\n\n"+agentsBegin) {
		t.Errorf("CLAUDE.md:\n%s", c)
	}
	if _, err := os.Stat(filepath.Join(h.dir, "AGENTS.md")); err == nil {
		t.Error("created AGENTS.md next to CLAUDE.md")
	}
}

func TestClaudeHookSettings(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	g := newGitRepo(t)
	path := filepath.Join(g.dir, ".claude", "settings.json")
	os.MkdirAll(filepath.Dir(path), 0o755)
	original := `{
  "permissions": {"allow": ["Bash(npm *)"]},
  "hooks": {
    "PostToolUse": [{"matcher": "Write", "hooks": [{"type": "command", "command": "prettier"}]}],
    "Stop": [{"hooks": [{"type": "command", "command": "say done"}]}]
  },
  "zzz": 1
}`
	os.WriteFile(path, []byte(original), 0o644)

	if code, out, errs := hookCmd(t, g.dir, "install", "--claude"); code != 0 || !strings.Contains(out, "installed the smates Claude Code Stop hook") {
		t.Fatalf("install: %d %s %s", code, out, errs)
	}
	content := readFile(t, path)
	var s struct {
		Permissions any `json:"permissions"`
		Zzz         int `json:"zzz"`
		Hooks       struct {
			PostToolUse []any `json:"PostToolUse"`
			Stop        []struct {
				Hooks []struct {
					Type    string `json:"type"`
					Command string `json:"command"`
					Timeout int    `json:"timeout"`
				} `json:"hooks"`
			} `json:"Stop"`
		} `json:"hooks"`
	}
	if err := json.Unmarshal([]byte(content), &s); err != nil {
		t.Fatalf("settings.json: %v\n%s", err, content)
	}
	if s.Permissions == nil || s.Zzz != 1 || len(s.Hooks.PostToolUse) != 1 || len(s.Hooks.Stop) != 2 ||
		s.Hooks.Stop[0].Hooks[0].Command != "say done" {
		t.Fatalf("other settings not kept:\n%s", content)
	}
	h := s.Hooks.Stop[1].Hooks[0]
	if h.Type != "command" || h.Timeout != claudeHookTimeout || !strings.Contains(h.Command, "smates hook claude-stop -- --no-agent") {
		t.Errorf("smates entry: %+v", h)
	}
	// Key order is kept.
	if strings.Index(content, `"permissions"`) > strings.Index(content, `"hooks"`) || strings.Index(content, `"hooks"`) > strings.Index(content, `"zzz"`) {
		t.Errorf("key order changed:\n%s", content)
	}
	// Idempotent.
	if code, out, _ := hookCmd(t, g.dir, "install", "--claude"); code != 0 || !strings.Contains(out, "already installed") || readFile(t, path) != content {
		t.Errorf("second install: %d %s", code, out)
	}
	// Other flags replace the entry.
	if code, out, _ := hookCmd(t, g.dir, "install", "--claude", "--", "--suite", "web"); code != 0 || !strings.Contains(out, "updated") {
		t.Errorf("reinstall: %d %s", code, out)
	}
	if c := readFile(t, path); strings.Count(c, claudeHookMarker) != 1 || !strings.Contains(c, "--no-agent --suite web") {
		t.Errorf("reinstalled:\n%s", c)
	}
	if _, out, _ := hookCmd(t, g.dir, "status"); !strings.Contains(out, "Claude Code Stop hook (project): installed") ||
		!strings.Contains(out, "Claude Code Stop hook (user): not installed") {
		t.Errorf("status:\n%s", out)
	}
	// Uninstall removes only smates' entry.
	if code, out, _ := hookCmd(t, g.dir, "uninstall", "--claude"); code != 0 || !strings.Contains(out, "removed") {
		t.Fatalf("uninstall: %d %s", code, out)
	}
	var after, before any
	json.Unmarshal([]byte(readFile(t, path)), &after)
	json.Unmarshal([]byte(original), &before)
	a, _ := json.Marshal(after)
	b, _ := json.Marshal(before)
	if string(a) != string(b) {
		t.Errorf("after uninstall\n got %s\nwant %s", a, b)
	}
	if _, out, _ := hookCmd(t, g.dir, "status", "--claude"); !strings.Contains(out, "(project): not installed") {
		t.Errorf("status after uninstall:\n%s", out)
	}

	// A new settings file; uninstalling leaves no empty hooks object behind.
	os.Remove(path)
	hookCmd(t, g.dir, "install", "--claude")
	hookCmd(t, g.dir, "uninstall", "--claude")
	if c := readFile(t, path); strings.TrimSpace(c) != "{}" {
		t.Errorf("after install+uninstall: %q", c)
	}

	// --user writes ~/.claude/settings.json and only runs where smates.yml exists.
	if code, _, errs := hookCmd(t, g.dir, "install", "--claude", "--user"); code != 0 {
		t.Fatalf("install --user: %d %s", code, errs)
	}
	if c := readFile(t, filepath.Join(os.Getenv("HOME"), ".claude", "settings.json")); !strings.Contains(c, "claude-stop --require-config -- --no-agent") {
		t.Errorf("user settings:\n%s", c)
	}
	if code, _, _ := hookCmd(t, g.dir, "install", "--user"); code != 2 {
		t.Errorf("--user without --claude: exit %d", code)
	}
	// Invalid JSON is an error, not overwritten.
	os.WriteFile(path, []byte("{oops"), 0o644)
	if code, _, _ := hookCmd(t, g.dir, "install", "--claude"); code == 0 || readFile(t, path) != "{oops" {
		t.Errorf("invalid settings.json: exit %d, %q", code, readFile(t, path))
	}
}

func TestClaudeStopHook(t *testing.T) {
	dir, write, _ := outputFixture(t)
	stop := func(input string, args ...string) (int, string, string) {
		t.Helper()
		var out, errb bytes.Buffer
		code, err := cmdHook(context.Background(), append([]string{"claude-stop"}, args...), strings.NewReader(input), &out, &errb)
		if err != nil {
			t.Fatalf("claude-stop: %v", err)
		}
		return code, out.String(), errb.String()
	}
	in := func(active bool) string {
		b, _ := json.Marshal(map[string]any{"hook_event_name": "Stop", "stop_hook_active": active, "cwd": dir, "session_id": "x"})
		return string(b)
	}
	// Nothing changed: nothing runs, Claude may stop.
	if code, out, errs := stop(in(false), "--", "--no-agent"); code != 0 || out != "" {
		t.Errorf("unchanged: %d %q %s", code, out, errs)
	}
	// A failing test blocks the stop with the summary as the reason.
	write("src/common.py", "X = 2\n")
	code, out, errs := stop(in(false), "--", "--no-agent")
	var dec struct{ Decision, Reason string }
	if code != 0 || json.Unmarshal([]byte(out), &dec) != nil || dec.Decision != "block" ||
		!strings.Contains(dec.Reason, "bad: bad/b_check.py") || !strings.Contains(dec.Reason, "--only bad:bad/b_check.py") ||
		!strings.Contains(dec.Reason, "X is 2") || strings.Contains(dec.Reason, "GOOD-NOISE") {
		t.Fatalf("failing: %d %q\n%s", code, out, errs)
	}
	// Claude is already continuing because of the hook: never block again.
	if code, out, _ := stop(in(true), "--", "--no-agent"); code != 0 || out != "" {
		t.Errorf("stop_hook_active: %d %q", code, out)
	}
	// Fixed: Claude may stop.
	write("src/common.py", "X = 1\n")
	if code, out, errs := stop(in(false), "--", "--no-agent"); code != 0 || out != "" {
		t.Errorf("passing: %d %q %s", code, out, errs)
	}
	// SMATES_SKIP_HOOK skips it.
	write("src/common.py", "X = 5\n")
	t.Setenv("SMATES_SKIP_HOOK", "1")
	if code, out, _ := stop(in(false), "--", "--no-agent"); code != 0 || out != "" {
		t.Errorf("skipped: %d %q", code, out)
	}
	t.Setenv("SMATES_SKIP_HOOK", "")
	// Errors of smates are not fed back to Claude as a block (exit 1, not 2).
	if code, out, errs := stop(in(false), "--", "--no-agent", "--suite", "nope"); code != 1 || out != "" || !strings.Contains(errs, "unknown suite") {
		t.Errorf("config error: %d %q %s", code, out, errs)
	}
	// --require-config: without smates.yml the hook does nothing.
	os.Remove(filepath.Join(dir, "smates.yml"))
	if code, out, _ := stop(in(false), "--require-config", "--", "--no-agent"); code != 0 || out != "" {
		t.Errorf("--require-config: %d %q", code, out)
	}
	// Outside a git repository it does nothing.
	b, _ := json.Marshal(map[string]any{"cwd": t.TempDir()})
	if code, out, _ := stop(string(b)); code != 0 || out != "" {
		t.Errorf("no repository: %d %q", code, out)
	}
}

func TestHeartbeat(t *testing.T) {
	_, write, run := outputFixture(t)
	write("bad/b_check.py", "import time, sys\nsys.path.insert(0, 'src')\nimport common\ntime.sleep(0.5)\n")
	t.Setenv("SMATES_HEARTBEAT", "100ms")
	code, _, errs := run("run", "--output", "failures")
	if code != 0 || !strings.Contains(errs, "smates: still running bad (") || !strings.Contains(errs, "test unit(s) started") {
		t.Errorf("heartbeat: exit %d\n%s", code, errs)
	}
	// Not in full mode (the runner output streams), not when turned off.
	write("bad/b_check.py", "import time, sys\nsys.path.insert(0, 'src')\nimport common\ntime.sleep(0.4)\n")
	if _, _, errs := run("run"); strings.Contains(errs, "still running") {
		t.Errorf("heartbeat in full mode:\n%s", errs)
	}
	t.Setenv("SMATES_HEARTBEAT", "0")
	write("bad/b_check.py", "import time, sys\nsys.path.insert(0, 'src')\nimport common\ntime.sleep(0.3)\n")
	if _, _, errs := run("run", "--output", "failures"); strings.Contains(errs, "still running") {
		t.Errorf("heartbeat turned off:\n%s", errs)
	}
}

func TestHeartbeatInterval(t *testing.T) {
	for v, want := range map[string]time.Duration{"": defaultHeartbeat, "10s": 10 * time.Second, "5": 5 * time.Second, "0": 0, "junk": defaultHeartbeat} {
		t.Setenv("SMATES_HEARTBEAT", v)
		if got := heartbeatInterval(); got != want {
			t.Errorf("SMATES_HEARTBEAT=%q: %v, want %v", v, got, want)
		}
	}
}

func TestLongRunHint(t *testing.T) {
	suite := func(name string, ms ...int64) *selector.Suite {
		s := &selector.Suite{Name: name}
		for i, d := range ms {
			s.Tests = append(s.Tests, &selector.Test{ID: name + string(rune('a'+i)), Duration: d})
		}
		return s
	}
	web, api := suite("web", 60_000, 50_000, 0), suite("api", 20_000)
	batches := []*batch{{sr: api, units: []string{"apia"}}, {sr: web, units: []string{"weba", "webb", "webc"}}}
	hint := func(f *runFlags) string {
		var b bytes.Buffer
		c := &cycle{f: f, stderr: &b}
		c.hintLongRun(batches)
		return b.String()
	}
	got := hint(&runFlags{budget: 90 * time.Second})
	for _, want := range []string{"about 2m 10s", "web ~1m 50s, api ~20s, plus 1 test unit(s) never timed", "--suite web", "--report smates-report.json"} {
		if !strings.Contains(got, want) {
			t.Errorf("hint lacks %q:\n%s", want, got)
		}
	}
	if got := hint(&runFlags{budget: 3 * time.Minute}); got != "" {
		t.Errorf("under budget: %q", got)
	}
	if got := hint(&runFlags{budget: 0}); got != "" {
		t.Errorf("--budget 0: %q", got)
	}
	if got := hint(&runFlags{budget: time.Second, onlySpecs: []onlySpec{{"web", "weba"}}}); got != "" {
		t.Errorf("--only: %q", got)
	}
}

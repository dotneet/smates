package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/dotneet/smates/internal/gitx"
)

// ---- Claude Code Stop hook ----
//
// `smates hook install --claude` adds a Stop hook to Claude Code's settings.json that
// runs `smates hook claude-stop`. When Claude is about to end its turn, the hook runs the
// tests the session's changes can affect (failures mode, no agent) and, when some fail,
// prints the documented Stop-hook decision on stdout and exits 0:
//
//	{"decision": "block", "reason": "<the failing tests and the rerun command>"}
//
// Claude Code then does not stop: it feeds the reason to Claude, which goes on to fix
// the tests. The hook input on stdin carries "stop_hook_active", true when Claude is
// already continuing because of a Stop hook; the hook then never blocks again, so it
// cannot loop. Errors of smates itself (configuration, environment) exit 1, which Claude
// Code shows to the user without blocking.

const (
	// claudeHookMarker identifies smates' entry among the Stop hooks.
	claudeHookMarker = "smates hook claude-stop"
	// claudeHookTimeout is the hook's timeout in seconds (the selected tests must finish).
	claudeHookTimeout = 600
	// maxClaudeReason bounds the reason fed back to Claude (its end is kept).
	maxClaudeReason = 16 << 10
)

// claudeSettingsPath is the project's .claude/settings.json, or the user's with user.
func claudeSettingsPath(repo *gitx.Repo, user bool) (string, error) {
	if user {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		return filepath.Join(home, ".claude", "settings.json"), nil
	}
	if repo == nil {
		return "", errors.New("not in a git repository (use --user for ~/.claude/settings.json)")
	}
	return filepath.Join(repo.Root, ".claude", "settings.json"), nil
}

// claudeHookCommand is the shell command of the Stop hook. It does nothing when smates
// is not installed, so a committed project setting does not break teammates without it.
func claudeHookCommand(user bool, runArgs []string) string {
	cmd := claudeHookMarker
	if user {
		// In every project: only those configured for smates.
		cmd += " --require-config"
	}
	if len(runArgs) > 0 {
		cmd += " -- " + shellJoin(runArgs)
	}
	return "if command -v smates >/dev/null 2>&1; then " + cmd + "; fi"
}

// orderedObject is a JSON object that keeps its keys' order and the values it does not
// touch as they were, so rewriting settings.json preserves what smates does not own.
type orderedObject struct {
	keys []string
	vals map[string]json.RawMessage
}

func parseObject(data []byte) (*orderedObject, error) {
	o := &orderedObject{vals: map[string]json.RawMessage{}}
	if len(bytes.TrimSpace(data)) == 0 {
		return o, nil
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	if t, err := dec.Token(); err != nil || t != json.Delim('{') {
		return nil, errors.New("not a JSON object")
	}
	for dec.More() {
		t, err := dec.Token()
		if err != nil {
			return nil, err
		}
		k, _ := t.(string)
		var v json.RawMessage
		if err := dec.Decode(&v); err != nil {
			return nil, err
		}
		o.set(k, v)
	}
	if _, err := dec.Token(); err != nil {
		return nil, err
	}
	return o, nil
}

func (o *orderedObject) set(k string, v json.RawMessage) {
	if _, ok := o.vals[k]; !ok {
		o.keys = append(o.keys, k)
	}
	o.vals[k] = v
}

func (o *orderedObject) del(k string) {
	delete(o.vals, k)
	o.keys = slices.DeleteFunc(o.keys, func(x string) bool { return x == k })
}

func (o *orderedObject) MarshalJSON() ([]byte, error) {
	var b bytes.Buffer
	b.WriteByte('{')
	for i, k := range o.keys {
		if i > 0 {
			b.WriteByte(',')
		}
		kj, _ := json.Marshal(k)
		b.Write(kj)
		b.WriteByte(':')
		b.Write(o.vals[k])
	}
	b.WriteByte('}')
	return b.Bytes(), nil
}

// indented renders v with two-space indentation and a final newline.
func indented(v any) ([]byte, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	var b bytes.Buffer
	if err := json.Indent(&b, raw, "", "  "); err != nil {
		return nil, err
	}
	b.WriteByte('\n')
	return b.Bytes(), nil
}

// claudeSettings is a settings.json with its Stop hook groups decoded.
type claudeSettings struct {
	root  *orderedObject
	hooks *orderedObject
	stop  []json.RawMessage
}

func readClaudeSettings(path string) (*claudeSettings, error) {
	data, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	root, err := parseObject(data)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	s := &claudeSettings{root: root, hooks: &orderedObject{vals: map[string]json.RawMessage{}}}
	if raw, ok := root.vals["hooks"]; ok {
		if s.hooks, err = parseObject(raw); err != nil {
			return nil, fmt.Errorf("%s: hooks: %w", path, err)
		}
	}
	if raw, ok := s.hooks.vals["Stop"]; ok {
		if err := json.Unmarshal(raw, &s.stop); err != nil {
			return nil, fmt.Errorf("%s: hooks.Stop: %w", path, err)
		}
	}
	return s, nil
}

// smatesCommands lists the commands of smates' Stop hook entries.
func (s *claudeSettings) smatesCommands() []string {
	var out []string
	for _, g := range s.stop {
		var group struct {
			Hooks []struct {
				Command string `json:"command"`
			} `json:"hooks"`
		}
		if json.Unmarshal(g, &group) != nil {
			continue
		}
		for _, h := range group.Hooks {
			if strings.Contains(h.Command, claudeHookMarker) {
				out = append(out, h.Command)
			}
		}
	}
	return out
}

// removeSmates removes smates' entries, and the groups they leave empty.
func (s *claudeSettings) removeSmates() error {
	var kept []json.RawMessage
	for _, g := range s.stop {
		group, err := parseObject(g)
		if err != nil {
			kept = append(kept, g)
			continue
		}
		var entries []json.RawMessage
		if json.Unmarshal(group.vals["hooks"], &entries) != nil {
			kept = append(kept, g)
			continue
		}
		n := len(entries)
		entries = slices.DeleteFunc(entries, func(e json.RawMessage) bool {
			var h struct {
				Command string `json:"command"`
			}
			return json.Unmarshal(e, &h) == nil && strings.Contains(h.Command, claudeHookMarker)
		})
		switch {
		case len(entries) == n:
			kept = append(kept, g)
		case len(entries) > 0:
			raw, err := json.Marshal(entries)
			if err != nil {
				return err
			}
			group.set("hooks", raw)
			raw, err = json.Marshal(group)
			if err != nil {
				return err
			}
			kept = append(kept, raw)
		}
	}
	s.stop = kept
	return nil
}

func (s *claudeSettings) write(path string) error {
	if len(s.stop) > 0 {
		raw, err := json.Marshal(s.stop)
		if err != nil {
			return err
		}
		s.hooks.set("Stop", raw)
	} else {
		s.hooks.del("Stop")
	}
	if len(s.hooks.keys) > 0 {
		raw, err := json.Marshal(s.hooks)
		if err != nil {
			return err
		}
		s.root.set("hooks", raw)
	} else {
		s.root.del("hooks")
	}
	data, err := indented(s.root)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

func installClaudeHook(path string, command string, stdout io.Writer) error {
	s, err := readClaudeSettings(path)
	if err != nil {
		return err
	}
	cmds := s.smatesCommands()
	if len(cmds) == 1 && cmds[0] == command {
		fmt.Fprintf(stdout, "smates Claude Code Stop hook already installed in %s\n", path)
		return nil
	}
	if err := s.removeSmates(); err != nil {
		return err
	}
	group := map[string]any{"hooks": []map[string]any{{"type": "command", "command": command, "timeout": claudeHookTimeout}}}
	raw, err := json.Marshal(group)
	if err != nil {
		return err
	}
	s.stop = append(s.stop, raw)
	if err := s.write(path); err != nil {
		return err
	}
	verb := "installed"
	if len(cmds) > 0 {
		verb = "updated"
	}
	fmt.Fprintf(stdout, "%s the smates Claude Code Stop hook in %s\n"+
		"When Claude ends its turn, the tests its changes can affect run; failures make it continue and fix them.\n", verb, path)
	return nil
}

func uninstallClaudeHook(path string, stdout io.Writer) error {
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		fmt.Fprintf(stdout, "no Claude Code settings at %s\n", path)
		return nil
	}
	s, err := readClaudeSettings(path)
	if err != nil {
		return err
	}
	if len(s.smatesCommands()) == 0 {
		fmt.Fprintf(stdout, "no smates Stop hook in %s\n", path)
		return nil
	}
	if err := s.removeSmates(); err != nil {
		return err
	}
	if err := s.write(path); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "removed the smates Stop hook from %s (other settings are kept)\n", path)
	return nil
}

// claudeHookCmd is `smates hook install|uninstall|status --claude [--user]`.
func claudeHookCmd(sub, dir string, user, useAgent bool, extra []string, stdout io.Writer) (int, error) {
	if dir == "" {
		dir = "."
	}
	repo, err := gitx.Open(dir)
	if err != nil && !user {
		return 1, err
	}
	if err != nil {
		repo = nil
	}
	path, err := claudeSettingsPath(repo, user)
	if err != nil {
		return 1, err
	}
	switch sub {
	case "install":
		var runArgs []string
		if !useAgent {
			runArgs = append(runArgs, "--no-agent")
		}
		err = installClaudeHook(path, claudeHookCommand(user, append(runArgs, extra...)), stdout)
	case "uninstall":
		err = uninstallClaudeHook(path, stdout)
	case "status":
		claudeHookStatus(repo, stdout)
	default:
		return 2, usageError{fmt.Errorf("unknown hook command %q", sub)}
	}
	if err != nil {
		return 1, err
	}
	return 0, nil
}

// claudeHookStatus prints whether the project's and the user's settings run smates.
func claudeHookStatus(repo *gitx.Repo, stdout io.Writer) {
	for _, user := range []bool{false, true} {
		path, err := claudeSettingsPath(repo, user)
		if err != nil {
			continue
		}
		scope := "project"
		if user {
			scope = "user"
		}
		status := "not installed"
		if s, err := readClaudeSettings(path); err != nil {
			status = "unreadable: " + err.Error()
		} else if len(s.smatesCommands()) > 0 {
			status = "installed"
		}
		fmt.Fprintf(stdout, "Claude Code Stop hook (%s): %s\n  %s\n", scope, status, path)
	}
}

// claudeStopInput is the part of Claude Code's Stop hook input smates reads.
type claudeStopInput struct {
	StopHookActive bool   `json:"stop_hook_active"`
	Cwd            string `json:"cwd"`
}

// hookClaudeStop is what the Claude Code Stop hook runs:
// `smates hook claude-stop [--require-config] [-- run flags]`. It always exits 0
// (allow or block via JSON) or 1 (an error shown to the user): exit code 2 would feed
// smates' own errors back to Claude as if they were test failures.
func hookClaudeStop(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
	requireConfig := false
	var runArgs []string
	for i, a := range args {
		if a == "--" {
			runArgs = args[i+1:]
			break
		}
		if a == "--require-config" {
			requireConfig = true
			continue
		}
		fmt.Fprintf(stderr, "smates hook claude-stop: unknown argument %q (run flags follow --)\n", a)
		return 1, nil
	}
	if v := os.Getenv("SMATES_SKIP_HOOK"); v != "" && v != "0" && v != "false" {
		return 0, nil
	}
	var in claudeStopInput
	if stdin != nil {
		data, _ := io.ReadAll(io.LimitReader(stdin, 1<<20))
		_ = json.Unmarshal(data, &in)
	}
	if in.StopHookActive {
		return 0, nil // Claude already continues because of a Stop hook: never block twice
	}
	f, err := parseRunFlags(append([]string{"--output", "failures"}, runArgs...), stderr)
	if err != nil {
		fmt.Fprintf(stderr, "smates hook claude-stop: %v\n", err)
		return 1, nil
	}
	dir := f.dir
	if dir == "" {
		dir = in.Cwd
	}
	if dir == "" {
		dir = "."
	}
	repo, err := gitx.Open(dir)
	if err != nil {
		return 0, nil // not a git repository: nothing to test
	}
	if requireConfig && f.config == "" {
		if _, err := os.Stat(filepath.Join(repo.Root, "smates.yml")); err != nil {
			return 0, nil
		}
	}
	f.dir = repo.Root
	f.heartbeat = 0
	f.skipEmpty = true
	var out bytes.Buffer
	code, err := runCycle(ctx, f, &out, &out, nil)
	if err != nil {
		fmt.Fprintf(stderr, "%s", out.String())
		fmt.Fprintf(stderr, "smates (Claude Code Stop hook): %v\n", err)
		return 1, nil
	}
	if testsExitCode(code) == 0 {
		return 0, nil
	}
	text := strings.TrimSpace(out.String())
	if len(text) > maxClaudeReason {
		text = "… (output truncated)\n" + text[len(text)-maxClaudeReason:]
	}
	reason := "smates: tests affected by your changes fail. Fix them before you finish " +
		"(rerun just them with the command at the end), or explain why they cannot pass.\n\n" + text
	if err := json.NewEncoder(stdout).Encode(map[string]string{"decision": "block", "reason": reason}); err != nil {
		return 1, nil
	}
	return 0, nil
}

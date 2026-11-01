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

	"github.com/dotneet/smates/internal/config"
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
		if ok, err := claudeHookInstalled(path); err != nil {
			status = "unreadable: " + err.Error()
		} else if ok {
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
		if !slices.ContainsFunc(config.FileNames, func(n string) bool {
			_, err := os.Stat(filepath.Join(repo.Root, n))
			return err == nil
		}) {
			return 0, nil // not configured for smates (an invalid config is reported by the run)
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

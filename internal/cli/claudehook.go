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
	"time"

	"github.com/dotneet/smates/internal/config"
	"github.com/dotneet/smates/internal/gitx"
)

// ---- Claude Code and Codex Stop hooks ----
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
//
// `smates hook install --codex` does the same for Codex, whose .codex/hooks.json has
// the same "hooks" object, Stop input and block decision; its hook runs
// `smates hook codex-stop`.
//
// Units the run leaves for later (--max-time, which the hooks default to
// hookMaxTime, and unmet requires) do not block: the hook prints one line to stderr and
// exits 0, and they stay selected.

const (
	// claudeHookMarker identifies smates' entry among the Stop hooks.
	claudeHookMarker = "smates hook claude-stop"
	// codexHookMarker identifies smates' entry among Codex's Stop hooks.
	codexHookMarker = "smates hook codex-stop"
	// claudeHookTimeout is the hook's timeout in seconds (the selected tests must finish).
	claudeHookTimeout = 600
	// hookMaxTime is the --max-time of the hook's run unless its run flags set one: the
	// hook's timeout less a margin for selecting, recording and estimates that are off.
	hookMaxTime = (claudeHookTimeout - 120) * time.Second
	// maxClaudeReason bounds the reason fed back to Claude (its end is kept).
	maxClaudeReason = 16 << 10
)

// agentHook is a coding agent smates installs a Stop hook for.
type agentHook struct {
	name   string // "Claude Code"
	agent  string // what ends its turn: "Claude"
	flag   string // the flag of `smates hook install`: "--claude"
	marker string // the hook command, which identifies smates' entry
	dir    string // the settings directory under the repository (or home) root
	file   string // the settings file in dir
}

var (
	claudeHook = agentHook{name: "Claude Code", agent: "Claude", flag: "--claude", marker: claudeHookMarker, dir: ".claude", file: "settings.json"}
	// Codex reads project hooks from .codex/hooks.json (in a trusted project).
	codexHook = agentHook{name: "Codex", agent: "Codex", flag: "--codex", marker: codexHookMarker, dir: ".codex", file: "hooks.json"}
)

// settingsPath is the project's settings file of the agent, or the user's with user.
func (a agentHook) settingsPath(repo *gitx.Repo, user bool) (string, error) {
	if user {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		return filepath.Join(home, a.dir, a.file), nil
	}
	if repo == nil {
		return "", fmt.Errorf("not in a git repository (use --user for ~/%s/%s)", a.dir, a.file)
	}
	return filepath.Join(repo.Root, a.dir, a.file), nil
}

// command is the shell command of the Stop hook. It does nothing when smates is not
// installed, so a committed project setting does not break teammates without it. It
// finds smates on PATH, so it works in every worktree of the repository.
func (a agentHook) command(user bool, runArgs []string) string {
	cmd := a.marker
	if user {
		// In every project: only those configured for smates.
		cmd += " --require-config"
	}
	if len(runArgs) > 0 {
		cmd += " -- " + shellJoin(runArgs)
	}
	return "if command -v smates >/dev/null 2>&1; then " + cmd + "; fi"
}

// claudeHookCommand is the shell command of the Claude Code Stop hook.
func claudeHookCommand(user bool, runArgs []string) string {
	return claudeHook.command(user, runArgs)
}

// agentHookCmd is `smates hook install|uninstall|status --claude|--codex [--user]`.
func agentHookCmd(a agentHook, sub, dir string, user, useAgent bool, extra []string, stdout io.Writer) (int, error) {
	if dir == "" {
		dir = "."
	}
	repo, err := gitx.Open(dir)
	if err != nil && !user {
		return 1, asEnvError(err)
	}
	if err != nil {
		repo = nil
	}
	path, err := a.settingsPath(repo, user)
	if err != nil {
		return 1, asEnvError(err)
	}
	switch sub {
	case "install":
		var runArgs []string
		if !useAgent {
			runArgs = append(runArgs, "--no-agent")
		}
		err = installAgentHook(a, path, a.command(user, append(runArgs, extra...)), stdout)
	case "uninstall":
		err = uninstallAgentHook(a, path, stdout)
	case "status":
		agentHookStatus(repo, stdout)
	default:
		return 2, usageError{fmt.Errorf("unknown hook command %q", sub)}
	}
	if err != nil {
		return 1, err
	}
	return 0, nil
}

// agentHookStatus prints whether the project's and the user's Claude Code settings,
// and the project's Codex hooks, run smates.
func agentHookStatus(repo *gitx.Repo, stdout io.Writer) {
	for _, h := range []struct {
		a    agentHook
		user bool
	}{{claudeHook, false}, {claudeHook, true}, {codexHook, false}} {
		path, err := h.a.settingsPath(repo, h.user)
		if err != nil {
			continue
		}
		scope := "project"
		if h.user {
			scope = "user"
		}
		status := "not installed"
		if ok, err := agentHookInstalled(h.a, path); err != nil {
			status = "unreadable: " + err.Error()
		} else if ok {
			status = "installed"
		}
		fmt.Fprintf(stdout, "%s Stop hook (%s): %s\n  %s\n", h.a.name, scope, status, path)
	}
}

// stopInput is the part of the Stop hook input (Claude Code's and Codex's) smates
// reads.
type stopInput struct {
	StopHookActive bool   `json:"stop_hook_active"`
	Cwd            string `json:"cwd"`
}

// hookAgentStop is what the Stop hook of agent a runs:
// `smates hook claude-stop|codex-stop [--require-config] [-- run flags]`. It always
// exits 0 (allow or block via JSON) or 1 (an error shown to the user): exit code 2
// would feed smates' own errors back to the agent as if they were test failures.
func hookAgentStop(ctx context.Context, a agentHook, args []string, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
	requireConfig := false
	var runArgs []string
	for i, arg := range args {
		if arg == "--" {
			runArgs = args[i+1:]
			break
		}
		if arg == "--require-config" {
			requireConfig = true
			continue
		}
		if isHelp(arg) {
			fmt.Fprintf(stdout, "usage: %s [--require-config] [-- run flags]\n", a.marker)
			return 0, nil
		}
		fmt.Fprintf(stderr, "%s: unknown argument %q (run flags follow --)\n", a.marker, arg)
		return 1, nil
	}
	if v := os.Getenv("SMATES_SKIP_HOOK"); v != "" && v != "0" && v != "false" {
		return 0, nil
	}
	var in stopInput
	if stdin != nil {
		data, _ := io.ReadAll(io.LimitReader(stdin, 1<<20))
		_ = json.Unmarshal(data, &in)
	}
	if in.StopHookActive {
		return 0, nil // the agent already continues because of a Stop hook: never block twice
	}
	// The run flags come last, so a --max-time among them wins.
	f, err := parseRunFlags(append([]string{"--output", "failures", "--max-time", hookMaxTime.String()}, runArgs...), stderr)
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", a.marker, err)
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
	var deferred string
	f.deferred = func(line string) { deferred = line }
	var out bytes.Buffer
	code, err := runCycle(ctx, f, &out, &out, nil)
	var re requiresError
	if err != nil && !errors.As(err, &re) {
		fmt.Fprintf(stderr, "%s", out.String())
		fmt.Fprintf(stderr, "smates (%s Stop hook): %v\n", a.name, err)
		return 1, nil
	}
	if testsExitCode(code) == 0 {
		// Units left for later (unmet requires, --max-time) do not block; they stay
		// selected and run next time.
		if deferred != "" {
			fmt.Fprintln(stderr, deferred)
		}
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

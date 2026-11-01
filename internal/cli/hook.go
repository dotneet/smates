package cli

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/dotneet/smates/internal/config"
	"github.com/dotneet/smates/internal/gitx"
	"github.com/dotneet/smates/internal/state"
)

// ---- hook ----

// The lines that delimit what `smates hook install` writes, in a hook of its own or
// appended to an existing one; `smates hook uninstall` removes exactly that.
const (
	hookBegin = "# >>> smates hook >>>"
	hookEnd   = "# <<< smates hook <<<"
	// hookStaleAfter is the age after which the hook refreshes the state from the remote.
	hookStaleAfter = 7 * 24 * time.Hour
	// hookPullEvery limits how often the hook fetches the state, so a remote with nothing
	// (newer) to pull costs at most one fetch per interval.
	hookPullEvery = time.Hour
)

var hookKinds = []string{"pre-push", "pre-commit"}

// hookTarget is a git hook file smates manages.
type hookTarget struct {
	kind string
	path string
	// manager is "husky" or "lefthook" when one of them owns the hooks.
	manager string
}

func cmdHook(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
	if len(args) == 0 {
		return 2, usageError{errors.New("usage: smates hook install|uninstall|status|run")}
	}
	sub, rest := args[0], args[1:]
	if sub == "run" {
		return hookRun(ctx, rest, stdin, stdout, stderr)
	}
	fs := flag.NewFlagSet("hook "+sub, flag.ContinueOnError)
	fs.SetOutput(stderr)
	dir := fs.String("C", "", "run as if started in `dir`")
	kind := fs.String("hook", "pre-push", "git hook: pre-push or pre-commit")
	appendTo := fs.Bool("append", false, "install: add smates to the end of an existing hook script")
	useAgent := fs.Bool("agent", false, "install: let the hook call the agent for unattributed changes")
	if err := fs.Parse(rest); err != nil {
		return 2, usageError{err}
	}
	if !slices.Contains(hookKinds, *kind) {
		return 2, usageError{fmt.Errorf("unsupported --hook %q (want %s)", *kind, strings.Join(hookKinds, " or "))}
	}
	d := *dir
	if d == "" {
		d = "."
	}
	repo, err := gitx.Open(d)
	if err != nil {
		return 1, err
	}
	h, err := locateHook(repo, *kind)
	if err != nil {
		return 1, err
	}
	switch sub {
	case "install":
		// Without the agent a hook never makes API calls; unattributed changes then run
		// the affected suites in full. Further `smates run` flags follow `--`.
		var runArgs []string
		if !*useAgent {
			runArgs = append(runArgs, "--no-agent")
		}
		runArgs = append(runArgs, fs.Args()...)
		err = installHook(h, runArgs, *appendTo, stdout, stderr)
	case "uninstall":
		err = uninstallHook(h, stdout)
	case "status":
		err = hookStatus(h, stdout)
	default:
		return 2, usageError{fmt.Errorf("unknown hook command %q", sub)}
	}
	if err != nil {
		return 1, err
	}
	return 0, nil
}

// locateHook finds the hook file git runs: `--git-path` honours core.hooksPath and
// resolves to the common git directory in linked worktrees.
func locateHook(repo *gitx.Repo, kind string) (*hookTarget, error) {
	out, err := repo.Git("rev-parse", "--git-path", "hooks/"+kind)
	if err != nil {
		return nil, err
	}
	path := strings.TrimSpace(out)
	if !filepath.IsAbs(path) {
		path = filepath.Join(repo.Root, path)
	}
	h := &hookTarget{kind: kind, path: filepath.Clean(path)}
	hooksPath, _ := repo.Git("config", "--get", "core.hooksPath")
	data, _ := os.ReadFile(h.path)
	switch {
	case strings.Contains(filepath.ToSlash(hooksPath), ".husky") || bytes.Contains(data, []byte("husky")):
		h.manager = "husky"
	case bytes.Contains(data, []byte("lefthook")):
		h.manager = "lefthook"
	default:
		for _, f := range []string{"lefthook.yml", "lefthook.yaml", ".lefthook.yml", ".lefthook.yaml", "lefthook.toml", "lefthook.json"} {
			if _, err := os.Stat(filepath.Join(repo.Root, f)); err == nil {
				h.manager = "lefthook"
			}
		}
	}
	return h, nil
}

// hookBlock is the marked shell snippet that runs smates from a hook.
func hookBlock(kind string, runArgs []string) string {
	skip := "git push --no-verify"
	if kind == "pre-commit" {
		skip = "git commit --no-verify"
	}
	return hookBegin + "\n" +
		"# Installed by `smates hook install`; removed by `smates hook uninstall`.\n" +
		"# Skip it with `" + skip + "` or SMATES_SKIP_HOOK=1.\n" +
		hookCommand(kind, runArgs, "\n", "\t", " || exit $?") + "\n" +
		hookEnd + "\n"
}

// hookCommand runs smates if it is installed and lets the hook pass otherwise.
func hookCommand(kind string, runArgs []string, sep, indent, onFail string) string {
	cmd := "smates hook run " + kind
	if len(runArgs) > 0 {
		cmd += " -- " + shellJoin(runArgs)
	}
	return "if command -v smates >/dev/null 2>&1; then" + sep +
		indent + cmd + onFail + sep +
		"else" + sep +
		indent + `echo "smates: not found on PATH; skipping the ` + kind + ` tests" >&2` + sep +
		"fi"
}

var shellSafe = regexp.MustCompile(`^[A-Za-z0-9_./=:,+@%-]+$`)

func shellJoin(args []string) string {
	q := make([]string, len(args))
	for i, a := range args {
		if shellSafe.MatchString(a) {
			q[i] = a
		} else {
			q[i] = "'" + strings.ReplaceAll(a, "'", `'\''`) + "'"
		}
	}
	return strings.Join(q, " ")
}

// splitHookBlock returns the content around smates's block.
func splitHookBlock(content string) (before, after string, ok bool) {
	i := strings.Index(content, hookBegin)
	if i < 0 {
		return "", "", false
	}
	j := strings.Index(content[i:], hookEnd)
	if j < 0 {
		return "", "", false
	}
	j += i + len(hookEnd)
	if j < len(content) && content[j] == '\n' {
		j++
	}
	return content[:i], content[j:], true
}

func installHook(h *hookTarget, runArgs []string, appendTo bool, stdout, stderr io.Writer) error {
	block := hookBlock(h.kind, runArgs)
	data, err := os.ReadFile(h.path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	content := string(data)
	if before, after, ok := splitHookBlock(content); ok {
		updated := before + block + after
		if updated == content {
			fmt.Fprintf(stdout, "smates %s hook already installed in %s\n", h.kind, h.path)
			return nil
		}
		if err := os.WriteFile(h.path, []byte(updated), 0o755); err != nil {
			return err
		}
		fmt.Fprintf(stdout, "updated the smates %s hook in %s\n", h.kind, h.path)
		return nil
	}
	if h.manager != "" {
		printHookManager(h, runArgs, stderr)
		return fmt.Errorf("git hooks are managed by %s: add smates to its configuration (see above)", h.manager)
	}
	if err == nil {
		if !appendTo {
			fmt.Fprintf(stderr, "smates: %s exists and was not written by smates; it was left unchanged.\n"+
				"Re-run with --append to add smates to the end of it, or add this to it yourself:\n\n%s\n\n",
				h.path, indent(hookCommand(h.kind, runArgs, "\n", "  ", " || exit $?"), "  "))
			return fmt.Errorf("%s hook already exists", h.kind)
		}
		if !isShellScript(content) {
			return fmt.Errorf("%s is not a shell script: add `smates hook run %s` to it yourself", h.path, h.kind)
		}
		if endsWithExit(content) {
			fmt.Fprintf(stderr, "smates: warning: %s ends with exit/exec, so the appended smates block would never run; move it up\n", h.path)
		}
		if !strings.HasSuffix(content, "\n") {
			content += "\n"
		}
		if err := os.WriteFile(h.path, []byte(content+"\n"+block), 0o755); err != nil {
			return err
		}
		fmt.Fprintf(stdout, "appended the smates %s hook to %s\n", h.kind, h.path)
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(h.path), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(h.path, []byte("#!/bin/sh\n"+block), 0o755); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "installed the smates %s hook in %s\n", h.kind, h.path)
	return nil
}

func printHookManager(h *hookTarget, runArgs []string, w io.Writer) {
	switch h.manager {
	case "husky":
		fmt.Fprintf(w, "smates: this repository uses husky. Add this to .husky/%s:\n\n%s\n\n", h.kind,
			indent(hookCommand(h.kind, runArgs, "\n", "  ", ""), "  "))
	case "lefthook":
		fmt.Fprintf(w, "smates: this repository uses lefthook. Add this to lefthook.yml and run `lefthook install`:\n\n%s\n",
			lefthookSnippet(h.kind, runArgs))
	}
}

// lefthookSnippet is the lefthook.yml entry that runs the hook (a block scalar: the
// command contains ": ", which a plain YAML scalar cannot).
func lefthookSnippet(kind string, runArgs []string) string {
	return "  " + kind + ":\n    commands:\n      smates:\n        run: |\n" +
		indent(hookCommand(kind, runArgs, "\n", "  ", ""), "          ") + "\n        use_stdin: true\n"
}

func indent(s, prefix string) string {
	return prefix + strings.ReplaceAll(s, "\n", "\n"+prefix)
}

// isShellScript reports whether a hook is a POSIX-shell script smates can append to.
func isShellScript(content string) bool {
	if !strings.HasPrefix(content, "#!") {
		return true // git runs hooks without a shebang with sh
	}
	line, _, _ := strings.Cut(content, "\n")
	f := strings.Fields(strings.TrimPrefix(line, "#!"))
	if len(f) == 0 {
		return false
	}
	interp := filepath.Base(f[0])
	if interp == "env" && len(f) > 1 {
		interp = f[len(f)-1]
	}
	return slices.Contains([]string{"sh", "bash", "zsh", "dash", "ksh"}, interp)
}

func endsWithExit(content string) bool {
	lines := strings.Split(strings.TrimSpace(content), "\n")
	last := strings.TrimSpace(lines[len(lines)-1])
	return strings.HasPrefix(last, "exit") || strings.HasPrefix(last, "exec ")
}

func uninstallHook(h *hookTarget, stdout io.Writer) error {
	data, err := os.ReadFile(h.path)
	if errors.Is(err, os.ErrNotExist) {
		fmt.Fprintf(stdout, "no %s hook at %s\n", h.kind, h.path)
		return nil
	}
	if err != nil {
		return err
	}
	before, after, ok := splitHookBlock(string(data))
	if !ok {
		fmt.Fprintf(stdout, "%s was not written by smates; left unchanged\n", h.path)
		return nil
	}
	rest := before + after
	if after == "" {
		rest = strings.TrimRight(before, "\n") + "\n"
	}
	body := rest
	if strings.HasPrefix(body, "#!") {
		_, body, _ = strings.Cut(body, "\n")
	}
	if strings.TrimSpace(body) == "" {
		if err := os.Remove(h.path); err != nil {
			return err
		}
		fmt.Fprintf(stdout, "removed %s\n", h.path)
		return nil
	}
	if err := os.WriteFile(h.path, []byte(rest), 0o755); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "removed the smates block from %s (the rest of the hook is kept)\n", h.path)
	return nil
}

func hookStatus(h *hookTarget, stdout io.Writer) error {
	data, err := os.ReadFile(h.path)
	var status string
	switch {
	case errors.Is(err, os.ErrNotExist):
		status = "not installed"
	case err != nil:
		return err
	default:
		before, after, ok := splitHookBlock(string(data))
		switch {
		case !ok:
			status = "a hook not written by smates exists"
		case strings.TrimSpace(strings.TrimPrefix(before, "#!/bin/sh\n")) == "" && strings.TrimSpace(after) == "":
			status = "installed"
		default:
			status = "installed (appended to an existing hook)"
		}
	}
	fmt.Fprintf(stdout, "%s: %s\n  %s\n", h.kind, status, h.path)
	if h.manager != "" {
		fmt.Fprintf(stdout, "  hooks are managed by %s\n", h.manager)
	}
	return nil
}

// ---- hook run ----

// hookRun is what the installed hook calls: `smates hook run <hook> [-- run flags]`.
func hookRun(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
	if len(args) == 0 || !slices.Contains(hookKinds, args[0]) {
		return 2, usageError{fmt.Errorf("usage: smates hook run %s [-- run flags]", strings.Join(hookKinds, "|"))}
	}
	kind, runArgs := args[0], args[1:]
	if len(runArgs) > 0 && runArgs[0] == "--" {
		runArgs = runArgs[1:]
	}
	if v := os.Getenv("SMATES_SKIP_HOOK"); v != "" && v != "0" && v != "false" {
		fmt.Fprintf(stderr, "smates: SMATES_SKIP_HOOK is set; skipping the %s tests\n", kind)
		return 0, nil
	}
	f, err := parseRunFlags(runArgs, stderr)
	if err != nil {
		return 2, err
	}
	d := f.dir
	if d == "" {
		d = "."
	}
	repo, err := gitx.Open(d)
	if err != nil {
		return 1, err
	}
	if kind == "pre-push" {
		updates := readPushUpdates(stdin)
		if len(updates) > 0 && !slices.ContainsFunc(updates, func(u pushUpdate) bool { return strings.Trim(u.sha, "0") != "" }) {
			return 0, nil // only deleting remote refs
		}
		// Only branches: a tag's SHA is the tag object for annotated tags.
		var branches []string
		for _, u := range updates {
			if strings.HasPrefix(u.ref, "refs/heads/") && strings.Trim(u.sha, "0") != "" {
				branches = append(branches, u.sha)
			}
		}
		if head := repo.Head(); len(branches) > 0 && head != "" && !slices.Contains(branches, head) {
			fmt.Fprintf(stderr, "smates: warning: the pushed commits are not checked out; testing the working tree at HEAD instead\n")
		}
	}
	if n := uncommitted(repo, kind); n > 0 {
		what := "uncommitted changes"
		if kind == "pre-commit" {
			what = "changes not staged for this commit"
		}
		fmt.Fprintf(stderr, "smates: warning: %d file(s) with %s: tests run against the working tree, not exactly what is committed\n", n, what)
	}
	refreshHookState(repo, f, stderr)
	// Variables git sets for the hook would point the tests' own git commands (e.g. in
	// temporary repositories) at this repository.
	for _, v := range []string{"GIT_DIR", "GIT_WORK_TREE", "GIT_INDEX_FILE", "GIT_PREFIX"} {
		os.Unsetenv(v)
	}
	f.dir = repo.Root
	code, err := runCycle(ctx, f, stdout, stderr, nil)
	if code != 0 || err != nil {
		skip := "git push --no-verify"
		if kind == "pre-commit" {
			skip = "git commit --no-verify"
		}
		fmt.Fprintf(stderr, "smates: %s stopped by failing tests (skip once with `%s` or SMATES_SKIP_HOOK=1)\n", kind, skip)
	}
	return code, err
}

// pushUpdate is a local ref git is about to push and its object (all zeros: a deletion).
type pushUpdate struct{ ref, sha string }

// readPushUpdates reads the pre-push hook's standard input
// ("<local ref> <local sha> <remote ref> <remote sha>" per line).
func readPushUpdates(stdin io.Reader) []pushUpdate {
	if f, ok := stdin.(*os.File); ok {
		if fi, err := f.Stat(); err != nil || fi.Mode()&os.ModeCharDevice != 0 {
			return nil // a terminal: not run by git
		}
	}
	if stdin == nil {
		return nil
	}
	var updates []pushUpdate
	sc := bufio.NewScanner(stdin)
	for sc.Scan() {
		if fields := strings.Fields(sc.Text()); len(fields) == 4 {
			updates = append(updates, pushUpdate{ref: fields[0], sha: fields[1]})
		}
	}
	return updates
}

// uncommitted counts the files whose working-tree content differs from what the hook's
// operation records: the index for pre-commit, HEAD for pre-push.
func uncommitted(repo *gitx.Repo, kind string) int {
	out, err := repo.Git("status", "--porcelain=v1", "-z", "--no-renames", "--untracked-files=normal")
	if err != nil {
		return 0
	}
	n := 0
	for _, rec := range strings.Split(out, "\x00") {
		// "XY path": Y is the working tree against the index, "??" untracked.
		if len(rec) >= 3 && (kind != "pre-commit" || rec[1] != ' ') {
			n++
		}
	}
	return n
}

// refreshHookState pulls the shared state (`smates state push`) when there is none
// locally, and merges it in when the local one is stale: older than hookStaleAfter or
// recorded on a commit outside HEAD's history (e.g. before a rebase). It fetches at
// most once per hookPullEvery.
func refreshHookState(repo *gitx.Repo, f *runFlags, log io.Writer) {
	path := f.state
	if path == "" {
		cfg, err := hookConfig(repo, f.config)
		if err != nil {
			return // `smates run` reports it
		}
		path = filepath.Join(repo.Root, filepath.FromSlash(cfg.State))
	}
	st, err := state.Load(path)
	missing := err != nil || st.Empty()
	if !missing && !hookStateStale(repo, st) {
		return
	}
	// The attempt is recorded whatever its outcome, so pushes in between do not fetch.
	stamp := filepath.Join(filepath.Dir(path), "."+filepath.Base(path)+".pulled")
	if fi, err := os.Stat(stamp); err == nil && time.Since(fi.ModTime()) < hookPullEvery {
		return
	}
	if err := os.MkdirAll(filepath.Dir(stamp), 0o755); err == nil {
		_ = os.WriteFile(stamp, nil, 0o644)
	}
	refs := stateRefs(repo)
	if missing {
		_ = pullState(repo, path, "origin", refs, log)
		return
	}
	// Same extension: state.Load picks the format from it.
	tmp := filepath.Join(filepath.Dir(path), ".pull-"+filepath.Base(path))
	defer os.Remove(tmp)
	if err := pullState(repo, tmp, "origin", refs, io.Discard); err != nil {
		return
	}
	remote, err := state.Load(tmp)
	if err != nil || remote.Empty() || !remote.Created.After(st.Created) {
		return
	}
	st.Merge(remote)
	if err := st.Save(path); err != nil {
		fmt.Fprintf(log, "smates: could not save the refreshed state: %v\n", err)
		return
	}
	fmt.Fprintf(log, "smates: merged the newer remote state into %s\n", path)
}

func hookConfig(repo *gitx.Repo, file string) (*config.Config, error) {
	var cfg *config.Config
	var err error
	if file != "" {
		data, err := os.ReadFile(file)
		if err != nil {
			return nil, err
		}
		cfg, err = config.Parse(data)
		if err != nil {
			return nil, err
		}
	} else if cfg, err = config.Load(repo.Root); err != nil {
		return nil, err
	}
	if cfg == nil {
		cfg = &config.Config{}
		_ = cfg.Normalize()
	}
	return cfg, nil
}

func hookStateStale(repo *gitx.Repo, st *state.State) bool {
	if time.Since(st.Created) > hookStaleAfter {
		return true
	}
	if st.Commit == "" {
		return false
	}
	_, err := repo.Git("merge-base", "--is-ancestor", st.Commit, "HEAD")
	return err != nil
}

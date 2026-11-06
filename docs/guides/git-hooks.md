# Git hooks and Claude Code

smates can run the affected tests automatically: before every `git push` (or commit), and
each time Claude Code ends its turn. This page covers installing and removing both kinds
of hook, and what they do.

## A git hook

```text
$ smates hook install
installed the smates pre-push hook in /home/me/src/shop/.git/hooks/pre-push
```

From now on, `git push` runs `smates run --no-agent` first and stops the push when a
selected test fails. Tests this machine cannot run because a suite's
[`requires`](../reference/configuration.md#requires) are not met do not stop it: they are
left for later (one line says so) and stay selected, so CI runs them.

| option | effect |
|---|---|
| `--hook pre-commit` | install a `pre-commit` hook instead of `pre-push` (the default) |
| `--append` | add smates to the end of an existing shell hook that smates did not write |
| `--agent` | let the hook call the [LLM agent](agent.md) for orphans |
| `-- <run flags>` | pass flags on to `smates run`, e.g. `smates hook install -- --explain` |

Without `--agent`, the hook makes no API calls. Orphans (changes no test is known to
depend on) then run the affected suites in full; see
[Changes no test explains](orphans.md).

### Existing hooks, husky and lefthook

smates never overwrites a hook it did not write:

- For a plain hook script, it stops and prints the lines to add, or appends itself with
  `--append`. It warns when the script ends with `exit` or `exec`, since the appended
  block would never run.
- When husky or lefthook manages the hooks, it prints the snippet for
  `.husky/pre-push` or `lefthook.yml` instead, and writes nothing.

The hook honours `core.hooksPath`, and all worktrees of the repository share it.
Installing again updates smates' block in place.

### What the hook does

On each push (or commit), the hook:

1. **Brings the state up to date.** It pulls the shared state (`smates state pull`)
   when there is none locally. It merges in the remote one when the local state is older
   than a week, or was recorded on a commit outside the current history. It fetches at
   most once an hour.
2. **Runs the affected tests** with `smates run` and the flags it was installed with.
3. **Stops the push** when a test fails.

!!! warning "The hook tests the working tree, not the pushed commits"
    smates hashes the files on disk. Uncommitted changes (for pre-commit: unstaged
    changes) and untracked files are part of what is tested. The hook warns when there
    are any, and when the pushed branch is not the one checked out. Commit or stash them
    first for an exact result.

### Skip it

- `git push --no-verify` (or `git commit --no-verify`) skips the hook once.
- `SMATES_SKIP_HOOK=1` skips it for as long as the variable is set.
- When `smates` is not on `PATH`, the hook lets the push through with a warning, so
  teammates without smates are not blocked.

### Status and removal

```bash
smates hook status      # where the hook lives, and whether the Claude Code and Codex hooks are installed
smates hook uninstall   # remove only what smates wrote
```

## Claude Code Stop hook { #claude-code-stop-hook }

```bash
smates hook install --claude
```

This adds a Stop hook to `.claude/settings.json`. When Claude ends its turn, the hook
runs the affected tests. If some fail, Claude does not stop: it gets the failing tests and
the rerun command, and goes on to fix them.

| option | effect |
|---|---|
| `--user` | install in `~/.claude/settings.json` instead. It is then active in every project that has a `smates.yml` |
| `--agent` | let the hook call the LLM agent for orphans (default: `--no-agent`) |
| `-- <run flags>` | more `smates run` flags, e.g. `-- --suite api` |

### How it behaves

The hook runs `smates hook claude-stop`, which calls
`smates run --output failures --no-agent --max-time 8m`, plus any flags given after `--`.

- **Tests fail:** it prints
  `{"decision": "block", "reason": "<failing tests and the rerun command>"}` and exits 0.
  This is Claude Code's documented way to keep Claude going. Claude reads the reason and
  fixes the tests.
- **It never blocks twice.** When the hook input has `stop_hook_active` (Claude is
  already continuing because of a Stop hook), it returns at once. This means it cannot
  loop.
- **Nothing selected:** it returns at once without writing the state. This also holds
  with `state_shared: true`, where it runs per worktree.
- **A time limit.** The hook's timeout is 600 s. With
  [`--max-time 8m`](../reference/cli.md#time-limit), smates starts no test batch that
  would end after eight minutes, by recorded durations, so a change that selects a large
  part of the repository does not run into the timeout. `-- --max-time 4m` sets
  another limit, `-- --max-time 0` none.
- **Tests left for later do not block.** Tests the time limit left out, and tests whose
  suite [`requires`](../reference/configuration.md#requires) are not met here (a
  database or a browser this worktree lacks), are not run and not recorded. They stay
  selected for the next run, or for CI. The hook prints one line about them to stderr and
  lets Claude stop, unless other tests failed.
- **smates' own errors** (the cases where `smates run` would exit 2, 3 or 4) go to the
  user with exit 1 instead of blocking Claude. Failing tests still block when only the
  state could not be saved.
- Where `smates` is not installed, the hook does nothing, so a committed
  `.claude/settings.json` does not break teammates who lack smates.
- `SMATES_SKIP_HOOK=1` skips it.

### Settings files stay intact

smates splices its entry into the text of `settings.json`:

- Everything else (other keys and hooks, their order and formatting) stays byte for
  byte. Installing again changes nothing.
- `smates hook uninstall --claude` removes exactly that text, plus a `Stop` or `hooks`
  entry it leaves empty.
- A file smates cannot edit unambiguously (invalid JSON, a duplicate key, a `hooks` that
  is not an object, or a `Stop` that is not an array) is reported and left alone.

`smates hook status` shows both the project and the user hook.

## Codex Stop hook { #codex-stop-hook }

```bash
smates hook install --codex
```

This adds the same Stop hook to `.codex/hooks.json`, which Codex reads in a trusted
project (some Codex versions also need hooks turned on in their configuration). The
hook runs `smates hook codex-stop`, which behaves like `claude-stop` above: it blocks
with the failing tests, never blocks twice, defers what it cannot run here, and does
nothing where smates is not installed. `--agent` and `-- <run flags>` work as for
`--claude`; `--user` does not apply.

The command finds `smates` on `PATH` and runs in the directory Codex gives it, so the
same committed `.codex/hooks.json` works in every worktree of the repository. Other
hooks in the file stay byte for byte, and `smates hook uninstall --codex` removes only
smates' entry.

## See also

- [Use smates from a coding agent](coding-agents.md)
- [Parallel worktrees](worktrees.md): how worktrees share a state
- [Commands](../reference/cli.md)

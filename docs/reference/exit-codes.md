# Exit codes

smates' exit code tells "fix the code" (1) apart from "fix the setup" (2 to 4). Every
command uses the same codes.

| Code | Meaning | What to do |
|---|---|---|
| `0` | Success: the tests passed, or there was nothing to run. | Nothing. |
| `1` | Tests failed, whatever code the runner exited with. Also an interrupted run and a failing `smates doctor` check; never smates' own errors. | Fix the code or the test. |
| `2` | Usage error: an invalid command line. | Fix the command line (`smates <command> -h`). |
| `3` | Configuration error. | Fix `smates.yml` (`smates config ...`), or the name you passed (`smates list`). |
| `4` | Environment problem. | Run `smates doctor`; each failed check has a fix. |

## 1: tests failed

`smates run` exits 1 when any runner batch failed: a failing test, but also a non-zero
exit with no failing test known (a `TestMain` exiting 1, a teardown error, a coverage
threshold). The runner's own code (pytest's 2, for example) does not leak through: it
is in `exit_code` of the [`--report`](cli.md#smates-run) JSON and in the summary.

A run stopped with Ctrl-C, SIGTERM or SIGHUP also exits 1. What ran is recorded; the
tests that did not run stay selected.

## 2: usage error

- an unknown command or flag, or a missing argument (`smates why` without a file);
- an invalid flag value (`--shard 5/4`, `--output verbose`, `--orphans maybe`, an
  unknown `--format`);
- flags that cannot be combined (see [`smates run`](cli.md#smates-run));
- `--add` naming a test that is not part of the run;
- `--base` naming no commit (in a clone that is not shallow);
- `smates state merge` naming a file that does not exist;
- `smates state push` on a detached HEAD without `--ref` (or `SMATES_BRANCH`).

## 3: configuration error

- `smates.yml` does not parse, has an unknown key or an invalid value;
- an unknown runner, or a suite command that cannot be parsed;
- no suites at all (none configured and none detected);
- a suite or test named on the command line that the configuration does not define
  (`--suite`, `--only`, `smates deps <test>`).

A suite command that cannot be parsed stops the run where its batch would start: the
batches that ran before it are recorded and reported (log, summary, `--report`) first,
and the rest stay selected.

## 4: environment problem

- git is missing, or the directory is not a git repository;
- a suite command that cannot be started (not on `PATH`, not executable);
- a tool that `npx` or `bunx` would have to download (it is not in
  `node_modules/.bin`): install the dependencies first;
- in a recorded run, a runner process that exits 126 or 127 (the shell's "not
  executable" and "command not found", also through `npm test`, `yarn`, `pnpm exec` or
  `sh -c`) before any test reported a result;
- a state that cannot be saved (for example, the state's lock is held by another
  smates process: another worktree's run, or `smates watch` next to a pre-push hook);
- a state file that cannot be read at all (permission denied). A state that is damaged
  or written by a newer smates is only a warning: the command starts cold, and the next
  save replaces it;
- a `--report` file that cannot be written, a trace directory that cannot be created;
- `smates state push` with no state to push, and git failing to push or fetch it;
- a suite's [`requires`](configuration.md#requires) (a database, a browser) that are not
  met here;
- a `--base` missing from a shallow clone (fetch it: `fetch-depth: 0`, or
  `git fetch origin <branch>`), or git failing to diff against the base or to list the
  repository while selecting.

When a runner cannot start, or requirements are not met, the other suites still run and
are recorded. The suite's
tests stay selected. smates prints the failure (output, summary) first, then exits 4.
In `--output failures` mode the summary line reads `NOT STARTED` instead of `PASSED`.

??? note "Details: when 126/127 is a test failure instead"
    The 126/127 rule applies only to a recorded run of a runner that reports per-test
    results. With `--no-record`, or with the `custom` runner, a command may report
    nothing even when its tests ran, so a 126 or 127 exit counts as a test failure
    (exit 1).

### When failures and setup problems meet

Failing tests matter more. When tests of another suite failed in the same run, smates
exits 1 and prints the environment problem as a note. The same holds for a state that
could not be saved, or a `--report` that could not be written: exit 1 when tests
failed, else 4.

Any error smates does not classify exits 4, never 1: exit 1 always means a test
failed (or the run was cut short).

## In reports and CI outputs

| Where | Value |
|---|---|
| `exit_code` in `smates run --report` | The runners' worst exit code; `4` when the run is an environment problem without failing tests. |
| `exit-code` written to `$GITHUB_OUTPUT` by `smates run` | Same as `exit_code`. |
| `exit-code` output of the GitHub Action | The exit code of the smates command itself (`1` for failed tests). |
| `smates hook claude-stop`, `codex-stop` | `0` (allow, or block with a JSON decision when tests fail) or `1` (smates' own error, shown to you instead of the agent). It never exits 2. Tests left out by unmet `requires` or the time limit are not an error: `0`, with one line on stderr. |
| `smates hook run pre-push`, `pre-commit` (the git hooks) | `0` when the tests passed, or when the only tests left out need what this machine lacks (unmet `requires`: one line on stderr, the push or commit goes on, and they stay selected for CI); `1` for failing tests; otherwise the codes of `smates run`. |
| `smates hook install`, `uninstall` | `0` when done; `2` for an invalid command line; `3` for a settings file smates cannot edit (not JSON, an unexpected layout: fix it by hand); `4` for other setup failures (a file that cannot be read or written, a hook script already there). |

## See also

- [Commands](cli.md)
- [Troubleshooting](troubleshooting.md)
- [Coding agents](../guides/coding-agents.md)

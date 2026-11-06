# Troubleshooting

Answers to the problems people hit most often, each with the command that shows what
is going on. Start with `smates doctor`.

## Start here: smates doctor

```text
$ smates doctor
smates 1.2.3
  ok    repository /home/me/shop
  ok    config smates.yml
  ok    suite web (vitest, root packages/web): 42 test units; command: vitest run
  FAIL  suite e2e: no node_modules (or Yarn PnP loader) from e2e up to the repository root: ...
  warn  agent unavailable (no credentials: set ANTHROPIC_API_KEY (or ANTHROPIC_AUTH_TOKEN), ...
```

It exits 1 when a check fails. `smates doctor --format json` gives each check a stable
`id` and a `fix` (a command to run, a config change or a link). The checks are listed
under [`smates doctor`](cli.md#smates-doctor).

## A test runs every time

Ask smates why it was selected:

```bash
smates run --explain          # or: smates select --explain
```

| Reason | What it means | What to do |
|---|---|---|
| `failed: last run: fail` | It did not pass last time. | Fix it. If it passes on a rerun, check `smates flaky`. |
| `new: no recorded run` | No recording for this test in the state. | Check that the state is restored ([below](#everything-runs-on-every-ci-run)). |
| `changed: <file>` | That input changed since it last passed. | If the file changes on every run, see below. |
| `env changed: <VAR>` | A variable it reads has a different value. | If the value differs between runs or machines, add it to `env_ignore`. |

Then look at what the test depends on:

```bash
smates deps src/cart.test.ts
```

Common causes of a test that never settles:

- **A file the tests regenerate.** A test that reads a file it (or the build) rewrites
  on every run, in a git-ignored path, depends on its new content each time. A test
  that probes an ignored path and creates it runs once more, then settles; a test that
  reads what it generates itself runs again on every fresh checkout.
- **A `dir/**` dependency.** A command smates cannot see into (`make`, a binary) makes
  the test depend on every file below its directory. Declare the real inputs with
  `rules` and call the tool directly, or accept the extra runs.
- **A warning in the log.** `N test file(s) passed but left no trace (their process was
  likely killed before writing it); they will run again next time` means the test
  process exited before smates could save what it used (a `process.exit()` in a test, a
  killed worker). `N test unit(s) ran and passed but left no trace` (or `N test file(s)
  passed but none left a trace`) means no test process of the batch was traced at all:
  the suite command most likely runs the tests through a wrapper that drops smates'
  tracing environment (`NODE_OPTIONS`, `NODE_V8_COVERAGE`, `SMATES_*`; a task runner
  such as turbo or nx, `env -i`, a container) or replays cached results. Point the
  suite's `command` at the test runner itself, with `root:` set to its package, or pass
  that environment through.
- **Runner arguments.** A run with arguments after `--` never records a test as passed.
  Move permanent flags into the suite's `command`.
- **A unittest module written for pytest.** Its status stays unknown, so it runs every
  time; the log suggests `runner: pytest`.
- **A flaky test.** `smates flaky` lists tests whose outcome flipped with unchanged
  inputs. See [Shadow mode, flaky tests and stats](../guides/trust.md).

## A test that should have run was skipped

1. Check the dependency map: `smates why <changed file>` lists the tests that depend on
   it, `smates deps <test>` what the test depends on.
2. Check whether the change was an orphan (a change no test is known to depend on) that
   was ignored. The log says `ignored N unattributed change(s) (orphans: ignore)`,
   `skipped by caller` (`--orphans=skip`), or lists them as changes to untested code
   (`untested: skip`). See [Changes no test explains](../guides/orphans.md).
3. Check [Limitations](limitations.md): a server in another process, an environment
   read by enumeration, and code that runs in a browser are known blind spots, each
   with a workaround.
4. Measure: `smates run --shadow` runs everything and reports any failing test the
   selection would have skipped; `smates stats` shows the miss rate over time.

If none of these explains it, please
[open an issue](https://github.com/dotneet/smates/issues) with the output of
`smates run --explain`, `smates why <file>` and `smates deps <test>`.

## The same files are orphans on every commit

Files such as `.env.example`, deploy configs, or a frontend no suite covers keep making
whole suites run (or the agent get called). Let smates find them:

```bash
smates run --all                    # first: record real dependencies
smates orphans                      # replay the last 100 commits, suggest ignore: globs
smates orphans --apply --dry-run    # show the change to smates.yml
smates orphans --apply              # write it
```

Files that can change how tests run (CI workflows, Makefiles, Dockerfiles, toolchain
pins) are suggested for `inputs:` or `rules:` instead. To add one glob by hand:
`smates config add-ignore 'deploy/**'`. See [Changes no test explains](../guides/orphans.md).

## A new test file is not picked up

smates reads which files are tests from the runner's configuration. When it cannot
read it (a config built by a function, `mergeConfig(...)`, a spread), it falls back to
the default patterns and warns:

```bash
smates list -v        # where each suite's patterns come from, the patterns, every unit
```

If the new file is missing from the list, set `tests:` (and `exclude:`) for the suite
in `smates.yml`. `smates doctor` reports the same warning (`suite-test-discovery`). See
[Configure suites](../guides/configuration.md).

## Exit code 4: the runner cannot start

```text
smates: web: the runner exited with code 127 before any test reported a result: ...
```

The suite command is not installed, not on `PATH`, or not executable. `smates doctor`
names the suite and the command to install it. smates never lets `npx` or `bunx`
download a runner: install the project's dependencies first (`npm ci`, `pnpm install`,
`uv sync`, ...). See [Exit codes](exit-codes.md#4-environment-problem).

## Everything runs on every CI run

The state was probably not restored, so every test is new.

- **GitHub Actions:** open the "Restore smates state" step log and check for a cache
  hit. `state-path` must match `state` in `smates.yml`. Jobs that run different tests
  (matrix entries, shards, suites split across jobs) each need their own `cache-key`.
  See [GitHub Actions](../ci/github-actions.md).
- **Other CI:** restore `.smates/` before `smates run`, save it after, also when tests
  failed. See [Other CI systems](../ci/other-ci.md).
- **Check:** `smates state show` prints where the state is and what it recorded;
  `smates doctor` warns `no recorded state`.
- **First run:** without any state, smates compares against the base branch, which
  needs history (`fetch-depth: 0`). Without it, everything runs once.

On the default branch every test runs by design, to refresh the state. Override with
`--select`.

## A new worktree starts cold, or from the wrong state

A linked git worktree without a state is seeded once, from the main worktree's state or
else the remote one. `smates state show` prints `seeded: from ...`. The snapshot is
kept only when it was recorded on a commit in the worktree's history; otherwise
changed files are judged against the default branch.

- Turn seeding off: `state_seed: false` or `SMATES_NO_STATE_SEED=1`.
- Share what every worktree learns: `state_shared: true`.
- Start from the remote state at any time: `smates state pull`. A worktree is seeded
  only once: a marker next to the state (`.smates/.state.json.gz.seeded`) remembers it,
  also after `smates state clear`.

With `state_shared: true`, saving waits up to a minute for another worktree's run to
release the lock, then gives up with exit code 4 (the test results are still
reported). A lock left behind by a crashed process is taken over after two minutes.
See [Parallel worktrees](../guides/worktrees.md).

## Recording is slow

Recording (the first run, `--all`, and every run on the default branch) costs more than
a selective run. Typical causes:

- **Python 3.8 to 3.11** is traced with `sys.setprofile`. Python 3.12 and later use
  `sys.monitoring`, which is much cheaper.
- **Go with `granularity: func`** records one process per test function. Raise
  `record_jobs` to record several functions of a package at once, if they do not share
  state.
- **`isolate: true`** starts the runner once per file while recording. Leave it off
  unless the dependencies of files run in a shared worker need to be exact.
- **A low `record_jobs`** for runners recorded one process per file (bun, `isolate`).

Selective runs on pull requests are not affected: they only start the selected tests.

## The agent is not called

The log says why, for example `agent unavailable (...); running affected suites in
full`. `smates doctor` reports the same (`agent` check).

| Cause | Fix |
|---|---|
| No API key in the environment | Set `ANTHROPIC_API_KEY`, `OPENAI_API_KEY` or `OPENROUTER_API_KEY` (see [Variables smates reads](environment-variables.md#agent-credentials)). |
| `agent.enabled: never`, or `orphans:` is not `agent` | Change `smates.yml`. |
| `--no-agent` | Git hooks pass it unless installed with `--agent`; `smates watch` needs `--agent`. |
| A sharded run (`--shard`) | Shards only reuse a decision cached by an unsharded `smates select` or `smates run` on the same change. |
| A pull request from a fork | Forks get no secrets; the `orphans` fallback applies. |
| No orphans | The agent is only called for changes no test depends on (and to explain failures). |

See [The LLM agent](../guides/agent.md).

## See also

- [Exit codes](exit-codes.md)
- [Limitations](limitations.md)
- [Commands](cli.md)

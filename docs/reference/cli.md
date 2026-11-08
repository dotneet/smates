# Commands

Every `smates` command and flag, grouped by task. Use this page to look up an option.
The guides explain when and why you use it.

| Command | What it does |
|---|---|
| [`smates run`](#smates-run) | Select the affected tests, run them with tracing, update the state |
| [`smates select`](#smates-select) | Print the selection without running anything |
| [`smates watch`](#smates-watch) | Rerun the affected tests on every change |
| [`smates why`](#smates-why) | List the tests that depend on a file or variable |
| [`smates deps`](#smates-deps) | List the files a test depends on |
| [`smates list`](#smates-list) | List suites and test units |
| [`smates doctor`](#smates-doctor) | Check the setup |
| [`smates flaky`](#smates-flaky) | List tests proven flaky |
| [`smates stats`](#smates-stats) | Summarize time saved, shadow misses, orphans and agent use |
| [`smates orphans`](#smates-orphans) | Replay recent commits and suggest `ignore:` globs |
| [`smates init`](#smates-init) | Write `smates.yml` (and optionally a GitHub workflow) |
| [`smates config`](#smates-config) | Edit `smates.yml` safely |
| [`smates guide`](#smates-guide) | Print the guide for coding agents |
| [`smates state`](#smates-state) | Inspect, move and clear the recorded state |
| [`smates hook`](#smates-hook) | Install a git pre-push/pre-commit hook, or a Claude Code or Codex Stop hook |
| [`smates version`](#smates-version-and-help) | Print the version |

Run `smates <command> -h` to print a command's flags (exit code 0); for `config`,
`state` and `hook` it also lists the subcommands. Flags take one dash or two (`-all`
and `--all` are the same). Flags may come before or after positional arguments
(`smates why src/a.ts --format json`); `--` ends the flags. In `run`, `select` and
`watch`, arguments after `--` go to the test runner.

## Common flags

The commands that load the project (`run`, `select`, `watch`, `why`, `deps`, `list`,
`doctor`, `flaky`, `orphans`) share these flags. There are no global flags before the
command name.

| Flag | Value | Default | Meaning |
|---|---|---|---|
| `-C` | dir | current directory | Run as if smates was started in `dir`. |
| `--config` | file | `smates.yml` at the repository root | Config file to read. smates looks for `smates.yml`, `smates.yaml`, `.smates.yml` and `.smates.yaml`, in that order. |
| `--state` | file | `.smates/state.json.gz` (or `state:` in `smates.yml`) | State file to read and write. |
| `--suite` | name | all suites | Limit the command to a suite. Repeatable. An unknown name exits with code 3. |
| `-v` | | off | Verbose output (for `run`/`select`: the selected tests and their reasons, like `--explain`). |

`--format` takes `text` (the default) or `json` on every command that has it, except
`smates select`, which has more formats. A JSON report goes to stdout; log lines go to
stderr. [JSON output](json-output.md) describes every field.

Exit codes are the same for every command: see [Exit codes](exit-codes.md).

## Running tests

### smates run

```bash
smates run [flags] [-- runner args]
```

Selects the tests your changes can affect, runs them with tracing, and records what
each test used. On CI, a build of the default branch (or of a
[`full_run_branches`](configuration.md#top-level-keys) branch) that is not for a pull
request (a push, a scheduled or a manually started build) runs every test instead. A
GitHub merge queue build (`merge_group`) counts as a pull request.
See [How smates decides](../getting-started/how-it-works.md).

| Flag | Value | Default | Meaning |
|---|---|---|---|
| `--all` | | off | Run every test and rewrite the recorded state. |
| `--select` | | off | Select tests even where the default is a full run (the default branch on CI). |
| `--base` | rev | auto | Git revision to diff against for tests that have no recording yet. Default: the merge base with the pull request's target or the default branch. In CI such tests run as new unless `--base` is given. |
| `--explain` | | off | Print why each test was selected (`[changed: src/price.ts]`). |
| `--only` | `SUITE:UNIT` | | Run exactly this test unit and record it as usual. Repeatable. Other tests and changes are left for the next run. |
| `--add` | `SUITE:UNIT` | | Also run this test. The unit may be a test file path; `SUITE:` may be left out when only one suite has it. Repeatable. Never removes tests. A name that matches no test exits with code 2. |
| `--orphans` | `agent`, `full`, `skip` | `orphans:` in `smates.yml` | How to handle changes no test depends on, for this run: ask the agent, run the affected suites in full, or apply no policy (explicit opt-out). See [Changes no test explains](../guides/orphans.md). |
| `--no-agent` | | off | Do not call the agent, neither for unattributed changes (the affected suites run in full instead) nor to explain failures. |
| `--no-record` | | off | Run without tracing and without updating the state. |
| `--shadow` | | off | Run every test, but report the failing tests the selection would have skipped. With `--shard`, every test the shard owns. See [Shadow mode](../guides/trust.md#shadow-mode). |
| `--shard` | `I/N` | | Run shard `I` of `N` of the selected tests. Tests are assigned to shards by a stable hash of suite and test ID; the assignment depends on `N`, so all shards of a run use the same `N`. Sharded runs do not call the agent. |
| `--fail-fast` | | off | Start no further test batch after one fails. Tests run likeliest-to-fail first. |
| `--output` | `full`, `failures` | `$SMATES_OUTPUT`, else `full` | `full` streams the runner output. `failures` prints only the output of failing tests, then a short summary with a command that reruns them. `agent` is accepted as an alias of `failures`. |
| `--report` | file | | Write a [JSON report](json-output.md#smates-run-report) (selection, per-test results, shadow escapes, `exit_code`) to `file`. `-` writes it to stdout and moves all other output to stderr. |
| `--markdown` | file | | Write the summary as Markdown to `file` (replaced), for a pull request comment. |
| `--summary` | file | `$GITHUB_STEP_SUMMARY` on GitHub Actions | Append the Markdown summary to `file`. |
| `--budget` | duration | `90s` | When the recorded durations of the selected tests add up to more than this, print how to split the run first. `0` turns it off. |
| `--max-time` | duration | `0` (no limit) | Start no test batch that would end after this, by recorded durations. The tests left out stay selected for the next run. See [Time limit](#time-limit). |
| `--dry-run` | | off | Print the runner commands instead of running them. |

Plus the [common flags](#common-flags).

Combinations that are rejected (exit code 2):

- `--only` with `--all`, `--shadow`, `--add` or `--orphans`.
- `--shadow` with `--fail-fast` or `--max-time`.
- `--dry-run` with `--report`, `--markdown` or `--summary` (use
  `smates select --format json|markdown` instead).

In `run` and `watch`, arguments after `--` reach every suite's runner
(`smates run -- -t name`). They may filter tests, so a run or a watch cycle with them
never records a test as passed. Put permanent flags in the suite's `command` instead.

??? note "Details: what `--output failures` prints"
    Each runner batch's output is buffered and printed only when the batch fails. For
    runners that report each failing test's own output (vitest, jest, pytest, `go test`,
    JUnit) only that is printed; otherwise the batch's output, at most 200 lines (the
    first 30 and the last 170). smates' progress lines are left out. After 30 s without
    output it prints `smates: still running <suite> (...)`; `SMATES_HEARTBEAT` changes
    the interval. See [Coding agents](../guides/coding-agents.md).

#### Time limit { #time-limit }

`--max-time 8m` keeps a run within about eight minutes:

- smates cuts each suite's tests into batches of about a quarter of the limit, by the
  durations recorded for them. A test counts as measured when its last pass, failure
  or skip was recorded with a duration above 0 ms; any other test (never recorded,
  recorded with another outcome, or recorded at 0 ms) counts as the mean of the
  suite's measured tests. A suite with no measured test (e.g. its runner reports 0 ms
  for every test) stays one batch, which starts only while less than half the limit is
  used. Each Go package a batch builds adds a second, as recorded durations leave out
  compiling and linking the test binaries.
- Batches start in the usual order, the likeliest to fail first. Before each one,
  smates checks whether it would end after the limit. Its estimate grows by how much
  longer than estimated the batches that ran took, on average per batch (starting the
  runner and building take about the same time for a short batch as for a long one):
  the batches of the same suite, or of every suite before a suite's first batch. The
  requirements are checked first: a batch whose units are partly left out by unmet
  requirements is estimated by the units that run, and one with no unit left does not
  count against the limit (the next batch is checked instead). If it would end after
  the limit, smates starts nothing more.
- The first batch always starts, so every run makes progress. A batch that is running
  is never stopped.
- The tests left out are not run and not recorded. They stay selected, and a later run
  runs them in the usual order (by priority, then shortest recorded duration first), so
  a long test can be left out again while new changes keep selecting others; CI runs
  them all. The log, the summary (`Time limit`) and the
  [report](json-output.md#smates-run-report) (`max_time_reached`, `not_run`) list them.

When everything that ran passed, the run exits 0. Do not use `--max-time` to gate a
merge on CI: tests it left out did not run. The Claude Code and Codex Stop hooks use
`--max-time 8m` by default (see [Git hooks](../guides/git-hooks.md#claude-code-stop-hook)).

#### Suites with unmet requirements { #unmet-requires }

Units of a suite whose [`requires`](configuration.md#requires) are not met do not
start. They stay selected and are not recorded; the log names each failed check. The
run exits 4 when no test failed, else 1. The [report](json-output.md#smates-run-report)
lists the checks under `unmet_requires` and the units under `not_run`.

### smates select

```bash
smates select [flags]
```

Prints the selection without running anything. It may still call the agent, and then
saves its answer in the state so the next `smates run` on the same change reuses it.
On a CI build of a [full-run branch](configuration.md#top-level-keys) that is not for a
pull request (a merge queue build counts as a pull request), it selects every test, as `smates run` runs them all there: outputs that gate jobs (`skip`, `suites`)
never skip the full run that refreshes the state.

| Flag | Value | Default | Meaning |
|---|---|---|---|
| `--format` | `text`, `files`, `json`, `github`, `markdown` | `text` | Output, see below. |
| `--all` | | off | Select everything. |
| `--base` | rev | auto | As for `run`. |
| `--explain` | | off | Print why each test was selected (to stderr). |
| `--add` | `SUITE:UNIT` | | As for `run`: preview the result of adding tests. |
| `--orphans` | `agent`, `full`, `skip` | `orphans:` in `smates.yml` | As for `run`. |
| `--no-agent` | | off | Do not call the agent. |
| `--shard` | `I/N` | | Only shard `I` of `N` of the selection. |
| `--max-shards` | `N` (1-256) | | Also size a shard count for the selection: `0` when nothing is selected, else the recorded duration of the selected tests divided by `--shard-time`, rounded up, within `1` to `N`. On such a full-run build it is sized for every test, as the selection is. See [Size the shards to the selection](../ci/github-actions.md#size-the-shards-to-the-selection). Not with `--shard`. |
| `--shard-time` | duration | `10m` | With `--max-shards`: recorded test time to aim for per shard. A selected test without a recorded duration counts as its suite's median; a suite with selected tests and no recorded duration at all gives `N`. |
| `--markdown` | file | | Also write the Markdown summary to `file`. |

Plus the [common flags](#common-flags).

| `--format` | stdout |
|---|---|
| `text` | One selected test ID per line; the selection summary goes to stderr. |
| `files` | One selected test ID per line, nothing on stderr. |
| `json` | The full selection: `selected`, `total`, `selected_tests` (per suite), per-test reasons, `orphan_candidates`, the agent's decision, and `shards` with `--max-shards`. See [JSON output](json-output.md#smates-select). |
| `github` | `selected`, `total`, `run-all`, `skip`, `tests` and `suites` (and with `--max-shards`, `shards` and `shard-matrix`) as step outputs, written to `$GITHUB_OUTPUT` (stdout when it is unset); the summary is appended to `$GITHUB_STEP_SUMMARY`. See [Outputs](../ci/github-actions.md#outputs). |
| `markdown` | The Markdown summary used for pull request comments. |

### smates watch

```bash
smates watch [flags] [-- runner args]
```

Polls the working tree and reruns the affected tests after every change. Keys (when
stdin is a terminal): `a` runs all, `f` reruns the failed tests, Enter or `r` reruns,
`q` quits. See [Watch mode](../guides/watch.md).

| Flag | Value | Default | Meaning |
|---|---|---|---|
| `--interval` | duration | `500ms` | How often to poll the working tree. Must be positive. |
| `--debounce` | duration | `300ms` | Wait until no file changed for this long before running. |
| `--agent` | | off | Ask the agent about unattributed changes. Without it the orphan policy applies without the agent. |
| `--clear` | | off | Clear the screen before each run. |
| `--fail-fast` | | off | In each run, start no further test batch after one fails. |

Plus the [common flags](#common-flags).

## Inspecting

### smates why

```bash
smates why [flags] <file | $VAR>
```

Lists the tests that depend on a file, and how (`static`, `recorded`, a `depends_on`
declaration, ...). With `'$TZ'` (quoted, so the shell does not expand it) it lists the
tests that read the environment variable `TZ`.

Flags: `--format text|json` ([JSON output](json-output.md#smates-why-deps-list)), plus the
[common flags](#common-flags).

### smates deps

```bash
smates deps [flags] <test unit>
```

Lists the dependencies of one test unit: a test file, a Go package directory, or
`pkg#TestName` for a suite with `granularity: func`. Each line says whether the
dependency is `static` (static analysis, `inputs`, `rules`, `depends_on`), `recorded`
(seen while the test ran) or both. A name that is not a test unit exits with code 3.

Flags: `--format text|json` ([JSON output](json-output.md#smates-why-deps-list)), plus the
[common flags](#common-flags).

### smates list

```bash
smates list [flags]        # alias: smates ls
```

Prints one line per suite: name, runner, root, number of units, command and where the
test patterns come from (`tests=jest.config.js testMatch`). With `-v` it also prints
the patterns and every unit. A configuration smates could not read is reported as a
discovery warning on stderr. The [JSON report](json-output.md#smates-why-deps-list) has
`tests_source`, `test_patterns`, `units` and `discovery_warning`.

Flags: `--format text|json`, plus the [common flags](#common-flags).

### smates doctor

```bash
smates doctor [flags]
```

Checks the setup and prints one line per check. It exits with code 1 when a check
fails (warnings do not). In [JSON](json-output.md#smates-doctor) each check has a stable
`id`, a `status` (`ok`, `warn`, `fail`), a `message` and a `fix` (`command`, `config` key
and value, or `doc`).

| Check `id` | What it checks |
|---|---|
| `git-installed`, `repository`, `project-load`, `config` | git is on `PATH`, the repository and `smates.yml` load |
| `suites-detected`, `suite-units` | there are suites, and test files match each one |
| `suite-not-detected` | without `smates.yml`: a package configures a test runner (a vitest or jest configuration file, a per-project `vitest.*.config.*`, a `vite.config.*` that imports vitest or sets `test`; not a plain Vite app) but got no suite (no test file smates can find there); `smates init` prints the same note |
| `suite-tests-source`, `suite-test-discovery` | where the test patterns come from; a runner config smates could not read |
| `suite-launcher`, `suite-dependencies-installed` | the suite command can start; `node_modules` is installed |
| `toolchain-node` | with a vitest, jest or node suite: `node` is on `PATH` and is version 18 or later (tracing needs it) |
| `toolchain-go`, `toolchain-bun`, `toolchain-python`, `suite-engines-node` | toolchains, and the Node version against `engines.node` |
| `service-reachable`, `local-server-reachable`, `env-var-set` | services and `localhost` URLs the code uses; variables it reads that are not set |
| `suite-server-dependency` | an end-to-end-looking suite without `depends_on`, `serves` or `rules` |
| `suite-rule-reaches` | how many test units each rule with `reaches` applies to; a warning for one that applies to none |
| `state-recorded` | a recorded state exists, and from which commit |
| `state-skipped` | test units that skipped themselves in their last run (not proven: they run every time until they pass) |
| `agent`, `ci` | the agent is available; the CI detected and whether it runs everything by default |
| `workflow-test-step` | a GitHub workflow step that runs the tests without smates |
| `workflow-no-seed` | a GitHub workflow that runs smates only on pull requests, with no workflow running it on the default branch under the same `cache-key`: nothing records the state pull requests start from |

Flags: `--format text|json`, plus the [common flags](#common-flags).

### smates flaky

```bash
smates flaky [flags]
smates flaky --clear [unit...]
```

Lists the tests whose outcome flipped while none of their inputs changed, with the
evidence (flips, last commit, recent outcomes, environment variables read). See
[Shadow mode, flaky tests and stats](../guides/trust.md).

| Flag | Value | Default | Meaning |
|---|---|---|---|
| `--clear` | | off | Forget the flake evidence of the given units (of all units when none is given). Units are only accepted with `--clear`. |
| `--format` | `text`, `json` | `text` | Output ([JSON](json-output.md#smates-flaky)). |

Plus the [common flags](#common-flags).

### smates stats

```bash
smates stats [flags]
```

Sums up the runs recorded in the state: skip rate, recorded test time saved (also
extrapolated to 30 days), shadow misses, orphans by handling, agent calls with tokens
and cost, flaky flips.

| Flag | Value | Default | Meaning |
|---|---|---|---|
| `--since` | when | all | Only runs since a date (`2026-01-02`), an RFC 3339 time, or an age (`30d`, `2w`, `12h`). |
| `--branch` | branch | all | Only runs on this branch. |
| `--ci` | | off | Only runs on CI. |
| `--recent` | n | `5` | Text output: list the last `n` runs. |
| `--format` | `text`, `json` | `text` | Output ([JSON](json-output.md#smates-stats)). |
| `-C` | dir | current directory | Run as if started in `dir`. |
| `--state` | file | `.smates/state.json.gz` | State file. |

`smates stats` takes no `--config`, `--suite` or `-v`.

### smates orphans

```bash
smates orphans [flags]
smates orphans --apply [--dry-run]
```

Replays recent first-parent commits against the current dependency map, ranks the files
that would have been orphans (changes no test depends on), and prints a ready-to-paste
`ignore:` block, plus candidates for `inputs:` or `rules:`. Run it after
`smates run --all`. See [Changes no test explains](../guides/orphans.md).

| Flag | Value | Default | Meaning |
|---|---|---|---|
| `--history` | n | `100` | Replay the last `n` first-parent commits. `0`: no limit. |
| `--since` | rev | | Replay the commits after `rev`. Without an explicit `--history`, there is no count limit. |
| `--apply` | | off | Add the suggested `ignore:` globs to `smates.yml` (and nothing else). Comments and layout are kept. |
| `--dry-run` | | off | With `--apply`: print the diff instead of writing. |
| `--format` | `text`, `json` | `text` | Output ([JSON](json-output.md#smates-orphans)). |

Plus the [common flags](#common-flags).

## Setup

### smates init

```bash
smates init [flags]
```

Detects the suites of the repository, writes `smates.yml` and adds `.smates/` to
`.gitignore`. The `go test` flags that change how a Go suite's tests run (`-p`,
`-tags`, `-race`, `-timeout`) go into its `command:` when the workflow steps or the
Makefile test target running it use them (`-tags` only when every run of all the
suite's unit tests agrees, see [Switch existing workflows](../ci/github-actions.md#switch-existing-workflows));
the environment those steps set is written as a comment only. When a workflow already
runs smates, `--github` writes no second one (unless `--force`). It refuses to overwrite an existing config unless `--force`; with
`--github` or `--agents-md` over an existing config, only that part runs. See
[Quick start](../getting-started/quickstart.md).

| Flag | Value | Default | Meaning |
|---|---|---|---|
| `--github` | | off | Also write `.github/workflows/smates.yml`. When an existing workflow already runs the tests, print a patch that switches its test steps to smates instead ([what it patches](../ci/github-actions.md#switch-existing-workflows)). |
| `--patch` | file | print it | With `--github`: write that patch to `file` (for `git apply`). |
| `--agents-md` | | off | Add a short smates section to `AGENTS.md` (or `CLAUDE.md`) for coding agents. |
| `--force` | | off | Overwrite an existing `smates.yml`. |
| `--stdout` | | off | Print the config instead of writing it. |
| `--format` | `text`, `json` | `text` | `json` reports the suites, the files written and `needs_review` items, each with its evidence and a recommended change ([JSON output](json-output.md#smates-init)). |
| `-C` | dir | current directory | Run as if started in `dir`. |

### smates config

```bash
smates config add-ignore <glob>... [flags]
smates config add-input <glob>... [flags]
smates config set <key> <value> [flags]
```

Edits `smates.yml` without hand-editing YAML. Comments and layout are kept, and the
result is validated before it is written. Flags may come after the arguments.

| Subcommand | Effect |
|---|---|
| `add-ignore <glob>...` | Append to the top-level `ignore:` (deduplicated). |
| `add-input <glob>...` | Append to the top-level `inputs:` (deduplicated). |
| `set <key> <value>` | Set a scalar key: `version`, `orphans`, `untested`, `state`, `state_seed`, `state_shared`, `flaky.retry`, `agent.enabled`, `agent.provider`, `agent.model`, `agent.max_turns`, `agent.timeout`, `agent.command`, `agent.effort`, `agent.instructions`, `agent.explain_failures`, `agent.explain_max`. |

| Flag | Value | Default | Meaning |
|---|---|---|---|
| `--dry-run` | | off | Print the diff, write nothing. |
| `--format` | `text`, `json` | `text` | Output ([JSON](json-output.md#smates-config)). |
| `--config` | file | `smates.yml` at the repository root | Config file to edit. It must exist (run `smates init` first). |
| `-C` | dir | current directory | Run as if started in `dir`. |

Use `--` before a glob that starts with `-`.

### smates guide

```bash
smates guide
```

Prints the one-page guide for coding agents
([Agent guide](../AGENTS.md)). No flags.

### smates version and help

```bash
smates version      # also: smates --version, smates -v
smates help         # also: smates -h, smates --help
```

`-v` before a command name prints the version; after a command name it means verbose.

## State

### smates state

```bash
smates state show [--format json]
smates state path
smates state pull [--remote origin] [--ref REF]
smates state push [--remote origin] [--ref REF]
smates state merge [-o file] <state file>...
smates state clear
```

| Subcommand | Effect |
|---|---|
| `show` | Where the state lives (and whether it is shared or was seeded), the recorded commit, the snapshot size and per-suite counts. |
| `path` | Print the state file's path. |
| `pull` | Fetch the state from a git ref and save it. Without `--ref` it tries `refs/smates/state/<branch>`, then the ref of the branch a merge or pull request targets (its base branch, when the CI reports one), then the default branch's ref. With `state_shared: true` the pulled entries are merged in. |
| `push` | Push the state as a commit of its own to `refs/smates/state/<branch>` (or `--ref`) on the remote (force-push, hooks skipped). |
| `merge` | Merge the given state files into the state (or into `-o file`). |
| `clear` | Delete the state. In a linked worktree it is then not seeded again. With `state_shared: true` this clears it for every worktree. |

| Flag | Value | Default | Meaning |
|---|---|---|---|
| `--remote` | name | `origin` | `pull`/`push`: git remote. |
| `--ref` | ref | `refs/smates/state/<branch>` | `pull`/`push`: git ref. |
| `-o` | file | the state file | `merge`: output file. |
| `--format` | `text`, `json` | `text` | `show`: output ([JSON](json-output.md#smates-state-show)). |
| `--json` | | off | `show`: deprecated alias of `--format json`. |
| `--state` | file | `.smates/state.json.gz` | State file. |
| `-C` | dir | current directory | Run as if started in `dir`. |

The branch is `$SMATES_BRANCH`, else the branch the CI builds, else the checked-out
branch. On a detached HEAD without either, `push` needs `--ref`. See
[Other CI systems](../ci/other-ci.md) and [Parallel worktrees](../guides/worktrees.md).

## Hooks

### smates hook

```bash
smates hook install [--hook pre-push|pre-commit] [--append] [--agent] [-- run flags]
smates hook install --claude [--user] [--agent] [-- run flags]
smates hook install --codex [--agent] [-- run flags]
smates hook uninstall [--hook ...] | [--claude [--user]] | [--codex]
smates hook status
```

`install` writes a git hook that runs `smates run --no-agent` (plus the run flags after
`--`) and stops the push or commit when a selected test fails. With `--claude` it adds a
Stop hook to `.claude/settings.json` instead, and with `--codex` to `.codex/hooks.json`.
`uninstall` removes only what smates wrote. `status` shows the git hook and the Claude
Code and Codex hooks. See [Git hooks and Claude Code](../guides/git-hooks.md).

| Flag | Value | Default | Meaning |
|---|---|---|---|
| `--hook` | `pre-push`, `pre-commit` | `pre-push` | Git hook to manage. Not with `--claude` or `--codex`. |
| `--append` | | off | `install`: add smates to the end of an existing hook script. Not with `--claude` or `--codex`. |
| `--agent` | | off | `install`: let the hook call the agent (otherwise it passes `--no-agent`). |
| `--claude` | | off | A Claude Code Stop hook in `.claude/settings.json` instead of a git hook. |
| `--codex` | | off | A Codex Stop hook in `.codex/hooks.json` instead of a git hook. Not with `--claude` or `--user`. |
| `--user` | | off | With `--claude`: use `~/.claude/settings.json` (active in every project that has a `smates.yml`). |
| `-C` | dir | current directory | Run as if started in `dir`. |

The installed hooks call internal subcommands, which you normally do not run yourself:

- `smates hook run pre-push|pre-commit [-- run flags]`: what the git hook runs. Tests
  left out by unmet [`requires`](configuration.md#requires) do not stop the push or
  commit: it prints one line about them and exits 0 (they stay selected for CI).
- `smates hook claude-stop|codex-stop [--require-config] [-- run flags]`: what the
  Claude Code and Codex Stop hooks run (`--output failures --max-time 8m` is implied;
  a `--max-time` in the run flags wins, and with `--shadow` none is added). It exits 0 and prints
  `{"decision": "block", ...}` when tests fail, or exits 1 on smates' own errors. Tests
  left out by the time limit or by unmet [`requires`](configuration.md#requires) do not
  block: it prints one line about them to stderr and exits 0.

All of them do nothing when `SMATES_SKIP_HOOK` is set to a value other than `0` or
`false`.

## See also

- [smates.yml](configuration.md): every configuration key
- [JSON output](json-output.md): every field of the JSON reports
- [Exit codes](exit-codes.md)
- [Variables smates reads](environment-variables.md)

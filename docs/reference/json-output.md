# JSON output

The JSON that smates prints for programs and coding agents, field by field. JSON goes to
stdout (`run --report` writes to its file unless it is `-`); log lines go to stderr.

| Command | Output |
|---|---|
| [`smates select --format json`](#smates-select) | The selection |
| [`smates run --report <file>`](#smates-run-report) | The selection, plus the results of the run |
| [`smates stats --format json`](#smates-stats) | Totals over the recorded runs |
| [`smates flaky --format json`](#smates-flaky) | Tests proven flaky |
| [`smates init --format json`](#smates-init) | What init wrote, and what needs a decision |
| [`smates why`, `deps`, `list --format json`](#smates-why-deps-list) | Dependencies, suites and units |
| [`smates doctor --format json`](#smates-doctor) | The checks |
| [`smates state show --format json`](#smates-state-show) | The recorded state |
| [`smates orphans --format json`](#smates-orphans) | Orphans in recent commits |
| [`smates config ... --format json`](#smates-config) | An edit of `smates.yml` |

Notes on the tables:

- Fields marked *optional* are left out when they are empty (or `false`, or `0`).
- Some objects use suite names or test IDs as keys: `{"web": {"src/a.test.ts": "pass"}}`.
- Times are RFC 3339 strings. Durations are integers in milliseconds (`_ms`).

## smates select { #smates-select }

`smates select --format json` prints the selection without running anything.

```json
{
  "mode": "state",
  "suites": [
    {
      "name": "go",
      "tests": [
        {
          "id": "cart",
          "selected": true,
          "reason": "changed",
          "changed": ["price/price.go"],
          "via": {"price/price.go": "import"},
          "duration_ms": 481
        }
      ],
      "all": true
    }
  ],
  "changed": ["notes.txt", "price/price.go"],
  "orphans": ["notes.txt"],
  "orphan_handling": {"notes.txt": "policy"},
  "selected": 1,
  "total": 1,
  "selected_tests": {"go": ["cart"]},
  "orphan_candidates": [
    {"path": "notes.txt", "status": "added", "referenced_by": [], "suites": ["go"], "handling": "policy"}
  ]
}
```

| Field | Type | Meaning |
|---|---|---|
| `mode` | string | How the selection was made: see [Modes](#modes). |
| `suites` | array | One entry per suite: see [Suites and tests](#suites-and-tests). |
| `changed` | array of strings, or `null` | The changed files the selection looked at. `null` when there are none. |
| `orphans` | array of strings, or `null` | Changed files no test depends on (see [Changes no test explains](../guides/orphans.md)). `null` when there are none. |
| `left_out` | array of strings, optional | Changes no test depends on that belong only to suites left out by `--suite` (not orphans of this run). |
| `orphan_handling` | object, optional | Orphan → how it was handled: see [Orphan handling](#orphan-handling). |
| `notes` | array of strings, optional | Notes on the selection that the log also prints (no recorded state, orphans skipped by the caller, ...). |
| `selected` | integer | Number of selected test units. |
| `total` | integer | Number of test units. |
| `selected_tests` | object | Suite → the IDs of its selected test units (an empty array when none). |
| `orphan_candidates` | array | One entry per orphan, for a caller that decides about orphans itself (`--add`, `--orphans`): see [Orphan candidates](#orphan-candidates). |
| `agent` | object, optional | The agent's decision, when the agent was asked or its cached answer was used: see [Agent decision](#agent-decision). |
| `agent_note` | string, optional | Why the agent was not used (unavailable, failed, or a sharded run). |
| `shards` | integer, only with `--max-shards` | The shard count sized for the selection (present even when `0`: nothing is selected). |

### Modes { #modes }

| `mode` | Meaning |
|---|---|
| `state` | Selected against the recorded state. |
| `diff` | No recorded state: selected by static analysis against the base revision. |
| `all` | Every test: `--all`, a full-run build on CI, or no recorded state and no base revision. |

### Suites and tests { #suites-and-tests }

Each entry of `suites`:

| Field | Type | Meaning |
|---|---|---|
| `name` | string | Suite name. |
| `tests` | array | Every test unit of the suite, selected or not. |
| `all` | boolean | `true` when every test unit of the suite is selected. |

Each entry of `tests`:

| Field | Type | Meaning |
|---|---|---|
| `id` | string | Test unit ID (a test file, a Go package directory, or `pkg#TestName`). |
| `selected` | boolean | Whether the unit is selected. |
| `reason` | string | Why: see [Selection reasons](#selection-reasons). `unaffected` for a unit that is not selected. |
| `detail` | string, optional | More about the reason (`last run: fail`, `references <file>`, the agent's reason). |
| `changed` | array of strings, optional | The changed dependencies that selected it: files, or `$NAME` for an environment variable. |
| `via` | object, optional | Entry of `changed` → how it reached the test: see [Via](#via). |
| `service` | object, optional | Entry of `changed` reached via `service` → the declaration (`depends_on suite api`, `serves backend/**`). |
| `duration_ms` | integer, optional | The duration recorded for the unit. |
| `shard` | integer, optional | With `--shard` or `--max-shards`: the shard that owns the unit (from 1). |
| `priority` | integer, optional | In `smates run`: how early the unit starts. `1`: it did not pass last time. `2`: its test file, or a file it imports directly, changed. `3`: something else it depends on changed (an indirect import, traced data, configuration), or reason `reference`, `agent`, `caller` or `orphan`. `4`: any other reason (`new`, `volatile`, `expired`, `unproven`, `only`, `all`, `rerun`). |

### Selection reasons { #selection-reasons }

The values of `reason`. [Why a test was selected](../getting-started/how-it-works.md#why-a-test-was-selected)
shows how `--explain` prints them.

| `reason` | Meaning |
|---|---|
| `all` | A full run. |
| `new` | The state has no entry for the test. |
| `changed` | An input changed since the test last ran. |
| `failed` | The test did not pass last time. |
| `unproven` | The test skipped itself last time, so its pass is not proven. |
| `volatile` | The suite's [`volatile`](configuration.md#suite-keys) lists it: it runs every time. |
| `expired` | Its last recorded run is older than the suite's [`max_age`](configuration.md#suite-keys). |
| `orphan` | The orphan policy ran the suite in full. |
| `reference` | The test names an orphan file. |
| `agent` | The agent added it. |
| `diff` | Diff mode: a file the test statically depends on changed since the base revision. |
| `rerun` | `smates watch` reran the previous selection. |
| `only` | Named with `smates run --only`. |
| `caller` | Added with `--add`. |
| `unaffected` | Not selected. |

### Via { #via }

The values of `via`:

| `via` | The changed dependency is |
|---|---|
| `test` | the test's own file |
| `input` | a suite or top-level input, or a rule in `smates.yml` |
| `service` | covered by a `depends_on` or `serves` declaration (`service` says which) |
| `import` | in the static import graph |
| `traced` | recorded by tracing a previous run (a file read, a directory listing, a command) |
| `reference` | an orphan file the test names |
| `orphan` | an orphan, under the orphan policy |

### Orphan handling { #orphan-handling }

The values of `orphan_handling` (and of `handling` in `orphan_candidates`):

| Value | Meaning |
|---|---|
| `reference` | The tests that name the file were selected. |
| `agent` | The agent decided. |
| `no-impact` | The agent judged it irrelevant to every test. |
| `policy` | The orphan policy ran the suites it may affect in full. |
| `ignored` | `orphans: ignore`. |
| `skipped` | The caller opted out of the orphan policy (`--orphans=skip`). |
| `untested` | Source code no test reaches (`untested: skip`). |

### Orphan candidates { #orphan-candidates }

Each entry of `orphan_candidates`:

| Field | Type | Meaning |
|---|---|---|
| `path` | string | The orphan file. |
| `status` | string | `added`, `modified` or `deleted`. |
| `referenced_by` | array | The tests that name the file: `suite`, `test`, `file` (the file that names it), `selected`. |
| `suites` | array of strings | The suites the orphan policy runs in full for it. |
| `handling` | string | How this run handled it ([values](#orphan-handling)); `""` when not handled. |

### Agent decision { #agent-decision }

The `agent` object:

| Field | Type | Meaning |
|---|---|---|
| `run_all` | boolean | The agent asked for every test of every suite. |
| `add` | array | The tests (or whole suites, when `Test` is `""`) it added. Unlike every other key, the keys of an entry are capitalized: `Suite`, `Test`, `Reason`, `Detail`, and `Files` (optional: the orphans that caused it). |
| `no_impact` | array of strings | Orphans it judged irrelevant to every test. |
| `summary` | string | A short explanation. |
| `usage` | object | `input_tokens`, `output_tokens`, `cache_read_tokens`, `cache_write_tokens`, `turns`, and `cost_usd` when the backend reports a cost. |
| `model` | string | The model. |
| `dropped` | array of strings, optional | Tests or suites it named that do not exist. |

## smates run --report { #smates-run-report }

`smates run --report <file>` writes this report to `file` (`-`: stdout, with all other
output on stderr). It has every field of [`smates select`](#smates-select), in which
`priority` is set on every selected unit (also those that end up in `not_run`), plus these:

| Field | Type | Meaning |
|---|---|---|
| `results` | object | Suite → unit → outcome, for the units that ran: `pass`, `fail`, `skip` (the test skipped itself) or `unknown` (no result could be determined, or runner arguments after `--` may have filtered it). |
| `exit_code` | integer | The runners' worst exit code; `4` when the run is an environment problem without failing tests. See [Exit codes](exit-codes.md#in-reports-and-ci-outputs). |
| `shadow` | boolean | The run used `--shadow`. |
| `escapes` | array of strings | With `--shadow`: failing tests the selection would have skipped (`suite: unit`). |
| `possible_escapes` | array of strings | With `--shadow`: unselected tests marked failed only because their run failed (the runner reports no per-file status). |
| `flaky` | array | Flips seen in this run: see below. |
| `failure_hints` | array | One hint per failing test: see [Failure hints](#failure-hints). |
| `fail_fast` | boolean | The run used `--fail-fast`. |
| `not_run` | object | Suite → selected units that did not run (`--fail-fast`, `--max-time`, an interrupt, unmet `requires`, a runner that could not start). They stay selected. |
| `interrupted` | boolean | The run was cancelled (Ctrl-C, SIGTERM or SIGHUP). What did not finish is in `not_run`. |
| `max_time_reached` | boolean | `--max-time` left out the units in `not_run` (except those of `unmet_requires`). See [Time limit](cli.md#time-limit). |
| `unmet_requires` | object | Suite → the [`requires`](configuration.md#requires) checks that failed. Their units are in `not_run`. |

Each entry of `flaky`:

| Field | Type | Meaning |
|---|---|---|
| `suite`, `unit` | string | The test unit. |
| `flip` | string | `pass->fail` or `fail->pass`. |
| `count` | integer | Flips recorded for the unit. |
| `retried` | boolean, optional | A known flaky unit failed, passed when run again ([`flaky.retry`](configuration.md#flaky)), and was not counted as a failure. |

### Failure hints { #failure-hints }

Each entry of `failure_hints` (see [Failure hints](../guides/failure-hints.md)):

| Field | Type | Meaning |
|---|---|---|
| `suite`, `test` | string | The failing test unit. |
| `inferred` | boolean, optional | The failure was derived from the exit code of a run of several units: it may belong to another test. |
| `verdict` | string | `changed-inputs` (some inputs changed: they are the candidates), `unattributed-changes` (none of its inputs changed, but changes no test explains did), `no-changed-inputs`, or `no-record` (nothing to compare with). |
| `basis` | string | What "changed" is measured against: `last-run`, `diff` (the changes of this run's selection) or `none`. |
| `since` | string, optional | The commit of the recorded run compared with. |
| `last_status` | string, optional | The outcome of that run. |
| `inputs` | integer | Number of known inputs of the test. |
| `changed` | integer | How many of them changed. |
| `candidates` | array | The likely causes, most likely first: `path`, `kind` (`source`, `test`, `data`, `env`, `config`, `listing`), `link` (how the test reaches it: `test file`, `direct import`, `static import`, `suite input`, `traced`, `unattributed`, `server code`), `change` (`modified`, `added`, `deleted`). |
| `more` | integer, optional | Candidates left out. |
| `output` | string, optional | The failure output the runner reported for the test. |
| `explanation` | object, optional | The agent's reading: `file`, `line`, `reason`, `confidence` (`low`, `medium`, `high`), `model`, `cached`. See [Agent explanations](../guides/failure-hints.md#agent-explanations). |

## smates stats { #smates-stats }

`smates stats --format json` sums up the runs recorded in the state (see
[Stats](../guides/trust.md#stats)). Each shard of a sharded run is a record of its own.

| Field | Type | Meaning |
|---|---|---|
| `state` | string | The state file. |
| `since` | time, optional | The `--since` bound. |
| `branch` | string, optional | The `--branch` filter. |
| `from`, `to` | time, optional | The first and the last run in the period. |
| `runs` | integer | Run records in the period. |
| `shard_jobs` | integer | Records of sharded runs. |
| `by_mode` | object | Records by mode: see below. |
| `by_source` | object | Records by source: `ci`, `local`, `watch`. |
| `failed_runs` | integer | Records with an exit code other than 0. |
| `selected_units` | integer | Test units selected, over the runs that were not in shadow mode. |
| `total_units` | integer | Test units in total, over the same runs. |
| `skip_rate` | number | `(total_units - selected_units) / total_units`, from 0 to 1. |
| `saved_ms` | integer | The recorded duration of the skipped units. |
| `skipped_untimed_units` | integer | Skipped units with no recorded duration (not in `saved_ms`). |
| `elapsed_ms` | integer | Time the runs that were not in shadow mode took. |
| `saved_per_30d_ms` | integer, optional | `saved_ms` extrapolated to 30 days (absent when the period is shorter than a day). |
| `shadow` | object | Shadow runs: see below. |
| `orphans` | object | `files` (orphans counted over the runs), `runs` (runs with orphans), `by_handling` (handling → count, [values](#orphan-handling)). |
| `agent` | object | Agent calls: `calls`, `cached`, `turns`, `input_tokens`, `output_tokens`, `cache_read_tokens`, `cache_write_tokens`, `cost_usd` (the sum of the costs reported), `cost_reported_calls` (uncached calls whose backend reported a cost). |
| `flaky` | object | `flips` (flips detected by the runs in the period), `units` (units the state marks flaky now). |

The keys of `by_mode`:

| Key | Runs |
|---|---|
| `state` | selected against the recorded state |
| `diff` | selected by static analysis against a base revision (no recorded state) |
| `all` | ran every test (`--all`, a full-run build on CI, or nothing recorded and no base revision) |
| `shadow` | `--shadow` runs, counted here instead of under their own mode |

The `shadow` object:

| Field | Type | Meaning |
|---|---|---|
| `runs` | integer | Shadow runs (a sharded shadow run counts once). |
| `runs_with_escapes` | integer | Shadow runs in which the selection would have skipped a failing test. |
| `failed_units` | integer | Failing units of shadow runs. |
| `escapes` | integer | Of those, the units the selection would have skipped. |
| `possible_escapes` | integer | Units marked failed only because their run failed. |
| `miss_rate` | number | `escapes / failed_units`, from 0 to 1. |

## smates flaky { #smates-flaky }

`smates flaky --format json` prints `{"flaky": [...]}`, one entry per unit the state
marks flaky (an empty array when none):

| Field | Type | Meaning |
|---|---|---|
| `suite`, `unit` | string | The test unit. |
| `count` | integer | Flips recorded. |
| `first`, `last` | time | The first and the most recent flip. |
| `commit` | string, optional | The commit of the most recent flip. |
| `flip` | string, optional | The most recent flip (`pass->fail`, `fail->pass`). |
| `status` | string | The unit's last recorded outcome. |
| `recent` | array of strings | Recent outcomes, oldest first. A `?` suffix marks an outcome inferred from the exit code of a run of several units. |
| `env` | array of strings | Environment variables the unit read. |

## smates init { #smates-init }

`smates init --format json` reports what init did and what it could not decide.

| Field | Type | Meaning |
|---|---|---|
| `config` | string | The config file. |
| `written` | boolean | `false` with `--stdout`, or when the config already existed. |
| `config_yaml` | string, optional | With `--stdout`: the config it would write. |
| `files_written` | array of strings | `smates.yml`, `.gitignore` (when `.smates/` was added), the workflow, the `--patch` file, and `AGENTS.md` or `CLAUDE.md` (when `--agents-md` added the section). |
| `suites` | array | The suites: `name`, `runner`, `root`, `exclude`. |
| `workflow` | object, optional | What `--github` did: see below. Absent without `--github`, and with `--stdout`. |
| `agents_md` | object, optional | The section for coding agents: see below. Absent when the repository has neither `AGENTS.md` nor `CLAUDE.md` and `--agents-md` was not given, and with `--stdout`. |
| `needs_review` | array | Items that need a person's (or an agent's) decision: see below. |
| `next_steps` | array of strings | Commands to run next. |

The `workflow` object:

| Field | Type | Meaning |
|---|---|---|
| `file` | string | The workflow file. |
| `written` | boolean | Whether it was written. |
| `skipped` | string, optional | Why it was not written. |
| `patch` | string, optional | A patch that replaces the test steps of existing workflows with the smates action. |
| `patch_file` | string, optional | With `--patch`: the file the patch was written to. |

The `agents_md` object:

| Field | Type | Meaning |
|---|---|---|
| `file` | string | `AGENTS.md` or `CLAUDE.md`. |
| `status` | string | `added` (`--agents-md` wrote it), `present` (the file has the section) or `suggested` (not written: `snippet` is the section to add). |
| `snippet` | string, optional | The section. |

Each entry of `needs_review`:

| Field | Type | Meaning |
|---|---|---|
| `id` | string | Stable for the same finding: `<kind>-<subject>` in kebab-case, or `<kind>` alone when there is no subject (`no-suites`, `orphans-policy`). |
| `kind` | string | See below. |
| `reason` | string | What was found. |
| `evidence` | array of strings | Where it was found. |
| `action` | object | The recommended action: `description`, and whichever apply of `command` (to run), `config` (`key`, a dotted path such as `suites.web.depends_on`, and `value`, any JSON value: a string, or an array for `depends_on`), `snippet` and `snippet_file` (the file to paste the snippet into). |

The values of `kind`:

| `kind` | Meaning |
|---|---|
| `no-suites` | No test suite was detected. |
| `makefile-command` | A Makefile target runs a suite's runner with flags or variables smates' own command does not pass. |
| `server-dependency` | A suite looks like end-to-end tests of a server in another process, with no `depends_on` or `serves`. |
| `service` | The tests appear to need a service (from a compose file, or code that connects to it on localhost). |
| `local-server` | A file a suite runs calls a `localhost` URL on a port from 1024, without starting or intercepting that server itself: tests using it need that server running. |
| `env` | The code reads an environment variable that an `.env.example` (`.env.sample`, `.env.template`, `.env.dist`) lists and no suite's `env` sets. |
| `workflow-test-step` | A workflow step runs the tests without smates. |
| `orphans-policy` | `orphans: agent`, but no agent is available. |
| `go-granularity` | One package holds most of a Go suite's test functions, so selecting whole packages saves little: `granularity: func` is suggested. |

## smates why, deps and list { #smates-why-deps-list }

`smates why <file | $VAR> --format json`:

| Field | Type | Meaning |
|---|---|---|
| `target` | string | The file (repository-relative) or `$VAR` asked about. |
| `tests` | array | The test units that depend on it: `suite`, `test`, `kind` (`content`, `declarations`, `listing`, `existence` or `tree`: what of the target they depend on), `how` (an array of strings, as the text output shows them: `static/inputs`, `recorded`, a `depends_on` or `serves` declaration, ...). |

`smates deps <test unit> --format json`:

| Field | Type | Meaning |
|---|---|---|
| `suite`, `test` | string | The test unit. |
| `deps` | array | Its dependencies: `dep` (a repository-relative file, or a key such as `$VAR`) and `sources` (`static` and/or `recorded`; empty for a dependency of neither kind). |

`smates list --format json`:

| Field | Type | Meaning |
|---|---|---|
| `config` | string | The config file the suites come from (`""` when they were detected). |
| `suites` | array | See below. |

Each entry of `suites`:

| Field | Type | Meaning |
|---|---|---|
| `name`, `runner`, `root`, `command` | string | The suite. |
| `unit_count` | integer | Number of test units. |
| `units` | array of strings | The test units. |
| `tests_source` | string | Where the test patterns come from: `smates.yml tests`, `runner defaults`, or the runner configuration they were read from (`jest.config.js testMatch`). |
| `test_patterns` | array of strings | The patterns (globs relative to the root, `!` for exclusions, `regex ...` for jest regexes). |
| `discovery_warning` | string, optional | Why the runner configuration could not be read (the default patterns are used then). |

## smates doctor { #smates-doctor }

`smates doctor --format json`:

| Field | Type | Meaning |
|---|---|---|
| `version` | string | The smates version. |
| `ok` | boolean | `false` when a check failed (the exit code is then 1). |
| `checks` | array | See below. |

Each check:

| Field | Type | Meaning |
|---|---|---|
| `id` | string | Stable, kebab-case: see the [check list](cli.md#smates-doctor). |
| `status` | string | `ok`, `warn` or `fail`. |
| `subject` | string, optional | For checks repeated per suite, service, variable or workflow step: what it is about. |
| `message` | string | What was found. |
| `fix` | object, optional | How to resolve it: `command`, `config` (`key`, a dotted path, and `value`, any JSON value) and/or `doc` (a URL). |

## smates state show { #smates-state-show }

`smates state show --format json`:

| Field | Type | Meaning |
|---|---|---|
| `path` | string | The state file. |
| `commit` | string | The commit of the recorded snapshot. |
| `created` | time | When the state was created. |
| `tool` | string | The smates version that wrote it. |
| `snapshot_files` | integer | Files in the snapshot. |
| `suites` | object | Suite → `tests` (number of recorded units), `status` (outcome → count), `avg_deps` (dependencies per unit, on average). |
| `agent_cache_entries` | integer | Cached agent answers. |
| `shared` | boolean | `true` when `state_shared: true` is set. |
| `snapshot_path` | string, optional | With a shared state: this worktree's snapshot file. |
| `seeded_from` | string, optional | Where the state was seeded from. |
| `seeded_at` | time, optional | When. |

## smates orphans { #smates-orphans }

`smates orphans --format json`:

| Field | Type | Meaning |
|---|---|---|
| `commits` | integer | Commits replayed. |
| `since` | string, optional | The `--since` revision. |
| `recorded_state` | boolean | `false` when no run was recorded: dependencies then come from static analysis, `inputs` and `rules` only, and the report is pessimistic. |
| `warnings` | array of strings | Warnings. |
| `orphan_policy` | string | `orphans:` in `smates.yml`. |
| `commits_with_orphans` | integer | Commits with a changed file no test explains. |
| `commits_with_orphans_after_ignore` | integer | Of those, the commits that would still have one with the suggested `ignore` globs. |
| `commits_with_orphans_after_inputs` | integer | Of those, the commits that would still have one if the `inputs`-kind files were also declared in `inputs` or `rules`. |
| `files` | array | The orphans, most frequent first: `path`, `commits`, `kind` (see below), `why` (kind `inputs`: what it looks like), `glob` (kind `ignore`: the suggested glob). |
| `ignore` | array | Suggested `ignore` globs: `glob`, `files`, `commits`. |
| `inputs` | array | The files of kind `inputs`, as in `files`. |
| `history` | array | The commits with orphans or with files a test names: `commit`, `subject`, `orphans`, `referenced` (orphans explained because a test names them). |
| `existing_ignore` | array of strings | `ignore:` in `smates.yml` today. |
| `applied` | object, optional | With `--apply`: the edit, as in [`smates config`](#smates-config). |

The values of `kind`:

| `kind` | Meaning |
|---|---|
| `ignore` | Nothing reads it: an `ignore` glob is suggested. |
| `inputs` | It may change how tests run: declare it in `inputs` or `rules`, not `ignore`. |
| `reference` | A test names it, which explains it without `ignore`. |
| `untested` | Source code no test reaches (`untested: skip`). |

## smates config { #smates-config }

`smates config add-ignore|add-input|set ... --format json` (and `applied` in
`smates orphans --apply --format json`):

| Field | Type | Meaning |
|---|---|---|
| `file` | string | The config file. |
| `key` | string | The key edited (`ignore`, `inputs`, or the key of `set`). |
| `added` | array of strings, optional | `add-ignore`, `add-input`: the globs added. |
| `present` | array of strings, optional | `add-ignore`, `add-input`: the globs that were already there. |
| `value` | string, optional | `set`: the new value. |
| `previous` | string, optional | `set`: the value before. |
| `changed` | boolean | Whether the file changes. |
| `written` | boolean | Whether it was written (`false` with `--dry-run`). |
| `dry_run` | boolean | `--dry-run` was given. |
| `diff` | string, optional | The change as a unified diff. |

## See also

- [Commands](cli.md)
- [Exit codes](exit-codes.md)
- [How smates decides](../getting-started/how-it-works.md)

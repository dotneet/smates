# smates on GitHub Actions

The `dotneet/smates` action installs smates, restores its state, runs the affected
tests and saves the updated state. One step replaces your test command:

```yaml
on:
  pull_request:
  push:
    branches: [main]

jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v5
      - uses: actions/setup-node@v4   # plus whatever toolchains your tests need
        with:
          node-version: 22
      - run: npm ci
      - uses: dotneet/smates@vX.Y.Z
        with:
          anthropic-api-key: ${{ secrets.ANTHROPIC_API_KEY }}   # optional
```

Replace `vX.Y.Z` with a release (`v1.2.3`), or use `@v1` to follow the newest v1.x
release (see [Pin the version](#pin-the-version)).

A complete, commented workflow is in
[`.github/workflows/smates.yml`](https://github.com/dotneet/smates/blob/main/.github/workflows/smates.yml).
`smates init --github` generates one for your project; when an existing workflow already
runs the tests, it prints (or, with `--patch FILE`, writes) a patch that replaces those
steps with the action instead of adding a second workflow (see
[Switch existing workflows](#switch-existing-workflows)).

## What the action does

1. **Install.** Downloads `smates_<os>_<arch>` from the release, verifies it against
   the release's `checksums.txt` and its build provenance attestation (see
   [`verify`](#verification)) and puts it on `PATH`. The archive is cached per version,
   OS and architecture, and verified again on every cache hit (which still needs network
   access for `checksums.txt`). With `version: source`,
   or when the download fails and Go is on `PATH`, it is built from the action's own
   checkout instead. A failed verification always fails the job; it never falls back.
2. **Restore state** (`.smates/`) from the Actions cache: the newest state of this
   branch, else (for a pull request into another branch than the default) of its base
   branch, else of the default branch, else none (cold start; see
   [Seed the state](#seed-the-state)).
3. **Run** `smates <command> <args>` in `working-directory`. The step's exit code is
   the tests' exit code.
4. **Comment** (with `comment: true`) the selection summary on the pull request.
5. **Save state**, also when tests failed (failed tests are then re-selected next
   time), and only if the state file actually changed.

Every run also writes the summary to the job page (`$GITHUB_STEP_SUMMARY`).

On the default branch `smates run` runs every test and writes a complete state; every
new pull request starts from it. On other branches it selects. Override with
`args: --all` or `args: --select`.

### Why branch fallback is safe

A state entry says "test T passed when its inputs had these content hashes". Reusing
an entry from another branch can only skip T if every input of T is byte-identical to
what that run saw. So any state is safe to start from; branch scoping just maximizes
hits.

What matters is who may *write* state that others read. GitHub scopes caches so that a
pull request can read the default branch's cache, but nothing outside that pull request
can read what it saves. Pull requests from forks cannot affect the default branch's
state.

## Inputs

| Input | Default | Description |
|---|---|---|
| `version` | action ref | Release to install (`v1.2.3`), `latest`, or `source`. By default it follows the ref in `uses:`: a release tag (or the commit of one) installs that release, a moving tag such as `v1` or `v1.2` the newest release of that line, any other ref `latest`. |
| `verify` | `auto` | How the download is verified: `auto`, `attestation` or `checksum` ([below](#verification)). |
| `command` | `run` | Subcommand. `select` defaults to `--format github`. |
| `args` | | Extra arguments, shell-parsed: `--shard 1/4 -- --reporter=dot`. Do not interpolate untrusted values (PR titles, branch names). |
| `working-directory` | `.` | Where smates runs. |
| `anthropic-api-key` | | Key for the orphan-file agent, passed only to the smates step. Falls back to `ANTHROPIC_API_KEY` in the job env. |
| `openai-api-key` | | Same, for OpenAI (`OPENAI_API_KEY`). |
| `openrouter-api-key` | | Same, for OpenRouter (`OPENROUTER_API_KEY`). With several keys, `agent.provider` in `smates.yml` picks one (default: Anthropic, then OpenAI, then OpenRouter). |
| `cache` | `true` | Restore/save state with the Actions cache. `false` if you store it yourself. |
| `cache-key` | | Extra cache key component, for jobs that run different tests on the same OS (matrix entries, a fixed number of shards). Jobs a plan job selects for (sized shards, suites gated per job) share one key with it instead, see [Size the shards to the selection](#size-the-shards-to-the-selection). |
| `state-path` | `.smates` | State directory, relative to `working-directory`. Must match `state` in `smates.yml`. |
| `comment` | `false` | `true` posts the summary as a pull request comment, updated in place ([below](#pull-request-comment)). |
| `comment-key` | | Separates the comments of several smates jobs in one pull request. |
| `github-token` | `github.token` | Token used for the comment and to look up attestations. |

Cache key: `smates-state-<runner.os>-[<cache-key>-]<branch>-<run_id>-<run_attempt>`,
restored by prefix `...-<branch>-`, then `...-<base branch>-` (pull requests into another
branch than the default), then `...-<default branch>-`.

## Outputs

| Output | Description |
|---|---|
| `selected` | Number of selected test units. |
| `total` | Number of known test units. |
| `run-all` | `true` when everything was selected. |
| `skip` | `true` when nothing was selected. |
| `tests` | JSON object: each suite's selected test unit IDs. |
| `suites` | JSON object, one entry per suite: `{"selected": 2, "total": 40, "all": false, "skip": false}`. `skip` is `true` only when the suite selected nothing ([gate on it](#gate-jobs-per-suite)). |
| `shards` | With `command: select` and `--max-shards N`: how many shards the selection needs, `0` when nothing was selected, else `1` to `N` ([below](#size-the-shards-to-the-selection)). |
| `shard-matrix` | With `--max-shards`: a `strategy.matrix`, `{"shard": ["1/3", "2/3", "3/3"]}`, whose values go to `--shard` as they are. |
| `exit-code` | Exit code of the smates command. |
| `flaky` | Number of test units `run` proved flaky (outcome flipped with unchanged inputs). |
| `flaky-tests` | JSON array of those units (`"suite: unit"`); see `smates flaky`. |
| `version` | Installed smates version (`source` for source builds). |
| `verified` | How it was verified: `attestation`, `checksum`, or `none` (source build). |
| `comment-url` | URL of the pull request comment, when one was posted or updated. |

## Patterns

### Pin the version

`uses: dotneet/smates@v1.2.3` installs exactly v1.2.3; `@v1` installs the newest v1.x
release. Let Dependabot (`package-ecosystem: github-actions`) bump a pinned version.

To pin by commit, as many security policies require, use the commit of a release tag;
Dependabot and Renovate keep the comment in sync:

```yaml
      - uses: dotneet/smates@0123456789abcdef0123456789abcdef01234567 # v1.2.3
        with:
          verify: attestation   # optional: refuse anything without valid provenance
```

The action finds the release tag that points at exactly that commit (from its own
checkout's tags, else `git ls-remote`, no API token) and installs that release's
binary; with an attestation, it must name that commit. A commit that is not a release
(or carries several release tags) is built from source, with a notice, and needs Go
(`actions/setup-go`) on the runner.

### Skip expensive setup when nothing is affected

Run `select` first in the same job:

```yaml
      - id: select
        uses: dotneet/smates@vX.Y.Z
        with:
          command: select
      - if: steps.select.outputs.skip != 'true'
        run: docker compose up -d && npx playwright install --with-deps
      - if: steps.select.outputs.skip != 'true'
        uses: dotneet/smates@vX.Y.Z
```

### Gate a whole job on the selection

Run the selection in a cheap job of its own:

```yaml
jobs:
  select:
    runs-on: ubuntu-latest
    outputs:
      skip: ${{ steps.select.outputs.skip }}
    steps:
      - uses: actions/checkout@v5
      - id: select
        uses: dotneet/smates@vX.Y.Z
        with:
          command: select
  test:
    needs: select
    # Skip only when the select job succeeded and said nothing is selected.
    if: ${{ !cancelled() && (needs.select.result != 'success' || needs.select.outputs.skip != 'true') }}
    runs-on: ubuntu-latest
    steps:
      # ... checkout, toolchains, then the smates step
```

Write the condition this way. Without `!cancelled()`, a `needs:` job is skipped when
the select job fails, and GitHub treats a skipped job as passing, so a required check
on it turns green with no test run. With it, a failed select job lets the test job run
as it would without the gate (it then selects on its own). Use this only if a skipped
job is acceptable for your required status checks.

### Gate jobs per suite { #gate-jobs-per-suite }

The `suites` output says, for each suite, whether it selected anything. Jobs that run
one suite each (`args: --suite <name>`) can then be skipped one by one:

```yaml
jobs:
  plan:
    runs-on: ubuntu-latest
    outputs:
      suites: ${{ steps.plan.outputs.suites }}
    steps:
      - uses: actions/checkout@v5
      - id: plan
        uses: dotneet/smates@vX.Y.Z
        with:
          command: select
          cache-key: suites        # the state the suite jobs record
  api:
    needs: plan
    if: >-
      ${{ !cancelled() && (needs.plan.result != 'success' ||
      fromJSON(needs.plan.outputs.suites || '{}')['api'].skip != true) }}
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v5
      # ... toolchains, services
      - uses: dotneet/smates@vX.Y.Z
        with:
          args: --suite api
          cache-key: suites
      - if: ${{ always() }}
        uses: actions/upload-artifact@v4
        with:
          name: smates-state-api
          path: .smates/state.json.gz
          include-hidden-files: true
          if-no-files-found: ignore
  # web: the same with --suite web and the artifact smates-state-web.
  # Combine what the suite jobs recorded, so the next plan sees all of it.
  merge-state:
    needs: [api, web]
    if: ${{ !cancelled() }}
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v5
      - uses: actions/download-artifact@v4
        with:
          pattern: smates-state-*
          path: ${{ runner.temp }}/suite-states
      - uses: dotneet/smates@vX.Y.Z
        with:
          command: state
          args: merge ${{ runner.temp }}/suite-states/*/state.json.gz
          cache-key: suites
```

Skip a job only when the output explicitly says its suite selected nothing:

- **Gate on `.skip != true`, never on `.selected > 0` or `.selected != 0`.** A suite
  missing from the output (a typo, a suite `--suite` left out, an empty output) reads as
  `null`, and a workflow expression treats `null` as `0`: a `selected` condition would
  skip the job, while `null != true` runs it.
- **Run when the plan job did not succeed** (`needs.plan.result != 'success'`, with
  `!cancelled()` so that the job is not skipped along with it). smates exits 2 to 4 on
  its own errors (configuration, environment), and then the outputs say nothing.
- `|| '{}'` keeps `fromJSON` from failing on an empty output.
- **One state for the plan and the suite jobs.** The plan selects from the state the
  suite jobs record, so they all share one `cache-key`, and `merge-state` combines
  what each suite job saved, as for [sized shards](#size-the-shards-to-the-selection).
  With a `cache-key` of its own (or none, while the suite jobs have theirs) the plan
  restores no recorded run: it selects by static analysis against the base revision,
  which does not see what the tests read at run time, nor re-select the tests that
  failed last time, and can skip a suite that its own job would run. If `merge-state`
  does not run, the next plan restores one suite job's state, in which the other
  suites' tests are unrecorded: they are then selected against the base, or run.

smates selects across suites: a change to a shared package selects the tests of every
suite that depends on it. This usually runs more than hand-written path filters, which
miss such dependents; keep path filters, if at all, only as a coarse outer gate that
never skips a suite smates selects.

**Required status checks.** A skipped job counts as passing. If one aggregate job is
the required check, have it accept a skipped job only when the plan said so:

```yaml
  tests-passed:
    needs: [plan, api, web]
    if: ${{ always() }}
    runs-on: ubuntu-latest
    steps:
      - env:
          PLAN: ${{ needs.plan.result }}
          SUITES: ${{ needs.plan.outputs.suites }}
          API: ${{ needs.api.result }}
          WEB: ${{ needs.web.result }}
        run: |
          ok() { # result suite
            [ "$1" = success ] && return 0
            [ "$1" = skipped ] && [ "$PLAN" = success ] &&
              [ "$(jq -r --arg s "$2" '.[$s].skip' <<<"$SUITES")" = true ]
          }
          ok "$API" api && ok "$WEB" web
```

### Shards and matrices

`--shard I/N` assigns every test to a shard by a stable hash
of its suite and ID, so for a given `N` a test always lands in the same shard and every
shard agrees, whatever state each one restored. Which shard owns a test depends on `N`,
so all shards of one run must use the same `N` (a matrix does). With a fixed shard count,
give each shard its own `cache-key`, so jobs do not overwrite each other's state (shards
sized by a plan job share one key instead, [below](#size-the-shards-to-the-selection)):

```yaml
    strategy:
      matrix:
        shard: [1, 2, 3, 4]
    steps:
      # ...
      - uses: dotneet/smates@vX.Y.Z
        with:
          args: --shard ${{ matrix.shard }}/4
          cache-key: shard-${{ matrix.shard }}
```

The same applies to a matrix over, say, Node versions (`cache-key: node-${{ matrix.node }}`).
The runner OS is already part of the key.

Sharded runs do not ask the agent ([why](../guides/agent.md#sharded-runs)): each shard
applies the orphan policy to its share. To use the agent with shards, run
`smates select` once before them (unsharded) and have every shard restore the state it
saved: they then reuse its cached decision.

### Size the shards to the selection { #size-the-shards-to-the-selection }

A fixed matrix starts every shard, each paying for its setup, even when the change
selects three tests. `smates select --max-shards N` sizes the shard count to the
selection: `0` when nothing is selected, else the recorded duration of the selected
tests divided by `--shard-time` (default `10m`), rounded up and kept within `1` to `N`.
A selected test without a recorded duration counts as the median of its suite; if a
suite with selected tests has no recorded duration at all, the count is `N`. On a push
build of the default branch (or a `full_run_branches` branch), where `smates run` runs
every test, the count is sized for all of them.

The count only decides how many jobs share the selection: every selected test still runs
in one of them. It is sized for the selection, so do not feed it to `--shadow` legs,
which run every test their shard owns: on a change that selects nothing it is `0` and no
shadow leg runs, which is where an escape would hide. Give sharded shadow runs a fixed
matrix (`--shadow --shard ${{ matrix.shard }}/4`). Ownership depends on the count, so the matrix values carry it
(`"1/3"`), and every shard of a run uses the same one.

```yaml
jobs:
  plan:
    runs-on: ubuntu-latest
    outputs:
      shards: ${{ steps.plan.outputs.shards }}
      shard-matrix: ${{ steps.plan.outputs.shard-matrix }}
    steps:
      - uses: actions/checkout@v5
      - id: plan
        uses: dotneet/smates@vX.Y.Z
        with:
          command: select
          args: --max-shards 8
          cache-key: sharded
          comment: true            # one comment for the whole selection
  test:
    needs: plan
    if: needs.plan.outputs.shards != '0'
    strategy:
      fail-fast: false
      matrix: ${{ fromJSON(needs.plan.outputs.shard-matrix) }}
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v5
      # ... toolchains
      - uses: dotneet/smates@vX.Y.Z
        with:
          args: --shard ${{ matrix.shard }}
          cache-key: sharded
      - if: ${{ always() }}
        uses: actions/upload-artifact@v4
        with:
          name: smates-state-${{ strategy.job-index }}
          path: .smates/state.json.gz
          include-hidden-files: true
          if-no-files-found: ignore
  # Mandatory: when the plan job did not succeed there is no matrix, and the sharded
  # job is skipped. Run a fixed matrix then, as without the plan.
  test-fallback:
    needs: plan
    if: ${{ !cancelled() && needs.plan.result != 'success' }}
    strategy:
      fail-fast: false
      matrix:
        shard: [1, 2, 3, 4, 5, 6, 7, 8]
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v5
      # ... toolchains
      - uses: dotneet/smates@vX.Y.Z
        with:
          args: --shard ${{ matrix.shard }}/8
          cache-key: sharded
  # Combine what the shards recorded, so the next plan sees every shard's outcomes.
  merge-state:
    needs: test
    if: ${{ !cancelled() && needs.test.result != 'skipped' }}
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v5
      - uses: actions/download-artifact@v4
        with:
          pattern: smates-state-*
          path: ${{ runner.temp }}/shard-states
      - uses: dotneet/smates@vX.Y.Z
        with:
          command: state
          args: merge ${{ runner.temp }}/shard-states/*/state.json.gz
          cache-key: sharded
```

- **The fallback is not optional.** If the plan job fails (smates exits 2 to 4 on its
  own errors, or the runner fails), `needs.plan.outputs.shards` is empty and the
  sharded job is skipped; without `test-fallback` nothing would test the change. A
  required check over these jobs must accept a skipped `test` only when the plan
  succeeded with `shards` `0`, and a skipped `test-fallback` only when the plan
  succeeded (see the aggregate job [above](#gate-jobs-per-suite)).
- **One cache key for all.** The plan and every shard share `cache-key: sharded`
  instead of one key per shard: with a varying count, shard 2 of one run owns other
  tests than shard 2 of the next, so a per-shard key would mostly restore entries of
  tests the shard no longer runs. Each shard saves what it ran; `merge-state` merges
  them (`smates state merge`, newer entry per test) and saves the result last, so the
  next run restores it. If `merge-state` does not run, the next run restores one
  shard's state: a test another shard ran is then selected again whenever its inputs
  differ from that state's older entry, which runs more, not less. A test that failed
  in another shard with inputs that have not changed since its last pass is only known
  to the merged state, so keep the merge job.
- **Comments.** Each shard's comment is keyed by `shard-I-of-N`, and a later run with
  another count leaves the old ones in place. Comment from the plan job instead, as
  above.
- **Overhead.** Recorded durations are test time only. For `go test` they leave out
  compiling packages, `TestMain` and fixtures, and every shard pays its own job setup.
  Lower `--shard-time` when that cost is large. The hash spreads tests evenly by count,
  not by duration, so shards can still differ in length.

### Monorepos

A single smates step covers every suite of the repository (`smates.yml` at the
repository root lists them). To split suites across jobs, give each job
`args: --suite <name>` and a distinct `cache-key` (or, when a plan job gates them, one
shared key, see [Gate jobs per suite](#gate-jobs-per-suite)). Each job then selects what
a single step would select for its suites:

- a change another suite's tests depend on is not an orphan in it;
- nor is a change under the root of a suite the job leaves out (the job running that
  suite handles it);
- `--suite <name> --all` on the default branch records the snapshot as a full run does,
  so pull requests only see what changed since.

To adopt suites one at a time in a single job instead, record the ready ones with
`--suite <name> --all` and pass `--base` to the job's selection: the suites that have
never been recorded are then decided against that revision by static analysis, as with
no state at all (without `--base`, CI runs their tests as new), until a full run records
them all; so are those the job has only run selectively since. A file only they have not
seen since a recorded suite saw it is no change for the recorded suites. The base must be
in the checkout: fetch it (`fetch-depth: 0`, or `git fetch origin <branch>`) and pass,
say, `--base origin/${{ github.base_ref }}`. A `--base` that names no commit stops the
selection: exit code 4 in a shallow clone that lacks it, 2 otherwise.

### Switch existing workflows { #switch-existing-workflows }

`smates init --github` reads `.github/workflows/` and, when a workflow already runs the
tests, proposes a patch (`--patch FILE` writes it) instead of adding a second workflow:

- Each test step becomes `uses: dotneet/smates@v1` with `args: --suite <name>` for the
  suites the step ran: those of its runner rooted at or below the directory it runs in
  (its `working-directory`, else the job's or the workflow's
  `defaults.run.working-directory`). The action's `working-directory` is not set:
  smates reads `smates.yml` and keeps its state at the repository root.
- When several steps are switched, each gets a `cache-key` named after its suites:
  per-module jobs keep separate state, and steps of one job running the same suites
  share it (so do jobs of the same name in different workflows, such as a push workflow
  seeding the pull request one). Two jobs running the same suites never share a key:
  they may run in different environments (a runtime version, other services). The
  smates steps the workflows already have count too: a single step switched next to an
  existing smates job gets a key of its own, never one that job uses. A step
  in a `strategy.matrix` job gets a key naming the leg (`web-${{ matrix.node }}`); a
  dimension whose values are objects is named by their properties
  (`${{ matrix.platform.os }}`), since GitHub turns an object into the string `Object`.
- The step's `id`, `if`, `timeout-minutes`, `continue-on-error` and `env` are kept.

It reports the steps it leaves alone, and why:

| The step | Why it is left alone |
|---|---|
| runs in a directory inside a suite's root (`pkg/app/internal`) | it runs only part of that suite: add a suite rooted there |
| names packages other than `./...` or `.`, or computes them (`$(go list ./... \| grep -v x)`, `"${packages[@]}"`) | smates selects among all of the suite's packages, which the job may not be set up for |
| passes `-run`, `-list`, `-bench`, `-c` or `-skip` (single tests, compile-only checks) | it is not the plain unit run |
| passes `-json` or `-coverprofile`, or pipes or redirects its output | another step may read the output |
| uses `${{ matrix.* }}` in its command or working directory, or its job's matrix (the whole matrix, its `include`, a dimension or a value) comes from an expression, or a matrix value is a list or a nested object | smates cannot tell which suite each leg runs, or give each leg its own state: give each leg `args: --suite <name>` and `cache-key: <name>` by hand |
| sets a variable on the test line (`DB_PORT=3307 go test ./...`, `env X=1 ...`), or an earlier line of its `run:` block changes the directory or environment (`cd`, `export`, `source`, `set -a`, `set --`; `set -e`, `set -u`, `set -x` and `set -o pipefail` do not) | the smates step would lose it: move it to the step's `env:` or `working-directory:`. An inline `GOFLAGS` is read as `go test` flags instead (carried as below, or left alone like the same flags on the command) |
| passes `-short`, names tests (`vitest run unit`, `jest unit`, a `pytest` path other than a suite's root) or packages (`-w`, `--workspace`, `--filter`) | it runs only some of the suite's tests |
| runs `turbo run test` or `node --test` | found, so that no second workflow runs the same tests, but never replaced |
| runs `go test` with `-p`, `-tags`, `-race` or `-timeout` that the suite's `command:` lacks | the action would run without them: add them to `smates.yml`, then rerun |
| is a plain run, but the suite's `command:` or the `GOFLAGS` of its `env:` passes `-short`, `-run` or `-skip`, or `-tags` the step does not pass, or that `GOFLAGS` is not a plain value (`${EXTRA}`) | the action would leave out tests the step runs (a tag can leave out files: `//go:build !fast`) |
| runs `go test`, but the suite's `command:` is not `go test` itself (`gotestsum -- -tags=fast`, `make test`, `sh -c "..."`) | smates cannot tell which flags it passes: check that it leaves out no test the step runs, then switch the step by hand |
| sets `GOFLAGS` on the test line while an `env:` of the workflow, job or step sets `GOFLAGS` too (or is an expression), unless both carry the same `-p`, `-tags`, `-race` and `-timeout` and the `env:` one leaves no test out | the line's value replaced the `env:` one for the step, but the smates step keeps the `env:` and runs with its `GOFLAGS`: set the flags in the step's `env:` |
| turns off on its command line (`-race=false`, `-tags=`) a flag the `GOFLAGS` of its `env:` sets | the suite's `command:` cannot turn it off, and the smates step keeps the `env:` |

Without an existing `smates.yml`, `smates init` writes those `go test` flags into the
suite's `command:` itself, so that `-p 1` for tests sharing a database applies to local
runs too. Flags from several steps or the Makefile combine toward the safer run (the
lowest `-p`, `-race`, the longest `-timeout`). `-tags` can also leave files out
(`//go:build !integration`), so only the runs of all of a suite's unit tests vote on
them: the steps the action can replace and the Makefile's `test` (or `test-unit`)
target, one without `-tags` voting for none. They are taken only when all votes agree;
a `test-integration` target or a step testing a few packages never puts its tags on the
whole suite. A `GOFLAGS` set in the `env:` of the workflow, job or step counts as flags
of the step's `go test` (its own flags win, and a `GOFLAGS` on the test line replaces
it); one smates cannot read (`${{ vars.GOFLAGS }}`, or an `env:` written as an
expression such as `${{ fromJSON(vars.CI_ENV) }}`) keeps the step from voting. The environment the steps set goes into
`smates.yml` only as a comment: CI values such as ports and database names may not hold
locally, and in CI the step's `env:` reaches the action anyway. Nothing is added to
`env_ignore`.

A workflow that runs only on pull requests also gets `push: branches: [<default>]`
(with the pull request's `paths:` filters) and a nightly `schedule:` in its `on:`; see
[Seed the state](#seed-the-state).

### Shadow mode

`args: --shadow` runs the full suite and reports what selection would have done, to
build confidence before trusting it. See
[Shadow mode](../guides/trust.md#shadow-mode) for a rollout plan.

It works with shards: `args: --shadow --shard ${{ matrix.shard }}/4` runs every test
the shard owns, selected or not, and reports as escapes the failing tests of its own
share that the selection (of the whole run) would have skipped. Together the shards run
every test once, and an escape is reported only by the shard that owns the test.
`smates stats` counts the shards as one shadow run and adds up their escapes.

### Seed the state { #seed-the-state }

A pull request starts from the state of its branch, else of its base branch, else of
the default branch. If no workflow runs smates on the default branch there is none: the
first run of every pull request starts cold, which without full history
(`fetch-depth: 0`) means running everything. `smates doctor` reports such workflows
(`workflow-no-seed`).

Run the same job on the default branch too. A nightly run with `--shadow` does two
things at once: it runs every test and records the state, and it reports the failing
tests a selection would have skipped (escapes), which `smates stats` counts:

```yaml
on:
  pull_request:
  push:
    branches: [main]       # optional: a full run per merge keeps the state current
  schedule:
    - cron: '23 3 * * *'   # nightly

jobs:
  test:
    steps:
      # ...
      - uses: dotneet/smates@vX.Y.Z
        with:
          args: --suite app ${{ github.event_name == 'schedule' && '--shadow' || '' }}
          cache-key: app
```

- Add the triggers to the job that already runs the tests rather than copying it into a
  separate seeding job: a copy has to repeat its services, setup and `env:`, and keep
  them in step. The `cache-key` must be the one the pull request job uses.
- On a push to the default branch `smates run` runs every test anyway. Drop `push:` if a
  full run per merge costs too much; the state is then at most a day old, which is still
  correct (a selection diffs against the state's own snapshot).
- Jobs gated on pull request data (`github.event.pull_request`, `github.head_ref`)
  skip or fail on push and schedule: check their `if:` conditions.
- A pull request into another branch than the default restores that branch's state,
  which exists only if that branch has runs of its own.
- `smates stats --since 30d` on the state restored from the default branch shows the
  shadow runs and escapes of the nightly runs. Pull request runs save their state in the
  pull request's own cache scope, so their records never reach it.

### Pull request comment { #pull-request-comment }

`comment: true` tells reviewers what the change affects and
why, in one comment that every push updates:

```yaml
jobs:
  test:
    permissions:
      contents: read
      pull-requests: write   # for the comment
    steps:
      # ...
      - uses: dotneet/smates@vX.Y.Z
        with:
          comment: true
```

The comment says:

- how many test units in which suites are affected, and how much recorded test time the
  skipped ones would have taken;
- for each changed file, which tests it selected and how they depend on it (static
  import, a read traced in their last run, a suite input, a directory listing, ...);
- tests selected for other reasons (failing last time, never recorded, picked by the
  agent);
- how each change no test depends on was handled (named by a test, the agent, the
  `orphans` policy);
- for `run`, the result and, in shadow mode, any escapes.

Long lists are cut with counts. Details:

- It works with `command: run` and `command: select`. The comment is found again by a
  hidden marker (`<!-- smates-summary -->`), so there is one per pull request.
- Several smates jobs in one pull request (matrix entries, suites split across jobs)
  overwrite each other's comment unless each sets its own `comment-key`.
- **Shards** each post their own comment (the key gets `shard-I-of-N`), covering the
  tests that shard owns: with a `cache-key` per shard, each shard's state only knows
  its own tests, so one shard cannot speak for the others. If one comment matters
  more than per-shard detail, add a separate unsharded `command: select` job with
  `comment: true` (it has no recorded state of its own and selects by static analysis
  against the base branch, so give it `fetch-depth: 0`).
- The comment never fails the job. It is skipped with a notice on pull requests from
  forks (their token is read-only; the summary is still on the job page), when the
  token lacks `pull-requests: write`, outside `pull_request` / `pull_request_target`
  events, and when the runner has no `gh` CLI (GitHub-hosted runners have it).
- Outside the action, `smates run --markdown file` and `smates select --format
  markdown` produce the same text.

### Your own storage

Set `cache: false` and manage `.smates/` yourself, e.g. with
`smates state pull` / `smates state push` (see [other CI systems](other-ci.md)).

## Verification { #verification }

Release archives and `checksums.txt` carry a [build provenance
attestation](https://docs.github.com/actions/security-for-github-actions/using-artifact-attestations)
signed by smates' release workflow. Unlike `checksums.txt`, which is downloaded from the
same release as the archive, it cannot be produced by someone who can only edit the
release: it is signed with a short-lived certificate for `dotneet/smates`'
`.github/workflows/release.yml`, run on a GitHub-hosted runner for the release's tag.

The action checks it with `gh attestation verify`, using `github-token`:

```bash
gh attestation verify <archive> --repo dotneet/smates \
  --signer-workflow dotneet/smates/.github/workflows/release.yml \
  --source-ref refs/tags/<version> --deny-self-hosted-runners
  # plus --source-digest <commit> when the action is pinned by commit
```

| `verify` | Behaviour |
|---|---|
| `auto` (default) | Checksum, plus the attestation when `gh` can check it and the archive has one. Releases published before attestations were introduced have none: checksum only, with a notice. When the release is the action's own (`uses: dotneet/smates@v1.2.3` or its commit, no `version` input), it is known to be attested, so a missing attestation fails the job. Without a usable `gh` (some self-hosted runners) it is checksum only, with a notice. |
| `attestation` | Checksum and attestation; anything else fails the job (no `gh`, no attestation, an older release). |
| `checksum` | `checksums.txt` only (the behaviour before attestations). |

An attestation that exists but does not verify always fails the job, as does a checksum
mismatch; the action never falls back to another method or to a source build after a
failed verification. `version: source` downloads nothing, so there is nothing to verify.

**Cached installs are verified too, and need network access.** Every run, including a
cache hit, downloads `checksums.txt` from the release (plus the attestation lookup when
`verify` is not `checksum`), so a cache hit does not work offline; the cost is one small
request per run.

??? note "Details: how the binary cache is verified"
    The cache entry (and, on self-hosted runners, the tool cache) holds only the release
    archive. It is restored into a staging directory that is never put on `PATH`; the
    action copies just the archive out of it and puts it through the same checks as a
    fresh download, then unpacks the `smates` binary into a new directory that holds
    nothing else and puts that on `PATH`. So the `PATH` entry holds only the verified
    binary, and a cache entry without the genuine archive fails verification instead of
    being used.

    This does not make the cache untrusted input: like any `actions/cache` restore, a
    crafted entry can write files outside its own path (the restore extracts with
    absolute paths), for example over the action's own scripts. Whoever can write the
    cache scope a job restores from remains trusted, as for the state (see
    [State is input](#security-model)).

## Notes

- **Checkout depth.** A shallow clone is enough once state exists. On a cold start
  (no state), smates diffs against the base branch when history is available
  (`fetch-depth: 0`); without it, it runs everything.
- **Forks.** Pull requests from forks get no secrets, so the agent is not used and the
  `orphans` policy applies; selection still works.
- **Windows.** The action and CLI run on `windows-latest` (steps run in Git Bash), but
  tracing has only been verified on Linux and macOS.
- **Runner arguments.** Arguments after `--` reach the test runner, but they may filter
  tests (`-t`, `-k`, `-run`), so a run with them never records a test as passed. Put
  permanent runner flags in the suite's `command` in `smates.yml` instead.
- **Storage.** A new cache entry is saved only when the state changed. Entries unused
  for 7 days are evicted by GitHub; the state is a small gzip file.
- **Debugging.** `args: --explain` prints why each test was selected. If every run on a
  pull request runs everything, check the "Restore smates state" step log for a cache
  hit, and that `state-path` matches `smates.yml`.

## Security model

- **The agent can only add tests.** Diffs and file contents are untrusted input to it
  (it is told so), and a manipulated answer can at worst fail to add tests for an
  orphan. Ignored files (such as `.env`) are never readable by its tools, including
  through symlinks. Untracked but not ignored files may be sent to the model as part of
  a diff.
- **Credentials stay with smates.** `ANTHROPIC_API_KEY` / `ANTHROPIC_AUTH_TOKEN`,
  `OPENAI_API_KEY` and `OPENROUTER_API_KEY` are removed from the environment of test
  processes, whichever provider the agent uses.
- **State is input.** The state decides what is skipped, so whoever can write the cache
  a job restores can make it skip tests. In a pull request that is the pull request
  itself, whose author can equally change the workflow or the tests; the guarantee
  for the default branch comes from its own runs, which run everything. Do not restore
  state written by untrusted jobs in jobs that guard protected branches.
- **`agent.command`** in `smates.yml` runs a command line chosen by the repository, like
  the test commands themselves do.

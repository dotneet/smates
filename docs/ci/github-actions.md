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
      - uses: actions/checkout@v4
      - uses: actions/setup-node@v4   # plus whatever toolchains your tests need
        with:
          node-version: 22
      - run: npm ci
      - uses: dotneet/smates@vX.Y.Z
        with:
          anthropic-api-key: ${{ secrets.ANTHROPIC_API_KEY }}   # optional
```

A complete, commented workflow is in
[`.github/workflows/smates.yml`](../../.github/workflows/smates.yml).

## What the action does

1. **Install.** Downloads `smates_<os>_<arch>` from the release, verifies it against
   the release's `checksums.txt` and puts it on `PATH`. The binary is cached per
   version, OS and architecture. With `version: source`, or when the download fails and
   Go is on `PATH`, it is built from the action's own checkout instead. A checksum
   mismatch always fails the job; it never falls back.
2. **Restore state** (`.smates/`) from the Actions cache: the newest state of this
   branch, else the newest state of the default branch, else none (cold start).
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
what that run saw, so any state is safe to start from; branch scoping just maximizes
hits. What matters is who may *write* state that others read: GitHub scopes caches so a
pull request can read the default branch's cache, but nothing outside that pull
request can read what it saves. Pull requests from forks cannot affect the default
branch's state.

## Inputs

| Input | Default | Description |
|---|---|---|
| `version` | action ref | Release to install (`v1.2.3`), `latest`, or `source`. Defaults to the ref in `uses:` when it is a release tag, otherwise `latest`. |
| `command` | `run` | Subcommand. `select` defaults to `--format github`. |
| `args` | | Extra arguments, shell-parsed: `--shard 1/4 -- --reporter=dot`. Do not interpolate untrusted values (PR titles, branch names). |
| `working-directory` | `.` | Where smates runs. |
| `anthropic-api-key` | | Key for the orphan-file agent, passed only to the smates step. Falls back to `ANTHROPIC_API_KEY` in the job env. |
| `openai-api-key` | | Same, for OpenAI (`OPENAI_API_KEY`). |
| `openrouter-api-key` | | Same, for OpenRouter (`OPENROUTER_API_KEY`). With several keys, `agent.provider` in `smates.yml` picks one (default: Anthropic, then OpenAI, then OpenRouter). |
| `cache` | `true` | Restore/save state with the Actions cache. `false` if you store it yourself. |
| `cache-key` | | Extra cache key component, for jobs that run different tests on the same OS (matrix entries, shards). |
| `state-path` | `.smates` | State directory, relative to `working-directory`. Must match `state` in `smates.yml`. |
| `comment` | `false` | `true` posts the summary as a pull request comment, updated in place ([below](#pull-request-comment)). |
| `comment-key` | | Separates the comments of several smates jobs in one pull request. |
| `github-token` | `github.token` | Token used for the comment. |

Cache key: `smates-state-<runner.os>-[<cache-key>-]<branch>-<run_id>-<run_attempt>`,
restored by prefix `...-<branch>-`, then `...-<default branch>-`.

## Outputs

| Output | Description |
|---|---|
| `selected` | Number of selected test units. |
| `total` | Number of known test units. |
| `run-all` | `true` when everything was selected. |
| `skip` | `true` when nothing was selected. |
| `exit-code` | Exit code of the smates command. |
| `flaky` | Number of test units `run` proved flaky (outcome flipped with unchanged inputs). |
| `flaky-tests` | JSON array of those units (`"suite: unit"`); see `smates flaky`. |
| `version` | Installed smates version (`source` for source builds). |
| `comment-url` | URL of the pull request comment, when one was posted or updated. |

## Patterns

**Pin the version.** `uses: dotneet/smates@v1.2.3` installs exactly v1.2.3. Let
Dependabot (`package-ecosystem: github-actions`) bump it.

**Skip expensive setup when nothing is affected.** Run `select` first in the same job:

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

**Gate a whole job** on a cheap selection job:

```yaml
jobs:
  select:
    runs-on: ubuntu-latest
    outputs:
      skip: ${{ steps.select.outputs.skip }}
    steps:
      - uses: actions/checkout@v4
      - id: select
        uses: dotneet/smates@vX.Y.Z
        with:
          command: select
  test:
    needs: select
    if: needs.select.outputs.skip != 'true'
    runs-on: ubuntu-latest
    steps:
      # ... checkout, toolchains, then the smates step
```

Use this only if a skipped job is acceptable for your required status checks (GitHub
treats a skipped job as passing).

**Shards and matrices.** `--shard I/N` assigns every test to a shard by a stable hash
of its suite and ID, so a test always lands in the same shard and every shard agrees,
whatever state each one restored. Give each shard its own `cache-key`, so jobs do not
overwrite each other's state:

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

**Monorepos.** A single smates step covers every suite of the repository (smates.yml
at the repository root lists them). If you split suites across jobs, use `args: --suite
<name>` and a distinct `cache-key` per job. Each job then selects what a single step would
select for its suites: a change another suite's tests depend on is not an orphan in it.

**Shadow mode.** `args: --shadow` runs the full suite and reports what selection would
have done, to build confidence before trusting it.

<a id="pull-request-comment"></a>
**Pull request comment.** `comment: true` tells reviewers what the change affects and
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

The comment says how many test units in which suites are affected, and how much
recorded test time the skipped ones would have taken; for each changed file, which
tests it selected and how they depend on it (static import, a read traced in their last
run, a suite input, a directory listing, ...); tests selected for other reasons
(failing last time, never recorded, picked by the agent); how each change no test
depends on was handled (named by a test, the agent, the `orphans` policy); and, for
`run`, the result and, in shadow mode, any escapes. Long lists are cut with counts.

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

**Your own storage.** `cache: false` and manage `.smates/` yourself, e.g. with
`smates state pull` / `smates state push` (see [other CI systems](other-ci.md)).

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

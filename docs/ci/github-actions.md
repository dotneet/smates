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
steps with the action instead of adding a second workflow.

## What the action does

1. **Install.** Downloads `smates_<os>_<arch>` from the release, verifies it against
   the release's `checksums.txt` and its build provenance attestation (see
   [`verify`](#verification)) and puts it on `PATH`. The archive is cached per version,
   OS and architecture, and verified again on every cache hit (which still needs network
   access for `checksums.txt`). With `version: source`,
   or when the download fails and Go is on `PATH`, it is built from the action's own
   checkout instead. A failed verification always fails the job; it never falls back.
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
| `cache-key` | | Extra cache key component, for jobs that run different tests on the same OS (matrix entries, shards). |
| `state-path` | `.smates` | State directory, relative to `working-directory`. Must match `state` in `smates.yml`. |
| `comment` | `false` | `true` posts the summary as a pull request comment, updated in place ([below](#pull-request-comment)). |
| `comment-key` | | Separates the comments of several smates jobs in one pull request. |
| `github-token` | `github.token` | Token used for the comment and to look up attestations. |

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
    if: needs.select.outputs.skip != 'true'
    runs-on: ubuntu-latest
    steps:
      # ... checkout, toolchains, then the smates step
```

Use this only if a skipped job is acceptable for your required status checks (GitHub
treats a skipped job as passing).

### Shards and matrices

`--shard I/N` assigns every test to a shard by a stable hash
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

Sharded runs do not ask the agent ([why](../guides/agent.md#sharded-runs)): each shard
applies the orphan policy to its share. To use the agent with shards, run
`smates select` once before them (unsharded) and have every shard restore the state it
saved: they then reuse its cached decision.

### Monorepos

A single smates step covers every suite of the repository (`smates.yml` at the
repository root lists them). To split suites across jobs, give each job
`args: --suite <name>` and a distinct `cache-key`. Each job then selects what a single
step would select for its suites:

- a change another suite's tests depend on is not an orphan in it;
- nor is a change under the root of a suite the job leaves out (the job running that
  suite handles it);
- `--suite <name> --all` on the default branch records the snapshot as a full run does,
  so pull requests only see what changed since.

To adopt suites one at a time in a single job instead, record the ready ones with
`--suite <name> --all` and pass `--base` to the job's selection: the suites that have
never been recorded are then decided against that revision by static analysis, as with
no state at all (without `--base`, CI runs their tests as new). A file only they have
not seen since a recorded suite saw it is no change for the recorded suites.

### Shadow mode

`args: --shadow` runs the full suite and reports what selection would have done, to
build confidence before trusting it. See
[Shadow mode](../guides/trust.md#shadow-mode) for a rollout plan.

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

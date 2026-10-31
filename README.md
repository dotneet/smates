# smates

**Run only the tests your change can affect** — for TypeScript/JavaScript (vitest, jest,
`node --test`, bun), Go and Python (pytest, uv), in CI and locally.

smates learns what every test *actually uses* by tracing real test runs (executed code,
files read, directories listed, child processes), adds a static import graph as a safety
net, and skips a test only when **nothing it depends on has changed since it last
passed**. Changes nobody is known to depend on (a Dockerfile, a CI script, a SQL file
applied by a service) are handed to a small LLM agent (Claude Haiku) that can only *add*
tests — or, without an agent, run the affected suites in full.

```
$ smates run
smates: selected 7 of 412 test unit(s) across 3 suite(s) (98% skipped)
smates: web: running 5 test unit(s) with vitest
...
smates: done in 6.2s: ran 7 of 412 test unit(s), exit code 0
```

- **One static binary** (~5 MB, no runtime dependencies). Installs in about a second.
- **No code changes, no plugins to install.** Tracing hooks are injected through
  environment variables (`NODE_OPTIONS`, `PYTHONPATH`, `GOFLAGS`) only while smates runs
  your existing test command.
- **Sees what static analysis cannot**: dynamic `import()`/`require()` with computed
  paths, `importlib`, fixtures and config files read at runtime, directories a test
  iterates over, scripts started as subprocesses, shared pytest fixtures.
- **Safe by construction**: content-hash fingerprints, failed tests always re-run, new
  tests always run, and every uncertainty resolves to running *more*.
- **Points at the cause of a failure**: a failing test comes with the changed files among
  its inputs, and optionally a one-line explanation from the agent
  ([failure hints](#failure-hints)).
- **Works anywhere**: GitHub Action included; any other CI uses the same CLI and a plain
  state file.

## How it decides

For every test unit `T` (a test file; a package for Go) smates stores the inputs it
used and their git blob hashes at the time it last ran:

```
D(T) = traced(T)        files executed or read while T ran (incl. its child processes)
     ∪ static(T)        import-graph closure computed now (new imports are seen at once)
     ∪ inputs           lockfiles, manifests, runner config, smates.yml, your globs
```

`T` is **skipped** only if it passed last time and every file in `D(T)` hashes the same
now. Otherwise it is **selected**, with a reason (`smates run --explain`):

```
+ src/cart.test.ts      [changed: src/pricing.ts]
+ tests/test_report.py  [changed: tests/data/catalog.json]
+ pkg/api               [failed: last run: fail]
+ src/new.test.ts       [new: no recorded run]
```

Hashes are compared by content, not with `git diff`, so selection needs no history
(`fetch-depth: 1` is fine), works on dirty working trees, and is never confused by
rebases or merge commits.

**Orphans.** A changed file that is in no test's `D(T)` and not ignored (docs, editor
config, …) is an *orphan*: deterministic analysis cannot explain it. smates then

1. selects tests whose sources or dependencies mention the file by name (fixtures,
   migrations, schemas referenced as strings);
2. asks the **agent** about the rest (when `ANTHROPIC_API_KEY` is set): it explores the
   repository with read-only tools and adds specific tests or requests a full run.
   It never removes tests, so a wrong or manipulated answer can only fail to *add*;
3. otherwise runs the suites the orphan can affect in full.

## Supported runners

| runner | unit | how dependencies are traced | per-file status |
|---|---|---|---|
| `vitest` | test file | V8 coverage per worker + fs hooks; globalSetup reads count for every test | injected reporter |
| `jest` | test file | V8 coverage + fs hooks per worker, workers recycled after each file (`isolate: true` for strictly one process per file) | injected reporter |
| `node` (`node --test`) | test file | V8 coverage per test process + fs hooks | process exit code |
| `bun` | test file | per-file `bun test --coverage` + module cache, preload hooks for `node:fs`, `Bun.file`, `Bun.Glob` (while recording) | process exit code |
| `pytest` | test file | `sys.monitoring` (3.12+) / `sys.setprofile`, audit hooks; per test, per fixture, per collected module, plus module-level reads | plugin |
| `go` | package | static import closure (exact for Go) + `-test.testlogfile` via `go test -exec`; Node/Python children of tests | binary exit code, build failures |
| `custom` | test file | any Node/Python processes your command starts | overall exit code |

Child processes are followed everywhere: Node and Python children trace themselves (even
when started through a shell), and shell scripts, `grep`, `cat` and other commands are
analyzed from their command line and script contents. A command smates cannot see into
(`make`, a compiled binary) makes the test depend on everything below the directory it
runs in.

Package managers: npm, pnpm, yarn, bun (local `node_modules/.bin` binaries are used
directly), uv / virtualenvs for Python. Monorepos with several suites, workspaces
(npm/pnpm/yarn/bun) and tsconfig `paths` are handled by the static analyzer.

## Quick start

```bash
# install (Linux/macOS; or download a release, or `go install github.com/dotneet/smates/cmd/smates@latest`)
curl -fsSL https://raw.githubusercontent.com/dotneet/smates/main/install.sh | sh

smates init --github # detect suites, write smates.yml (+ a GitHub workflow), gitignore .smates/
smates run --all     # run everything once and record what each test uses
# ... edit code ...
smates run           # run only what your changes can affect
smates why src/pricing.ts     # which tests depend on this file?
smates deps src/cart.test.ts  # what does this test depend on?
```

Without `smates.yml` smates auto-detects suites (`smates list`); `smates doctor` checks
the setup.

### GitHub Actions

```yaml
on:
  push: { branches: [main] }
  pull_request:

jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v5
      - uses: actions/setup-node@v4   # / setup-go / astral-sh/setup-uv, as usual
        with: { node-version: 22 }
      - run: npm ci
      - uses: dotneet/smates@v1
        with:
          anthropic-api-key: ${{ secrets.ANTHROPIC_API_KEY }}   # optional
```

On pushes to the default branch everything runs and the state is refreshed; on pull
requests only affected tests run. The state is kept in the Actions cache per branch,
falling back to the default branch's. Outputs (`selected`, `total`, `skip`, `run-all`)
let you skip downstream jobs, and a summary is written to the job page. See
[docs/ci/github-actions.md](docs/ci/github-actions.md) (sharding, matrices) and
[docs/ci/other-ci.md](docs/ci/other-ci.md) (GitLab, CircleCI, Buildkite, Jenkins).

### Other CI systems

The state is one file (`.smates/state.json.gz`). Keep it in your CI cache, or in the
repository itself under a git ref with `smates state push` / `smates state pull`
(no external storage needed). Default-branch detection reads the usual CI variables
(GitHub, GitLab, CircleCI, Buildkite, Jenkins); `--all` / `--select` override it.

## Configuration (`smates.yml`)

```yaml
version: 1
suites:
  - name: web
    runner: vitest
    root: packages/web
    # optional:
    tests: ["src/**/*.test.ts"]      # test file globs (relative to root)
    exclude: ["src/legacy/**"]
    command: "pnpm exec vitest run"   # selected files are appended (or put in {tests})
    inputs: ["/proto/**"]            # extra files all tests of the suite depend on ("/" = repo root)
    rules:                           # dependencies tracing cannot see
      - tests: ["e2e/**"]
        deps: ["server/**", "docker-compose.yml"]
    env: { TZ: UTC }
    isolate: true                    # jest/bun: one process per file while recording
    junit: reports/junit.xml          # per-file status from a JUnit report you produce
  - name: api
    runner: go
    root: services/api
  - name: tools
    runner: pytest
    root: tools

orphans: agent        # agent | all | ignore
ignore: ["design/**"] # added to the built-in docs/editor/lint defaults
inputs: [".github/workflows/test.yml"]
full_run_branches: [main, release]
flaky:
  retry: 0            # re-run a failing, already known flaky test up to N times (default 0: never hide a failure)
agent:
  model: claude-haiku-5-5
  max_turns: 12
  timeout: 120s
  instructions: "e2e/ tests talk to the server built from server/."
  explain_failures: true   # one-line probable cause for failing tests (built-in backend only)
  explain_max: 3           # failing tests explained per run
  # command: "fx ask --json --no-save"   # use any external agent CLI instead
```

## Commands

| command | |
|---|---|
| `smates run [-- args]` | select, run with tracing, update state. `--all`, `--select`, `--shard I/N`, `--shadow`, `--explain`, `--no-agent`, `--no-record`, `--report file.json`, `--dry-run` |
| `smates select` | selection only: `--format text\|files\|json\|github` |
| `smates init [--github]` / `list` / `doctor` | setup helpers (`--github` writes a workflow with the right toolchain setup) |
| `smates why <file>` / `deps <test>` | inspect the dependency map |
| `smates flaky` | tests proven flaky, with evidence: `--format text\|json`, `--clear [test...]` |
| `smates state show\|pull\|push\|merge\|clear` | inspect and move the state |

**Shadow mode** (`smates run --shadow`) runs every test but evaluates the selection and
reports any failing test it would have skipped. Use it for a while before trusting the
selection in a new repository, or permanently on a nightly job.

**Flaky tests** are detected by proof, not by heuristics. smates knows the content hash of
every input of every test, so when a test's outcome flips between pass and fail while none
of those inputs changed (a failing test that passes when re-run, a full run on the default
branch that fails a test nothing touched), the test is flaky — or it depends on something
smates cannot see (time, network, environment variables, files outside the repository).
`smates run` reports such tests in its log, in `--report` (`flaky`), in the job summary and
in the `flaky` / `flaky-tests` outputs, and remembers the evidence (number of flips, last
commit, recent outcomes) in the state; `smates flaky` lists it. A flaky failure still
fails the run and is still selected next time. With `flaky: {retry: 1}`, a failing test
that is *already known* to be flaky is re-run once, and if it then passes the run does not
fail (only when every failure of that suite run is such a test; the flip is reported).

## Failure hints

When a test fails, smates already knows every file it used and which of them changed, so
`smates run` lists, for each failing test, **the changed files among its inputs** — usually
a handful, often one:

```
smates: failure hints for 1 failing test unit(s):
  web: src/cart.test.ts — 2 of its 37 known inputs changed since its last recorded run (3f2a1c9, passed)
      src/price.ts  (source, direct import, modified)
      fixtures/prices.json  (data, traced, modified)
    → agent (high confidence): src/price.ts:12 — roundCents now truncates instead of rounding, so the total is off by one cent
```

- "Changed" is measured against the content recorded when that test last ran (or, for a
  test with no record, against the files this run's selection found changed). Candidates
  are ranked: code the test imports directly, other code it uses, the test file itself,
  data, configuration, directory listings; then by modification time.
- **None of its inputs changed** points at a flaky test or the environment (time, network,
  ordering, something tracing cannot see); if it did not pass last time either, the cause
  predates that run. Changed files no test is known to depend on are listed instead when
  there are some.
- Shown in the run log, the step summary (`--summary` / `$GITHUB_STEP_SUMMARY`) and the
  JSON report (`failure_hints`, with the runner's failure output when it reports one:
  vitest, jest, pytest, `go test`, JUnit). Runs with `--no-record` have no per-test
  results and no hints.
- **With the agent** (an API key, not `--no-agent`, `agent.explain_failures` not false),
  up to `agent.explain_max` failures get a one-line probable cause (file, line, reason,
  confidence). It is one tool-less request per failure with the failure output and the
  diffs of the top candidates (bounded in size; git-ignored files are never sent), made in
  the background while later suites run, with a 30 s cap; answers are cached by content in
  the state, and errors only print a note.

## The agent

- Called only for orphans (and, after a failing run, to explain a few failures), so most
  pull requests make no LLM call at all.
- Built-in client for the Anthropic Messages API (no SDK), default `claude-haiku-5-5`,
  low effort, prompt caching. Typical cost is well under $0.01 per call and a few seconds.
- Read-only tools confined to the repository (`list_files`, `grep`, `read_file`,
  `git_diff`, `tests_for_file`, `test_deps`, `search_tests`); ignored files such as
  `.env` are never readable.
- Bounded by turns, a wall-clock timeout and a token budget; answers are validated
  against the real test inventory and cached per change. Any failure falls back to
  running the affected suites in full.
- Fork pull requests get no secrets: smates simply falls back.

## Limitations

- Dependencies outside the repository (services, databases, network) are not traced;
  declare them with `rules`/`inputs`, or let orphans reach the agent.
- Code executed in a browser (vitest browser mode, Playwright) is covered by the static
  graph only.
- Granularity is the file (the package for Go): a change anywhere in a module re-runs
  every test that loaded it. For Python, data a module reads at import time or behind a
  cache belongs to that module.
- Jest occasionally runs two files back to back in one worker even when recycling; their
  dependencies are then merged (still correct). `isolate: true` makes it exact at the cost
  of one jest start per file while recording.
- Vitest 5's `fsModuleCache` and other runner caches are outside smates' view: a cache
  restored in CI can hide failures that a cold run would show, with or without smates.
- Arguments passed through to the runner (`smates run -- -t name`) may filter tests, so
  such runs never record a test as passed; put permanent flags in the suite `command`.
- Python 3.8–3.11 is traced with `sys.setprofile`, which slows tests noticeably while
  recording; Python 3.12+ uses `sys.monitoring` (low overhead).
- On case-insensitive file systems, an import whose case differs from the file on disk
  is recorded under the imported spelling and may not match the file's changes.
- Type-only changes are handled like any change (type checking is not a test run;
  keep `tsc` in CI).
- Windows: the CLI builds and runs there, but tracing has only been verified on Linux and
  macOS.

## License

Not chosen yet (add a LICENSE file before publishing).

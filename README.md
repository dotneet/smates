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
D(T) = traced(T)        files executed or read while T ran (incl. its child processes),
                        and environment variables its code read
     ∪ static(T)        import-graph closure computed now (new imports are seen at once)
     ∪ inputs           lockfiles, manifests, runner config, smates.yml, your globs
```

`T` is **skipped** only if it passed last time and every file in `D(T)` hashes the same
now. Otherwise it is **selected**, with a reason (`smates run --explain`):

```
+ src/cart.test.ts      [changed: src/pricing.ts]
+ tests/test_report.py  [changed: tests/data/catalog.json]
+ src/dates.test.ts     [env changed: TZ]
+ pkg/api               [failed: last run: fail]
+ src/new.test.ts       [new: no recorded run]
```

Hashes are compared by content, not with `git diff`, so selection needs no history
(`fetch-depth: 1` is fine), works on dirty working trees, and is never confused by
rebases or merge commits.

**Environment variables.** A variable a test reads (`process.env.X` in Node and Bun,
also through jest's and vitest's per-file copies and `import.meta.env`; `os.environ` /
`os.getenv` in Python; `os.Getenv` / `os.LookupEnv` in Go) is a dependency like a file:
`$X` in `smates deps`, compared by a hash of its value (never the value itself; unset and
empty differ). Only reads made with repository code on the stack count, not the runner's
own lookups; copying the whole environment (`{...process.env}`, `dict(os.environ)`,
starting a child process) is not a read of every variable, and a value the test set
itself is not an outer dependency. The value compared is the one the suite's tests see:
`env:` in `smates.yml` overrides the shell. Variables that differ between machines and
between CI and a laptop are ignored by default, so a state recorded on CI and pulled
locally does not select every test: `PATH`, `HOME`, `USER`, `TMPDIR`, `SHELL`, locale
(`LANG`, `LC_*`), terminal (`TERM`, `COLUMNS`, color switches), CI metadata (`CI`,
`GITHUB_*`, `RUNNER_*`, `GITLAB_*`, …), proxies, toolchain locations (`GOPATH`,
`VIRTUAL_ENV`, `npm_*`, …) and what smates sets itself (`SMATES_*`, `NODE_OPTIONS`,
`PYTHONPATH`, `GOFLAGS`, …); see `DefaultEnvIgnore` in
[internal/config/config.go](internal/config/config.go). Add your own with `env_ignore`,
track a default one with `env_track` (top level or per suite), or turn tracking off with
`env_ignore: ["*"]`.

**Orphans.** A changed file that is in no test's `D(T)` and not ignored (docs, editor
config, …) is an *orphan*: deterministic analysis cannot explain it (with `--suite`, the
tests of the other suites count too). smates then

1. selects tests whose sources or dependencies mention the file by name (fixtures,
   migrations, schemas referenced as strings);
2. asks the **agent** about the rest (when `ANTHROPIC_API_KEY`, `OPENAI_API_KEY` or
   `OPENROUTER_API_KEY` is set): it explores the
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
| `go` | package, or test function with `granularity: func` | static import closure (exact for Go) + `-test.testlogfile` via `go test -exec`; Node/Python children of tests. Per function: coverage per test process (below) | binary exit code, build failures; per function: its own process (recording) or go test's `--- FAIL` lines |
| `custom` | test file | any Node/Python processes your command starts | overall exit code |

**Go test functions** (`granularity: func` on a `go` suite). When one package holds most
of the tests and most changes reach it, selecting packages saves little. With
`granularity: func` a unit is a top-level test function (`internal/api#TestCreateUser`:
`TestXxx`, `FuzzXxx` with its seed corpus, `ExampleXxx`; subtests run with their parent).
While recording, smates builds each package's test binary once with coverage over the
repository packages it links (`go test -c -cover -coverpkg=...`, with the suite command's
build flags such as `-tags` and `-race`) and runs every function in its own process
(`-test.run '^TestX$'`, a coverage profile and a test log). A function then depends on:

- the Go files in which it executed code (a file counts when any of its blocks ran;
  package initialization runs in every process, so it counts for every function);
- the *declarations* of every other Go file it may reach (the file without its function
  bodies): a changed constant, variable initializer, type, signature or method selects
  every function that could see it, an edit inside a body only those that executed it;
- its own `_test.go` file, the test files declaring helpers it names, and test files with
  `TestMain`, `init` or package-level variables initialized by calls;
- as for a package: files and directories its test log recorded, environment variables
  it read, embedded and other non-Go files of the static closure, `go.mod`/`go.sum`,
  what its child processes used; Go packages it builds or runs as a subprocess count in
  full; and the listing of its package directory (a file added there selects every
  function of the package).

A changed test file selects the functions it declares; a new function is selected as new.
Selected functions run with `go test -run '^(TestA|TestB)$'` (or the whole package when
the pattern would be too long) when not recording. When the binary cannot be built with
coverage, or the suite command is not a plain `go test` (or passes `-run`, `-skip`,
`-bench`, `-fuzz`, `-c`), that package is recorded as a whole and says so: each of its
functions then depends on everything the package does. Recording costs one process per
function (plus one coverage build per package); functions of one package run one at a
time, as `go test` runs them, unless `record_jobs` allows more.

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
smates hook install  # optional: run the affected tests before every git push
```

Without `smates.yml` smates auto-detects suites (`smates list`); `smates doctor` checks
the setup.

### Git hook

`smates hook install` writes a `pre-push` hook (`--hook pre-commit` for a pre-commit
one) that runs `smates run --no-agent` and stops the push when a selected test fails.
Without the agent the hook makes no API calls (changes no test is known to depend on run
the affected suites in full); `--agent` keeps it, and `smates run` flags can follow `--`
(`smates hook install -- --explain`). The hook:

- honours `core.hooksPath` and is shared by all worktrees of the repository;
- never overwrites a hook it did not write: it stops and prints the line to add, or
  appends itself to a shell hook with `--append`. With husky or lefthook it prints the
  snippet for `.husky/pre-push` or `lefthook.yml` instead;
- pulls the shared state (`smates state pull`) when there is none locally, and merges in
  the remote one when the local state is older than a week or was recorded on a commit
  outside the current history (fetching at most once an hour);
- is skipped with `git push --no-verify` or `SMATES_SKIP_HOOK=1`, and lets the push
  through with a warning when `smates` is not on `PATH`.

smates hashes the working tree, not the commits being pushed: uncommitted (pre-commit:
unstaged) changes and untracked files are part of what is tested. The hook warns when
there are any, and when the pushed branch is not the one checked out; commit or stash
them first for an exact result. `smates hook uninstall` removes only what smates wrote;
`smates hook status` shows where the hook lives.

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
          anthropic-api-key: ${{ secrets.ANTHROPIC_API_KEY }}   # optional (or openai-api-key / openrouter-api-key)
```

On pushes to the default branch everything runs and the state is refreshed; on pull
requests only affected tests run. The state is kept in the Actions cache per branch,
falling back to the default branch's. Outputs (`selected`, `total`, `skip`, `run-all`)
let you skip downstream jobs, and a summary is written to the job page. With
`comment: true` (and `permissions: pull-requests: write`) the same summary becomes one
pull request comment, updated on every push: how many tests in which suites the change
affects, which changed file selected which test and how it depends on it, how changes
no test depends on were handled, and the recorded test time skipped. See
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
    granularity: func                # select Go tests per test function (default: package)
    record_jobs: 4                   # with func: functions of one package recorded at once (default 1)
  - name: tools
    runner: pytest
    root: tools

orphans: agent        # agent | all | ignore
ignore: ["design/**"] # added to the built-in docs/editor/lint defaults
env_ignore: ["BUILD_*", DEPLOY_ID]  # environment variables whose changes select nothing ("*": tracking off)
env_track: [LANG]     # track a variable the built-in ignore list has (suites may also set both)
inputs: [".github/workflows/test.yml"]
full_run_branches: [main, release]
flaky:
  retry: 0            # re-run a suite run whose failures are all known flaky tests, up to N times (default 0: never hide a failure)
agent:
  provider: auto      # auto (from the API key that is set) | anthropic | openai | openrouter
  # model: gpt-6-luna   # default depends on the provider (see "The agent")
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
| `smates run [-- args]` | select, run with tracing, update state. `--all`, `--select`, `--shard I/N`, `--shadow`, `--fail-fast`, `--explain`, `--no-agent`, `--no-record`, `--report file.json`, `--markdown file.md` (summary for a PR comment), `--summary file.md` (append it; default `$GITHUB_STEP_SUMMARY`), `--dry-run` |
| `smates select` | selection only: `--format text\|files\|json\|github\|markdown`, `--markdown file.md` |
| `smates watch [-- args]` | rerun the affected tests on every save: `--interval`, `--debounce`, `--agent`, `--clear`, `--fail-fast` |
| `smates init [--github]` / `list` / `doctor` | setup helpers (`--github` writes a workflow with the right toolchain setup) |
| `smates why <file>` / `deps <test>` | inspect the dependency map (`smates why '$TZ'`: the tests that read a variable) |
| `smates flaky` | tests proven flaky, with evidence: `--format text\|json`, `--clear [test...]` |
| `smates stats` | what the recorded runs saved and how far to trust the selection: `--since 30d\|2026-01-02`, `--branch`, `--ci`, `--format text\|json` |
| `smates state show\|pull\|push\|merge\|clear` | inspect and move the state |
| `smates hook install\|uninstall\|status` | git hook that runs the affected tests: `--hook pre-push\|pre-commit`, `--append`, `--agent`, `-- run flags` |

**Shadow mode** (`smates run --shadow`) runs every test but evaluates the selection and
reports any failing test it would have skipped. Use it for a while before trusting the
selection in a new repository, or permanently on a nightly job.

**Likeliest failures first.** `smates run` starts the selected tests in this order: tests
that did not pass last time; tests whose own file or a file they import directly changed;
tests reached through an indirect dependency (a transitive import, traced data,
configuration, a file-name reference, the agent); then new tests and the rest. Within a
group, the shortest recorded duration goes first. Suites run in the order of their best
test, and each runner gets its files in that order (pytest and `go test` start them in
that order; vitest and jest use their own sequencers, which also put previously failed
files first). With **`--fail-fast`**, each suite is split into one runner call per group
and the calls run group by group across suites; after a call fails, no further call
starts (the running one finishes). Tests that did not start are not recorded, so they
stay selected; the log, the summary and `--report` (`not_run`) list them. Extra runner
start-ups are the price, so the split only happens with `--fail-fast`. It cannot be
combined with `--shadow`.

**Flaky tests** are detected by proof, not by heuristics. smates knows the content hash of
every input of every test, so when a test's outcome flips between pass and fail while none
of those inputs changed (a failing test that passes when re-run, a full run on the default
branch that fails a test nothing touched), the test is flaky — or it depends on something
smates cannot see (time, network, environment variables it ignores or cannot see being
read, files outside the repository). `smates flaky` also lists the variables each flaky
test reads (they were the same across its flips).
`smates run` reports such tests in its log, in `--report` (`flaky`), in the job summary and
in the `flaky` / `flaky-tests` outputs, and remembers the evidence (number of flips, last
commit, recent outcomes) in the state; `smates flaky` lists it. A flaky failure still
fails the run and is still selected next time. With `flaky: {retry: 1}`, when every
failure of a suite run is a test *already known* to be flaky, the suite run is repeated
once with the same tests, and if it then passes the run does not fail (the flip is
reported).

**Stats** (`smates stats`) sum up the runs recorded in the state: how many test units
ran out of how many (skip rate), the recorded test time the skipped units would have
taken (the time saved, also extrapolated to 30 days), shadow-mode misses and their rate,
orphans by how they were handled (reference scan, agent, full suite run), agent calls
with their tokens and cost (when the backend reports one, e.g. `claude -p`), and flaky
flips. Every `smates run` adds a short record (not with `--no-record` or `--dry-run`);
the state keeps the last 1000 records within 180 days, and `smates state merge` unions
them, so the stats of a state restored on CI cover the runs that state went through. A
sharded run leaves one record per shard (its orphans are counted by shard 1 only). The job summary shows the cumulative time saved
in one line.

## Failure hints

When a test fails, smates already knows every file it used and which of them changed, so
`smates run` lists, for each failing test, **the changed files among its inputs** — usually
a handful, often one:

```
smates: failure hints for 1 failing test unit(s):
  web: src/cart.test.ts — 2 of its 37 known inputs changed since its last recorded run (3f2a1c9, passed)
      src/price.ts  (source, direct import, modified)
      fixtures/prices.json  (data, traced, modified)
      $TZ  (environment variable, read in its last run, changed)
    → agent (high confidence): src/price.ts:12 — roundCents now truncates instead of rounding, so the total is off by one cent
```

- "Changed" is measured against the content recorded when that test last ran (or, for a
  test with no record, against the files this run's selection found changed). Candidates
  are ranked: code the test imports directly, other code it uses, the test file itself
  and environment variables it read, data, configuration, directory listings; then by
  modification time.
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

## Watch mode

`smates watch` reruns only the tests a change can affect, every time you save. Unlike a
runner's own watch mode it works for every supported runner (go test, pytest, vitest,
jest, node --test, bun, custom) and across languages, because it uses the same traced
dependencies as `smates run`: editing `testdata/orders.json` reruns the Go package whose
test reads it, editing a YAML file a pytest test loads reruns that test file.

```text
$ smates watch
smates: watching ~/src/shop for changes (go, pytest); orphan policy: agent (...)

── 10:42:07 ── 1 changed: internal/store/testdata/orders.json
smates: selected 1 of 16 test unit(s) across 2 suite(s) (94% skipped)
    + internal/store  [changed: internal/store/testdata/orders.json]
ok  	example.com/shop/internal/store	0.170s
── ✓ PASS · 1 passed · 600ms
   watching for changes (a: run all · f: rerun failed · enter: rerun · q: quit)
```

- Every cycle is a `smates run` (recording included), so the state stays fresh and
  tests that failed are retried until they pass. It starts with one run of the current
  selection. Flags before `--` are the usual `-C`, `--config`, `--state`, `--suite`, `-v`;
  arguments after `--` go to the runner.
- Changes are found by polling `git status` (no file system watcher, no dependency;
  `.gitignore` is respected, git's own locks are never taken). Bursts — atomic saves,
  formatters, checkouts — are debounced into one run, and files saved while tests run
  start exactly one follow-up run. smates' state, `node_modules`, Python and Vite caches,
  coverage, JUnit reports and editor temp files never start a run; a file that every run
  rewrites stops retriggering after three runs in a row. On very large repositories,
  `git config core.fsmonitor true` and `core.untrackedCache true` make each poll cheap.
- Keys (when stdin is a terminal): `a` runs every test, `f` reruns the tests that have not
  passed, `Enter` reruns the last selection, `q` quits. Ctrl-C stops the running tests
  (their whole process tree) and exits.
- `smates watch --fail-fast` runs every cycle with `smates run --fail-fast`: the
  likeliest failures run first and nothing more starts once one fails.
- Unattributed changes follow the orphan policy, but only for files changed while
  watching, and the agent is not called unless you pass `--agent`: with `orphans: agent`,
  an unattributed change runs the suites it can affect in full instead, as with
  `smates run --no-agent`.

## The agent

- Called only for orphans (and, after a failing run, to explain a few failures), so most
  pull requests make no LLM call at all.
- Built-in clients (no SDK) for three providers. With `provider: auto` (the default) the
  first one whose API key is set is used; `agent.provider` picks one explicitly and
  `agent.model` overrides its default model:

  | provider | API key (base URL override) | API | default model |
  |---|---|---|---|
  | `anthropic` | `ANTHROPIC_API_KEY` or `ANTHROPIC_AUTH_TOKEN` (`ANTHROPIC_BASE_URL`) | Messages | `claude-haiku-5-5` |
  | `openai` | `OPENAI_API_KEY` (`OPENAI_BASE_URL`) | Responses | `gpt-6-luna` |
  | `openrouter` | `OPENROUTER_API_KEY` (`OPENROUTER_BASE_URL`) | Chat Completions | `anthropic/claude-haiku-5.5` |

  Low effort and prompt caching by default (the effort is dropped for models that do not
  reason). Typical cost is well under $0.01 per call and a few seconds.
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
- Environment variables are seen only when code reads them through `process.env` (not
  Bun's `Bun.env` / `import.meta.env`), `os.environ` or Go's `os.Getenv`/`os.LookupEnv`.
  What the runtime itself consults (`TZ` for dates, the locale, `GODEBUG`), native code,
  `syscall.Getenv`, and commands smates cannot trace (shell scripts, binaries) are
  invisible. In Go, `t.Setenv` looks the old value up to restore it, so a test that sets
  a variable still depends on its outer value (it may run more often than needed). Values are stored as SHA-256 hashes: a guessable value (a weak password) can
  be confirmed by whoever reads the state file; list such variables in `env_ignore`.
- Code executed in a browser (vitest browser mode, Playwright) is covered by the static
  graph only.
- Granularity is the file (the package for Go, or the test function with
  `granularity: func`): a change anywhere in a module re-runs every test that loaded it.
  Go per-function selection costs one process per test function while recording (the
  default branch's full runs get slower; selective runs only start the selected
  functions); functions share no process, so a test relying on state another test of
  its package left behind in memory behaves differently than under `go test`. Code that
  matters without running (an unexecuted method satisfying an interface) is covered
  through declarations, but code executed through reflection alone is not. For Python, data a module reads at import time or behind a
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

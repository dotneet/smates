# smates design

smates ("smart test selection") selects the tests a change can actually affect, for
TypeScript/JavaScript (vitest, jest, `node --test`, bun; npm, pnpm, yarn, bun), Go and
Python (pytest; uv, virtualenvs), and runs them. It is a single static Go binary (~5 MB)
with no runtime dependencies; language shims (a Node preload, a Python `sitecustomize` and
pytest plugin, reporters for vitest and jest) are embedded and extracted at run time.

## 1. Core idea: per-test content fingerprints over observed inputs

For every test unit `T` (a test file; a package directory for Go) smates stores the
inputs it was observed to depend on, with the content hash (git blob SHA-1) each had when
`T` last ran, and whether `T` passed:

    T -> { deps: {key: hash}, status: pass|fail|unknown, duration_ms, commit }

A test is **skipped** iff it last passed and every input it depends on hashes the same
today. Otherwise it is selected, with the reason. This is the rule of Go's test cache and
Bazel, but with inputs *discovered* by tracing real runs, across languages, and with
content hashes rather than mtimes (so fresh CI checkouts do not invalidate everything).

The dependency set used at selection time is

    D(T) = recorded(T)        traced during T's last run (code, data, listings, probes, subprocesses)
         ∪ static(T) at HEAD  import-graph closure computed now
         ∪ suite inputs       lockfiles and manifests (up to the repo root), runner config,
                              smates.yml, configured globs
         ∪ rule deps          declared in smates.yml

A key present in `D(T)` but absent from the recorded hashes counts as changed, so a newly
added import, conftest.py, Go file in an imported package or rule target triggers the test
without having been recorded. Hashes are compared by content, not by git diff: no history
is needed (`fetch-depth: 1`), dirty working trees work, rebases do not matter.

Dependency keys:

| key | hash | recorded when |
|---|---|---|
| `path` | blob SHA-1 (`""` = absent) | a file is executed or read |
| `dir/` | hash of the entry names | a directory is listed (readdir, os.listdir, Bun.Glob) |
| `path?` | `exists` / `absent` | existence is checked (exists/stat/access, Go `stat`) — also for files that do not exist yet |
| `dir/**` | hash of every non-ignored file below | a command smates cannot see into ran in `dir` |

Soundness argument: an entry's dependencies and hashes are captured from one tree state. If
no input differs between that state and now, the test sees the same inputs and its outcome
is the same — modulo nondeterminism and inputs outside the repository (services, network),
which are handled by global inputs, rules, and the orphan path below.

### Orphans: the part TIA cannot explain

A changed file (relative to the state's full-tree snapshot) that no test's `D(T)` explains
and that is not ignored is an **orphan**: a Dockerfile, a CI script, an SQL migration
applied by a service. `Covers` defines "explains": the file itself is a dependency; or a
listing or existence check of it, but only when the file appeared or disappeared (an
edited file that tests merely list may still be read by something untraced); or a
`dir/**` tree other than the repository root.

Orphans go through, in order:

1. **Name references**: tests whose own dependencies mention the orphan's file name
   (fixtures, scripts, schemas referenced as strings) are selected. Files every test of a
   suite depends on (package.json, lockfiles, runner config) only count when no
   test-specific file mentions the orphan.
2. **The agent** (Claude Haiku by default) explores the repository with read-only tools and
   adds tests or requests a full run. It can never remove deterministic selections, so a
   wrong or manipulated answer can at worst fail to add tests for the orphan.
3. **The orphan policy** (default without agent: run the suites the orphan can affect, by
   suite root).

Orphans handled by a run are written into the snapshot so later runs on the same branch do
not ask again (and the agent's answers are cached per change).

## 2. Tracing per ecosystem

| Runner | Unit | Code deps | Data deps | Attribution |
|---|---|---|---|---|
| vitest | test file | V8 coverage per worker (`NODE_V8_COVERAGE`) | fs hooks (`--require` preload) | one worker per file (vitest `isolate`); globalSetup window marked by the reporter |
| node:test | test file | V8 coverage per process | fs hooks | one process per file (files always passed explicitly) |
| jest | test file | V8 coverage per worker | fs hooks | workers recycled after each file (`--workerIdleMemoryLimit=0`); `isolate: true` = one jest process per file |
| bun | test file | `bun test --coverage` lcov + `require.cache` per process | preload fs + `Bun.file`/`Bun.Glob` hooks | one process per file while recording; shim prepended to bunfig preloads |
| pytest | test file | `sys.monitoring` PY_START (3.12+) / `sys.setprofile` | audit hook `open`, `os.listdir`/`scandir`, wrapped `os.stat/lstat/access` | contexts per collected module, test and fixture; module-level reads via `ReadsBy` |
| go test | package | static import closure (exact for Go) incl. packages named in test string literals | `-test.testlogfile` via `GOFLAGS=-exec=smates` | one binary per package; build failures parsed from output |

Processes are linked to their test even through untraced intermediaries: every traced
process records `pid`/`ppid` and `SMATES_TRACE_PARENT` (inherited through the environment,
so `sh -c "node x | tr"` or `make` in between does not break the chain). A process
without a test of its own belongs to its nearest traced ancestor that has one; processes
under a Go test binary belong to that package. Helpers of the runner itself that execute
repository code (a server from global setup) are global dependencies; helpers that only
read files (crawlers, transformers) are ignored.

**Subprocesses smates cannot trace** (shell scripts, grep, make, compiled binaries) are
analyzed from their command line when the child starts (`child_process.*` in Node,
`subprocess.Popen`/`os.system` audit events in Python):

- read-only commands (`cat`, `grep`, `jq`, ...) depend on the paths and shell globs they name;
- shell scripts are read and analyzed the same way, following `cd`, `source`, variables,
  for-loops, functions, `$(dirname "$0")` and read-only command substitutions;
- anything else may read anything below its working directory: the test depends on
  `cwd/**` (commands running outside the repository only on the paths they name).

**Python module-level state.** pytest imports a module once per process, and caches
(`functools.cache`) run once: a read made then belongs to the module that made it
(`by` in the trace), and is given to every test that depends on that module (`ReadsBy`,
transitively). Granularity is the module: a test that imports a module depends on the data
that module reads.

Static analysis (fast Go scanners, no toolchain): JS/TS imports, requires, dynamic imports
with literal or template prefixes, `new URL(…, import.meta.url)`, mocks; tsconfig paths and
workspace packages (falling back to the whole package when unresolvable). Python imports
including relative, dynamic with literal prefixes, conftest chains. Go imports, embeds, and
package directories named in test string literals (`go run ../../cmd/tool`).

## 3. Status and shadow mode

Per-file status comes from reporters (vitest, jest — injected only when the user configures
none), the pytest plugin, process exit codes (node:test, bun, go), or a JUnit file. When only
the run's exit code is known, failures are *inferred*: such units are re-run next time but
not reported as definite misses. Units that ran but left no trace in a failing run are
`unknown` (re-run next time). Go packages that fail to build are failures.

`smates run --shadow` runs everything while evaluating the selection and reports
*escapes*: tests that failed but would have been skipped (and, separately, unselected
tests whose failure was only inferred).

### Failure hints

The same data explains failures. For a failing unit `T`, the candidates are the keys of
`D(T)` (plus what the failing run itself traced) whose hash differs from the one recorded
in `T`'s previous entry — "changed since this test last ran". Without an entry, they are
`D(T)` ∩ the selection's changed files (snapshot or base revision); in a full run with
neither, there is no basis and the hint says so. The hints are computed per suite right
after it runs and before its results are recorded, since recording overwrites the entry.

Ranking is by tier, then most recent modification, then directory proximity to the test
file: code the test imports directly (static graph), other code, the test file itself,
data, configuration (suite inputs, manifests, lockfiles), then directory listings,
existence checks and opaque trees. A failure with no changed input is reported as such
(flaky, environmental, or untraceable), or as "already failing" when the previous entry
did not pass; orphans of the run are offered as candidates in that case, since they are
exactly the changes smates cannot attribute. A failure inferred from a shared exit code is
marked, as it may belong to another unit.

Reporters also return a bounded failure message per file (vitest and jest reporters, the
pytest plugin, JUnit `<failure>` text, and the per-package block of `go test` output);
runners that only give exit codes (node:test, bun, custom) have none.

The optional explanation is one request per failure without tools: system prompt (fixed,
cacheable), the candidate list, the failure output (≤ 6 KB, ANSI stripped) and the diffs of
the top three candidates (≤ 8 KB, produced like the orphan agent's `git_diff`: recorded blob,
else the base revision or HEAD; ignored files and paths outside the repository are never
read), answered with structured outputs (`file`, `line`, `reason`, `confidence`). At most
`agent.explain_max` (3) per run, non-inferred failures first; requests start as soon as
their suite has finished and run in parallel with later suites, each capped at 30 s; the
run only waits for them before saving the state. Answers are cached in the state's agent
cache under a key of the test, its candidates' hashes and its failure output with
durations, addresses and timestamps masked. Errors are reported in one line and never
change the exit code. The external `agent.command` backend is not used for explanations.

## 4. State

`.smates/state.json.gz` (gitignored), interned JSON (paths and hashes stored once). A full
run rewrites the snapshot and all entries; a selective run updates the entries of the tests
it ran. Moving the state between CI runs is a thin layer: the GitHub Action uses
`actions/cache` (branch, falling back to the default branch); other CIs can use their cache
or `smates state push/pull` (a git ref, `refs/smates/state/<branch>`); shards are combined
with `smates state merge`.

## 5. Agent

Built-in client for the Anthropic Messages API (no SDK), default `claude-haiku-5-5`, low
effort, prompt caching, read-only tools confined to the repository and to files git does
not ignore (`list_files`, `grep`, `read_file`, `git_diff`, `tests_for_file`, `test_deps`,
`search_tests`, `submit_selection`). Bounded by turns, wall-clock time and an input-token
budget; answers are validated against the test inventory and cached per change. Any
failure falls back to the orphan policy. An external agent CLI can be plugged in instead
(`agent.command`).

## 6. Watch mode

`smates watch` is a loop around `smates run`: every cycle loads the project, selects
against the current state and runs with recording, so a watch session leaves the same
state behind as the equivalent `smates run` calls, and later commands (or CI) see it.

**Change detection** polls instead of subscribing to file system events. A watcher would
be a dependency (fsnotify), needs one watch per directory (inotify limits) or per file
(kqueue on macOS, which also misses edits inside watched directories), and still has to
reimplement `.gitignore`. A poll is one `git --no-optional-locks status --porcelain=v2
--branch --untracked-files=all` (cheap with git's stat cache, and with `core.fsmonitor`
on huge trees; it takes no locks, so it never races the user's own git commands) plus an
`lstat` of each listed file. A tree state is HEAD plus the metadata (size, mtime, mode)
of every dirty file. Two states differ in a file when:

- it is dirty in either and its metadata differs (an edit or a revert; staging and
  committing keep the metadata, so they are no change), or
- it is clean in both but HEAD moved and the two commits differ in it (checkout, pull,
  reset), from `git diff --name-only old new`.

Files modified within 2 seconds of a scan are "racy" (coarse file system clocks, e.g. a
Linux tick, can give a quick same-size rewrite the same mtime): their content hash is
recorded too, and the next scan hashes them again. The poll interval backs off to twice
the cost of a scan on trees where `git status` is slow.

**Cycles.** A change starts settling: the tree is polled until nothing changed for the
debounce period (capped at 10× that, at least 5 s, for endless writers), and the net
change since the last cycle starts one; a burst that nets out (a temp file come and
gone) starts none. Changes made while a cycle runs are compared against the tree it
started from once it ends, so they start exactly one follow-up. Changes never start a
cycle when they are smates' state (and its temp file), vendored and cache directories,
coverage and JUnit reports, or editor temp files; anything else does, including files the
orphan check ignores (a test may read them). As a guard against tests that write
non-ignored files, a change set seen during three runs in a row is taken for test
output: it stops starting runs (until it changes while idle).

**Orphans.** Every cycle after the first applies the orphan policy only to orphans among
the files that changed since the previous cycle (the rest were handled at start, or are
branch changes that a state without a full-run snapshot reports on every run). The agent
is called only with `--agent`, so a burst of saves never turns into a burst of LLM calls;
without it `orphans: agent` behaves like `smates run --no-agent`.

**Stopping.** Watch runs test commands in their own process group; cancelling a cycle
(`q`, Ctrl-C, SIGTERM, SIGHUP) sends SIGINT to the whole group and SIGKILL after 5 s, and
kills stragglers that outlive the runner. (Tests get no stdin; one that opened the terminal itself would
be stopped by job control, as a background job.) Keys are read in non-canonical, no-echo
terminal mode with signals left on, so Ctrl-C keeps working (line mode where termios is
not available).

## 7. Verification

- Unit tests for every package (`go test -race ./...`).
- End-to-end fixtures per ecosystem (`e2e/fixtures/*`: vitest, pnpm monorepo, jest, node:test,
  bun, pytest/uv, Go, polyglot) full of dependencies static analysis misses.
  `TestScenarios` checks expected selections and shadow-mode escapes per scenario, and that
  every test a scenario breaks gets a failure hint naming the edited file;
  `TestMutations` breaks every source, data and script file in turn and requires that every
  test failing because of it was selected.
  `TestWatch` runs `smates watch` on the Go, pytest and vitest fixtures, edits files while
  it runs and checks each cycle's selection and result, that runs do not retrigger it, and
  that SIGINT stops it.

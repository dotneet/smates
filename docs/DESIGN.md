# smates design

smates ("smart test selection") selects the tests a change can actually affect, for
TypeScript/JavaScript (vitest, jest, `node --test`, bun; npm, pnpm, yarn, bun), Go and
Python (pytest; uv, virtualenvs), and runs them. It is a single static Go binary (~5 MB)
with no runtime dependencies; language shims (a Node preload, a Python `sitecustomize` and
pytest plugin, reporters for vitest and jest) are embedded and extracted at run time.

## 1. Core idea: per-test content fingerprints over observed inputs

For every test unit `T` (a test file; a package directory for Go, or a top-level test
function with `granularity: func`, see "Go test functions" below) smates stores the
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
         ∪ cross-suite deps   declared in smates.yml (depends_on / serves), expanded at HEAD

A key present in `D(T)` but absent from the recorded hashes counts as changed, so a newly
added import, conftest.py, Go file in an imported package or rule target triggers the test
without having been recorded. Hashes are compared by content, not by git diff: no history
is needed (`fetch-depth: 1`), dirty working trees work, rebases do not matter.

**Cross-suite dependencies.** Tracing follows the test process and its children; it
cannot follow a request into a server running in another process or container (an e2e
suite against a backend started by docker compose). A suite declares that code with
`depends_on`: `{suite: S}` is S's code — the files under S's root minus test files (of
any suite: two suites may share a root), ignored and vendored files, and files of other
suites rooted below S's root (so a suite at the repository root does not swallow the
others) — and `{paths: [globs]}` is repo-relative globs (a leading `./` is stripped,
backslashes are rejected); `tests:` narrows the entry to some of the suite's tests.
`serves: [globs]` is shorthand for a paths entry. The depending suite's own test files are
never added. Each entry is expanded once per selection (`serviceExpander`; a suite's code
once however many suites depend on it), not per test.

A declaration is **one dependency key**, `@set:suite:S` or `@set:paths:<globs>`, whose
hash (`Hasher.WithSets`) is a SHA-1 over the sorted paths and blob hashes of its files at
HEAD. Like rule deps it is static (in `Known`) and recorded with the test, so the state
grows with tests × declarations, not tests × server files, and each set is hashed once
per selection or recording. When the key changed, the explanation names the files of the
declaration among the selection's changed files (the snapshot or base-revision diff,
deleted files included: a declaration is also a path predicate, `ServiceDeps.Match`), each
as `Test.Via = service` with `Test.Service` naming the declaration ("depends_on suite
api"); only when none of them changed since the snapshot (the change happened between
the test's run and the snapshot) is the key itself shown ("code of depends_on suite
api"). `--explain`, `smates why`, the summary and failure hints (which expand the key into
its files) render it. A changed file a declaration matches is covered for the orphan check
— also with `--suite`, since the expansion sees `Input.Others`: the declaration's files go
into `Result.Covered` once, and `Result.Explains` (used by selection and `smates orphans`)
also matches deleted ones. Older smates versions read
the key as a missing file and select the test, which is safe.

Dependency keys:

| key | hash | recorded when |
|---|---|---|
| `path` | blob SHA-1 (`""` = absent) | a file is executed or read |
| `dir/` | hash of the entry names | a directory is listed (readdir, os.listdir, Bun.Glob) |
| `path?` | `exists` / `absent` | existence is checked (exists/stat/access, Go `stat`) — also for files that do not exist yet |
| `dir/**` | hash of every non-ignored file below | a command smates cannot see into ran in `dir` |
| `$NAME` | `env:` + truncated SHA-256 of the value (`""` = unset) | test code read the environment variable (see below) |
| `f.go#decls` | `decls:` + SHA-1 of the file's tokens without function bodies (the blob when it does not parse) | Go test functions: every Go file of the static closure (see "Go test functions") |
| `@set:ID` | `set:` + SHA-1 over the paths and blobs of the set's files | the test's suite declares `depends_on`/`serves` (above) |

Soundness argument: an entry's dependencies and hashes are captured from one tree state. If
no input differs between that state and now, the test sees the same inputs and its outcome
is the same — modulo nondeterminism and inputs outside the repository (services, network),
which are handled by global inputs, rules, cross-suite dependencies, and the orphan path
below.

### Environment variables

A variable a test reads is an input like a file. Only a hash of its value is stored, so a
state file does not leak secrets (but a guessable value can be confirmed against the hash;
such variables belong in `env_ignore`). Unset hashes as `""` and an empty value does not,
so setting a variable that a test probed selects it. The value hashed is the one the
suite's tests see: the suite's `env:` from smates.yml (already an input of every test)
over smates' own environment, without the agent credentials smates withholds from tests.
Variables smates sets to trace a run are not taken into account (they are ignored).

What counts as a read, per runtime (section 2 has the mechanics): `process.env` is
replaced by a proxy in Node and Bun, and jest's per-file copy of it is wrapped the same
way; `os.environ` is patched in Python (`os.getenv`, `.get`, `in`, indexing); Go test
logs already list `getenv` calls (`os.Getenv`, `os.LookupEnv`, the records `go test`
itself uses to invalidate its cache). A lookup is recorded only when

- repository code is on the stack: runners, reporters and libraries look variables up
  for their own bookkeeping in every test, which would make every test depend on them;
  a library called by the test (an SDK reading `AWS_REGION`) does count;
- it is not the test framework saving a value it is about to stub (`vi.stubEnv`,
  `monkeypatch.setenv`);
- the value is still the one the process started with: a test that sets `TZ` and reads
  it back does not depend on the outer `TZ`;
- it is not part of copying the whole environment (`{...process.env}`, `Object.keys`,
  `dict(os.environ)`, `os.environ.copy()`, `items()`): children started with the copy
  record their own reads, and an untraced child is a limitation either way.

Reads that happen while a module is imported once per process are given, like file
reads, to every test depending on the module (`ReadsBy`).

**CI and local.** The state is usually recorded on CI and pulled to a laptop, where many
variables differ for reasons that have nothing to do with the tests: the user and
machine (`PATH`, `HOME`, `USER`, `TMPDIR`, `SHELL`), the locale, the terminal (`TERM`,
`COLUMNS`, color switches), CI metadata (`CI`, `GITHUB_*`, `RUNNER_*`, ...), proxies,
toolchain locations (`GOPATH`, `VIRTUAL_ENV`, `npm_*`) and the variables smates injects
(`NODE_OPTIONS`, `PYTHONPATH`, `GOFLAGS`, `SMATES_*`). Tracking them would select every
test that touches one of them on the first local run, which defeats pulling the state.
They are therefore ignored by default (`config.DefaultEnvIgnore`); `env_ignore` adds to
the list, `env_track` takes names out of it (top level or per suite), and the ignore list
is applied both when recording and when comparing, so adding a name stops it from
selecting tests at once, without a new recording. What remains tracked are the variables
a project defines (`DATABASE_URL`, `APP_*`), `TZ`, `NODE_ENV` and the like. When one of
them differs between CI and a laptop, the tests reading it do run locally — they may
behave differently there, which is the point — and only those tests.

Tracking is **on by default**. Recorded variables only add keys to `D(T)`, so a selection
can only grow compared with not tracking them: the risk is running too much, never too
little, in line with resolving every uncertainty towards running more. The two ways it
could run too much are bounded: systematic CI/local differences are on the ignore list,
and lookups that every test would share (the runner's own) are excluded by the stack
rule, so a differing variable selects the tests whose code reads it, not the suite. A
project that still sees noise lists the variable in `env_ignore`; `env_ignore: ["*"]`
turns the feature off.

Compatibility: env keys are stored in the state's path table like any key, so the format
version is unchanged and older state files load as they are. Their entries simply have no
env keys until the test runs again. An older smates reading a newer state takes `$NAME`
for a missing file and selects those tests (the safe direction).

### Orphans: the part TIA cannot explain

A changed file (relative to the state's full-tree snapshot) that no test's `D(T)` explains
and that is not ignored is an **orphan**: a Dockerfile, a CI script, an SQL migration
applied by a service. `Covers` defines "explains": the file itself is a dependency; or a
listing or existence check of it, but only when the file appeared or disappeared (an
edited file that tests merely list may still be read by something untraced); or a
`dir/**` tree other than the repository root. With `--suite`, the suites left out still
explain the changes their tests depend on (`Input.Others`), so a run limited to some
suites selects what a run over every suite selects for them.

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

`smates orphans` replays the first-parent history (each commit's diff against its first
parent; added and deleted files count as existence changes) against the covered set of an
all-tests selection (`Result.Covered`) and the reference scan, without running anything.
It ranks the orphans, suggests `ignore:` globs (a `dir/**` only when no file below it is a
dependency, named by a test or execution-relevant) and flags CI, build, container and
toolchain files for `inputs:`/`rules:` instead, since ignoring them would hide changes that
can alter test results.

## 2. Tracing per ecosystem

| Runner | Unit | Code deps | Data deps | Attribution |
|---|---|---|---|---|
| vitest | test file | V8 coverage per worker (`NODE_V8_COVERAGE`) | fs hooks (`--require` preload) | one worker per file (vitest `isolate`); globalSetup window marked by the reporter |
| node:test | test file | V8 coverage per process | fs hooks | one process per file (files always passed explicitly) |
| jest | test file | V8 coverage per worker | fs hooks | workers recycled after each file (`--workerIdleMemoryLimit=0`); `isolate: true` = one jest process per file |
| bun | test file | `bun test --coverage` lcov + `require.cache` per process | preload fs + `Bun.file`/`Bun.Glob` hooks | one process per file while recording; shim prepended to bunfig preloads |
| pytest | test file | `sys.monitoring` PY_START (3.12+) / `sys.setprofile` | audit hook `open`, `os.listdir`/`scandir`, wrapped `os.stat/lstat/access` | contexts per collected module, test and fixture; module-level reads via `ReadsBy` |
| go test | package | static import closure (exact for Go) incl. packages named in test string literals | `-test.testlogfile` via `GOFLAGS=-exec=smates` | one binary per package; build failures parsed from output |
| go test, `granularity: func` | top-level test function | coverage profile per function (`go test -c -cover -coverpkg=<linked repository packages>`) + declarations of the static closure | `-test.testlogfile` of the function's process | one process per function (`-test.run '^Name$'`) |

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

**Environment variables** (see section 1). Node and Bun: `process.env` becomes an
accessor returning a proxy (re-created when a test assigns `process.env`); `get`, `has`
and `getOwnPropertyDescriptor` record a name after checking the stack (`Error().stack`,
at most 32 checks per name and context); an `ownKeys` call followed, within the same
tick, by lookups of the returned keys in order is a copy. jest-util's
`installCommonGlobals` is shadowed on load so the copy jest gives each test file is
proxied too; vitest's `import.meta.env` reads through `process.env`. Bun's `Bun.env` is
not configurable and is not traced. Python: `os._Environ.__getitem__` (behind `get`, `in`
and `os.getenv`) records the name with the nearest repository frame as `by`;
`__iter__` remembers the frame and instruction that enumerates, and lookups from that
same instruction are the copy. Go: `getenv` lines of the test log; they carry no stack,
so the lookup `t.Setenv` makes to restore a variable later counts as a read (the test
depends on the outer value of a variable it sets: more runs, never fewer).

**Go test functions.** With `granularity: func` a unit is `dir#Name` for every top-level
`TestXxx(*testing.T)`, `FuzzXxx(*testing.F)` and `ExampleXxx()` found by parsing the
package's `_test.go` files (a file that does not parse makes the package itself a unit).
Recording builds the package's test binary once with `-cover -coverpkg=` set to the
main-module packages it links (`go list -deps -test`), lists its tests (`-test.list`),
and runs each selected function in its own process (`-test.run '^Name$'`, a coverage
profile, a test log, `GOCOVERDIR` and the Node/Python tracing environment in a trace
directory of its own), bounded by `record_jobs` per package (default 1, like `go test`)
and by `-p` packages at once (command or `GOFLAGS`, default GOMAXPROCS, like `go test`).
Test flags of `GOFLAGS` and of the command are translated to `-test.*` flags of the
binary, `GOFLAGS` first so the command's win. Its dependencies are the files with an executed block
(blocks placed by `//line` in a file that is not there: every Go file of the package);
the declarations (`f.go#decls`) of every Go file of the static closure, which is what
makes this sound: coverage only sees statements inside function bodies, so constants,
variable initializers, types, signatures and method sets are hashed separately (as
tokens, so formatting does not count) and a change to them selects every function that
could see them, while an edit inside a body selects only the functions that executed
it; the content of Go files of packages the binary does not link (a test that runs
`go run ../cmd/tool`); the content of its own test file, of test files declaring a name
its file mentions (transitively) and of test files with `TestMain`, `init` or
package-level variables initialized by a call, and the declarations of the package's
other test files (without their test functions, so adding a test does not select its
neighbours); the listing of the package directory (a new file selects every function
of the package); and everything the package unit has besides (test log reads and
environment variables, non-Go files of the closure, `go.mod`/`go.sum`, children). A
missing profile makes every file of the closure a content dependency. When per-function
recording is impossible (the binary does not build with coverage, `go list` fails, the
command is not a plain `go test` or passes `-run`, `-skip`, `-bench`, `-fuzz`, `-c`,
`-o`), the package is recorded through the package path with `-run` restricted to the
selected functions, and each function gets the package's observation with every Go file
of its closure as content: package granularity under function IDs, with a warning.
Without recording, selected functions run with `go test -run '^(A|B)$'` (one pattern
for as many packages as fit in 16 KiB; a function of the same name in another of those
packages runs too; a package that alone needs more runs whole), and outcomes come from
the top-level `--- FAIL/PASS/SKIP` lines (or `-json` events) before each package's
summary line: a package that passed passes all its functions, a failed build fails them
all, other functions of a failed package are unknown (inferred).

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

### Run order and `--fail-fast`

`smates run` orders what it runs so the likeliest failures finish first. Priorities come
from the selection reasons (what `--explain` prints) and the state, and are set on the
selected tests (`priority` in `--report`):

1. the unit did not pass last time (reason `failed`, or a recorded `fail` even when an
   input changed since);
2. a changed input is the unit's own test file, or a file one of its test files imports
   directly (`Graph.Direct`);
3. an indirect dependency changed (a transitive import, a traced read, listing or
   command, a suite input or rule), or the unit was pulled in by a name reference, the
   agent or the orphan policy;
4. anything else: new units, full runs, watch reruns.

Within a priority, units with a recorded duration go first, shortest first.

What smates controls is limited by the runners. The order of **suites** is fully under its
control. Within one runner call it only controls the order of the arguments: pytest
collects in that order (unless a plugin such as pytest-randomly reorders), `go test`
builds and runs packages in parallel but starts them in that order, `node --test` and the
one-process-per-file recording runs (bun, `isolate: true`) start files in that order with
bounded concurrency. Vitest and jest ignore it: their sequencers sort by their own cache
(previous failures first, then the longest files, to balance workers). So by default
smates passes units in priority order and runs suites by their best priority — free, and
the outcome is the same.

`--fail-fast` needs more: a failure deep in one big runner call is only known when the call
ends. With `--fail-fast` each suite is split into one call per non-empty priority, the
calls run by priority across suites (suite order breaking ties), and once a call exits
non-zero (after retries of known flaky tests) no further call starts. A call that is
running is left to finish rather than killed: its units are mostly done, killing would
leave partial traces and unknown outcomes for all of them, and the runner's own bail flags
(`--bail`, `-x`, `-failfast`) remain available after `--` for users who want the runner to
stop too. Splitting costs one runner start-up per extra call (a vitest or jest start-up is
around a second; `go test` reuses its build cache), which is why it is not the default: a
plain run pays nothing for the ordering. A full suite run (`--all`, the default branch)
under `--fail-fast` is split too when some of its units failed last time, passing explicit units instead of letting the runner
discover its tests, so previously failed units can run first without running twice.

Recording stays on the safe side. Outcomes of a suite's calls are merged and recorded once
per suite; units that did not start get no observation, so their old entry (or the
absence of one) stays and they are selected again for the same reason. A suite's entries
are replaced wholesale (the full-run rule) only when every call of it ran. A run that
stopped early does not touch the repository-wide snapshot, neither the full-run refresh
nor the orphans it handled: tests that would have covered them may not have run. Results,
failure hints, flake evidence, the summary and `--report` only contain what ran;
`--report` lists the rest under `not_run` and the summary and log count them.
`--fail-fast` is rejected with `--shadow`, whose point is to run everything.

### Flaky tests

Because every entry stores the content hash of every input, a flaky test can be detected
by proof: if a unit's outcome flips between pass and fail while its inputs are the same,
something nondeterministic or invisible to smates decided it. "The same inputs" means one
of:

- every dependency recorded by the previous run still has the same hash, and every file
  the unit depends on now but did not record then is unchanged since the recorded
  snapshot (so a dependency the previous trace missed cannot hide a real change); an
  environment variable read for the first time has nothing to compare with, so it rules
  this out;
- an earlier outcome in the unit's history has the same *fingerprint* (a hash of its whole
  dependency map);
- both outcomes come from the same run (a failed attempt and its retry).

Only outcomes reported for the unit itself count: inferred statuses, `unknown`, runs with
runner arguments passed through (`smates run -- ...`), and entries written before outcome
histories existed are never evidence. Each entry keeps its last 8
outcomes (status, fingerprint, time) and, once flaky, the evidence (flip count, first and
last flip, commit). Detection never changes selection: a failure is re-run next time as
always. The opt-in `flaky.retry` re-runs the suite run when every failure is a unit
already known to be flaky, and lets the run pass if the re-run passes; the flip is
recorded and reported like any other. The re-run covers the same units as the failed
run, not only the failing ones: a run can also fail for reasons no unit reports (an
unhandled error, teardown, a crash), which re-running the flaky units alone would hide.

### Failure hints

The same data explains failures. For a failing unit `T`, the candidates are the keys of
`D(T)` (plus what the failing run itself traced) whose hash differs from the one recorded
in `T`'s previous entry — "changed since this test last ran". Without an entry, they are
`D(T)` ∩ the selection's changed files (snapshot or base revision); in a full run with
neither, there is no basis and the hint says so. The hints are computed per suite right
after it runs and before its results are recorded, since recording overwrites the entry.

Ranking is by tier, then most recent modification, then directory proximity to the test
file: code the test imports directly (static graph), other code, the test file itself,
environment variables the test read (tied with the test file), data, configuration
(suite inputs, manifests, lockfiles), then directory listings,
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
with `smates state merge`, which keeps the newer entry of each test but unions outcome
histories and flake evidence (flip counts take the maximum, since both sides usually share
a past). Outcome histories and flake evidence are optional fields, so state files from
before them still load and the format version is unchanged.

The state also keeps a short record of each `smates run` (`runs`: selection mode, units
selected/total, recorded duration of the skipped units, exit code, shadow misses, orphan
handling, agent usage, flaky flips) for `smates stats`. Records carry a random ID, so
`smates state merge` unions them without double counting what both sides inherited; the
list is bounded to the newest 1000 records within 180 days of the newest one. It is an
optional field too. A sharded run records only the units its shard owns (and only shard 1 records the
orphans, which every shard sees), so the records of all shards add up to one selection. Records describe the state's own lineage: a state
restored from the default branch carries the default branch's records forward.

### Summaries for reviewers

`internal/summary` renders a selection (and, for `run`, its result) as Markdown for the
job summary and for pull request comments (`--markdown`, `select --format markdown`,
the action's `comment` input). The selection carries the "why" it needs: for each
changed dependency of a selected test, `Test.Via` says how it reached the test (the test
file itself, a suite input or rule, a `depends_on`/`serves` declaration (named in
`Test.Service`), the static import graph, or only the traced reads of
its last run), additions for orphans carry the orphan files (`Addition.Files`), and
`Result.OrphanHandling` records whether each orphan was explained by a reference scan,
the agent (or judged irrelevant by it), the `orphans` policy, or ignored. Time saved is
the sum of the recorded durations of the skipped tests. A sharded summary covers only
the tests its shard owns, because each shard's state only knows those. The output is
deterministic and bounded (per-file and per-list truncation with counts, collapsible
sections); other features append their own sections (`summary.Section`).

## 5. Agent

Built-in clients (no SDK) for the Anthropic Messages API (default `claude-haiku-5-5`), the
OpenAI Responses API (default `gpt-6-luna`, stateless: `store: false`, reasoning carried
encrypted between turns) and OpenRouter's Chat Completions API (default
`anthropic/claude-haiku-5.5`). `agent.provider: auto` uses the first provider whose API
key is set, in that order; `agent.model` overrides the provider's default. One tool loop
drives all three through a small per-provider `dialect` (request encoding, response
decoding, errors). Low effort (dropped once a model rejects it), prompt caching,
read-only tools confined to the repository and to files git does
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

**Fail fast.** `smates watch --fail-fast` passes `--fail-fast` to every cycle; the footer
counts what ran and how many selected units were not started. Rerun (`Enter`) still
reruns the whole previous selection, not only what started.

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
  test failing because of it was selected. `TestFlaky` flips a test through a file outside
  the repository and checks detection, reselection, `smates flaky` and `flaky.retry`.
  `TestWatch` runs `smates watch` on the Go, pytest and vitest fixtures, edits files while
  it runs and checks each cycle's selection and result, that runs do not retrigger it, and
  that SIGINT stops it. Fixtures, scenarios and mutations run in parallel on a pool of
  prepared repositories per fixture; CI splits them into shards of about equal run time
  (`SMATES_E2E_FIXTURE` takes a list of fixtures, or of `-fixture` exclusions), one e2e
  suite in smates.yml per shard so that smates selects them per fixture.

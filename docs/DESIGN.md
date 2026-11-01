# smates design

smates ("smart test selection") selects the tests a change can actually affect, for
TypeScript/JavaScript (vitest, jest, `node --test`, bun; npm, pnpm, yarn, bun), Go and
Python (pytest, unittest; uv, virtualenvs), and runs them. It is a single static Go binary (~5 MB)
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
                              configured globs, smates.yml (hashed as the suite's settings)
         ∪ rule deps          declared in smates.yml
         ∪ cross-suite deps   declared in smates.yml (depends_on / serves), expanded at HEAD

Every test depends on its suite's settings in smates.yml through the key
`smates.yml#suite`, which hashes as `ConfigHash`: the suite's own entry as written and
the top-level `env_track`, not the file's content. The defaults smates fills in are not
part of it (`runner.Suite.Configured`): they depend on the machine (the pytest command
on whether a virtualenv exists, Go's excludes on `GOWORK`), so a state recorded on CI
would otherwise rerun every suite on a laptop or in another worktree. An entry recorded
by an older smates hashed the suite with its defaults; it counts as unchanged while
those are the same, until the test runs again. A test that reads smates.yml itself
records the file and depends on its content as usual. The other
settings cannot change a test's outcome (ignore globs only change the `dir/**` hashes
they affect, input globs and rules add keys of their own, `env_ignore` applies when
comparing), so editing them selects nothing. Diff mode (no recorded state) still
compares the file by content. An entry recorded by an older smates holds the file's blob
hash instead of the key; it counts as unchanged while the file is, until the test runs
again.

A key present in `D(T)` but absent from the recorded hashes counts as changed, so a newly
added import, conftest.py, Go file in an imported package or rule target triggers the test
without having been recorded. Hashes are compared by content, not by git diff: no history
is needed (`fetch-depth: 1`), dirty working trees work, rebases do not matter.

A file's hash is its blob SHA-1 followed by `+x` (`gitx.ExecSuffix`) when git records it
as executable (mode 100755: in the index, or for a dirty or untracked file the owner's
executable bit on disk; with `core.fileMode=false` the bit on disk is ignored as git
ignores it: a tracked file keeps the index's mode, an untracked one is not executable), so a `chmod -x` on a script a test runs selects the test, committed or not.
git status and git diff report a mode-only change as a modification, so the snapshot,
diff mode and watch mode see it too. A symlink hashes as its target path (as git stores
it), a submodule as `commit:` and its pinned commit. Entries recorded before the marker
existed hold the plain blob of executable files: tests depending on one run once more
after upgrading, then settle.

A path a test reads through a symlink inside the repository depends on the file it
resolves to and on every link on the way (`pathutil.Links`: the file itself, a linked
directory, a link to a link), each hashed as its target: retargeting
`data/current.json -> v2.json` selects the tests that read `data/current.json`. Links
outside the repository (a root reached through one) and vendored ones are left out. A
module Node or Bun loads through a link is traced by its resolved path only (the
runtime resolves the link before loading it; coverage lists the target), so retargeting
such a link may be an orphan.

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
| `path` | blob SHA-1, `+x` appended for an executable file (`""` = absent) | a file is executed or read (also a symlink the path went through) |
| `dir/` | hash of the entry names (of a directory git ignores entirely: from disk) | a directory is listed (readdir, os.listdir, Bun.Glob) |
| `path?` | `exists` / `absent` | existence is checked (exists/stat/access, Go `stat`) — also for files that do not exist yet |
| `dir/**` | hash of every non-ignored file below (of a directory git ignores entirely: paths and sizes on disk) | a command smates cannot see into ran in `dir` |
| `$NAME` | `env:` + truncated SHA-256 of the value (`""` = unset) | test code read the environment variable (see below) |
| `f.go#decls` | `decls:` + SHA-1 of the file's tokens without function bodies (the blob when it does not parse) | Go test functions: every Go file of the static closure (see "Go test functions") |
| `@set:ID` | `set:` + SHA-1 over the paths and blobs of the set's files | the test's suite declares `depends_on`/`serves` (above) |

The hashes are those of the snapshot taken before the run. Paths git does not list
(ignored files such as `.env`, caches, reports, generated code) are read from disk
instead. Existence checks of them are taken before the first test starts, for the
paths the entries of the suites being run probed (`Hasher.PinBefore`), so a test that
probes an output directory and then creates it is recorded as having seen it absent
(it runs once more, then settles on `exists`). A path first probed when the run is
recorded that exists and was modified since the run started may have been created by
the tests: it is recorded as `unsettled`, which matches nothing, so the test runs again
and then records the path's state before that run. "Since" allows 100ms before the
start, since file systems date files with a coarser clock than smates' (Linux: the
kernel tick); a path generated more than that before the run (`make gen && smates run`)
existed before it. Whole-second timestamps (HFS+, ext3) count from the start's second;
on FAT, with two-second timestamps, a path created in the first second of the run can
still look older and be recorded as existing before it. The *content* of such files is
hashed as the run left it: tests commonly rewrite what they read there (a timestamped
report, a cache, Go tests creating files in the package), and their state before the
run would never match again. The flip side: a test that reads what it generates itself
in an ignored path runs again on every fresh checkout, where the path is absent before
the run. Known limitation: a git-ignored input edited by hand while
the run is in progress is recorded with its new content, so the edit selects nothing.

A directory the snapshot has no file below (one git ignores entirely: generated
fixtures, `build/`) is listed from disk too, without vendored entries and those the
`ignore` globs match, as the run left it like content (a test that adds a timestamped
report to the directory it lists settles after one run). The listing of a directory the
snapshot has files below is the snapshot's alone: entries git ignores inside a tracked
directory are not part of it. That is a known gap (a git-ignored file added next to
tracked ones selects no test that lists the directory, unless the test read it), kept
on purpose: with `granularity: func` every Go test function depends on its package's
listing, so a log a test writes into the package, a `go test -c` binary, `cover.out`,
profiles, `.DS_Store` or an editor's swap file would rerun the whole package (on every
fresh checkout, for files a test writes there), where only the test reading the file
should run. A `dir/**` tree of a directory git ignores entirely is the relative paths
and sizes of its files on disk, without vendored ones and those the `ignore` globs
match; contents are not read (build output can be gigabytes, and a build that is not
reproducible would rerun the tests forever), so a rebuilt file of the same size is no
change. A tree of more than 20,000 entries hashes as a constant (`tree:over:20000`): its
changes select nothing while it stays that large. A tree the snapshot has files below
leaves its ignored files out.

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

**Enumeration is not tracked, also when it filters.** Code that enumerates the
environment and picks variables itself (`Object.entries(process.env)` filtered on
`MYAPP_`, `{k: v for k, v in os.environ.items() if k.startswith("MYAPP_")}`,
pydantic-settings and other settings loaders that scan `os.environ`, a config library
merging every variable with a prefix) reads through the same operations as a copy, so
none of the variables it picks are recorded: changing `MYAPP_URL`, or setting a new
`MYAPP_*` variable, selects nothing. Recording such an enumeration as a dependency would
need the whole environment as the key, which differs between any two machines and shells
and would rerun the tests on every run. There is no configuration that declares a
variable as an input: `env_track` only takes a name out of the ignore list (it matters
only for a variable that is read by name), and `inputs:`/`rules:` are file globs. The
ways to make such settings count are to read the variables by name in the code under
test (`process.env.MYAPP_URL`, `os.environ["MYAPP_URL"]`, `os.getenv`), which records
them; to set them in the suite's `env:` in smates.yml, which is part of the suite's
settings (`smates.yml#suite`) and overrides the shell, so changing a value there reruns
the suite; to load them from a file the tests read (a dotenv file, tracked or ignored, is
a file dependency); or to run `smates run --all` after changing the environment.

Reads that happen while a module is imported once per process are given, like file
reads, to every test depending on the module (`ReadsBy`).

**CI and local.** The state is usually recorded on CI and pulled to a laptop, where many
variables differ for reasons that have nothing to do with the tests: the user and
machine (`PATH`, `HOME`, `USER`, `TMPDIR`, `SHELL`), the locale, the terminal (`TERM`,
`COLUMNS`, color switches), CI metadata (`CI`, `GITHUB_*`, `RUNNER_*`, ...), proxies,
toolchain locations (`GOPATH`, `VIRTUAL_ENV`, `npm_*`) and the variables smates injects
(`NODE_OPTIONS`, `BUN_OPTIONS`, `PYTHONPATH`, `GOFLAGS`, `SMATES_*`). Tracking them would select every
test that touches one of them on the first local run, which defeats pulling the state.
They are therefore ignored by default (`config.DefaultEnvIgnore`); `env_ignore` adds to
the list, `env_track` takes names out of it (top level or per suite), and the ignore list
is applied both when recording and when comparing, so adding a name stops it from
selecting tests at once, without a new recording. Known limitation: a name taken out of
the top-level `env_ignore` was never recorded, so that selects nothing either; the tests
reading the variable depend on it from their next run on (`smates run --all` records it
for every test). (`env_track` and a suite's own lists are part of the suite's settings,
`smates.yml#suite`, and changing them reruns the suite.) What remains tracked are the
variables a project defines (`DATABASE_URL`, `APP_*`), `TZ`, `NODE_ENV` and the like. When one of
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
suites selects what a run over every suite selects for them; and a change that only
suites left out would run for under the orphan policy (it is under their root, not under
the root of a picked suite, and no suite rooted at the repository root is picked) is not
an orphan of the run either (`selector.Ownership.Owned`): the job that runs those suites handles
it. A `--suite` run that runs every test of its suites (`--all`, `--shadow`) refreshes
the snapshot for them: a file only its suites own gets its hash; a file the suites left
out also own (or own alone) gets a marker `?<hash>@<suites>` (`state.MarkerSnap`), the
content these suites saw, unless its entry already equals the current hash (or holds a
content hash and only suites left out own it). A later `--suite` run finds a marked
file unchanged only when every suite it runs saw that content; a run of other suites,
or of every suite, finds it changed (and, to the agent and failure hints, unrecorded),
so a shared state (local runs, `state merge`, CI without a `cache-key` per job) never
hides a change from the suites that have not compared it. Orphans a `--suite` run
handles are recorded the same way.

Orphans go through, in order:

1. **Name references**: tests whose own dependencies mention the orphan's file name
   (fixtures, scripts, schemas referenced as strings) are selected. Files every test of a
   suite depends on (package.json, lockfiles, runner config) only count when no
   test-specific file mentions the orphan.
2. **Untested code** (`untested: skip`, the default): an orphan in a language a suite
   analyzes statically (Go, Python, JS/TS) under that suite's root is imported by no
   test, directly or transitively (every test's import closure is in its `D(T)`), and
   was neither read nor executed by any test in its last run. No test can be affected,
   so it selects nothing and is reported as a change to untested code. Code configuring
   tools (`*.config.*`, `*rc.*`, `setup.py`, setup files, `.d.ts`), source under data
   directories (`testdata/`, `fixtures/`, `__mocks__/`, ...) and files a suite excludes
   are left out, and so are files under the root of a suite with a test that has no
recorded entry yet; so is everything without a recorded state, where `D(T)` holds only the
   import closure and files tests read as data are not known yet. Code run by another
   process must be declared (rules), as for any dependency tracing
   cannot see.
3. **The agent** (Claude Haiku by default) explores the repository with read-only tools and
   adds tests or requests a full run. It can never remove deterministic selections, so a
   wrong or manipulated answer can at worst fail to add tests for the orphan.
4. **The orphan policy** (default without agent: run the suites the orphan can affect, by
   suite root).

A calling agent (a coding agent running smates) can take the place of steps 2 and 3:
`smates select --format json` reports `orphan_candidates` (status, the tests that name
each file from step 1's `selector.Mentions`, the suites `OrphanAll` would run, the
handling), `--add suite:unit|test-file` adds tests with reason `caller` (through
`Result.Apply`, so it can only select), and `--orphans=agent|full|skip` overrides the
policy for one run. `skip` is the one explicit opt-out: the orphans are recorded as
handled `skipped` ("skipped by caller" in the summary and `smates stats`). Step 1 still
runs, and nothing the caller passes can deselect a test.

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
| node:test | test file | V8 coverage per process | fs hooks | one process per file (files always passed explicitly, escaped as globs on Node 21+); `isolate: true` = one process per file under `--test-isolation=none` |
| jest | test file | V8 coverage per worker | fs hooks | workers recycled after each file (`--workerIdleMemoryLimit=0`); `isolate: true` = one jest process per file |
| bun | test file | `bun test --coverage` lcov + `require.cache` per process, read by a global `afterAll` registered from the preload (bun test emits no exit event; lcov leaves out JSON loaded at run time) (no lcov: bun covered nothing, or the file failed; only a passing file whose static closure has source files is then not recorded) | preload fs + `Bun.file`/`Bun.Glob` hooks (a glob lists the literal directory before its first glob segment when only the last segment is one, else may read everything below it); the test file's `__snapshots__/<file>.snap` when it exists after the run (bun reads snapshots natively, past the hooks) | one process per file while recording; shim prepended to bunfig preloads (top-level `test.*` dotted keys and `test = {...}` move into the `[test]` table); bun children preload it through `BUN_OPTIONS` (bun ignores `NODE_OPTIONS --require`) |
| pytest | test file | `sys.monitoring` PY_START (3.12+) / `sys.setprofile` | audit hook `open`, `os.listdir`/`scandir`, wrapped `os.stat/lstat/access` | contexts per collected module, test and fixture; module-level reads via `ReadsBy` |
| unittest | test module | as pytest | as pytest | selected modules passed by dotted name (relative to the discovery top-level dir) and run in one process by `python -m smates_unittest` (unittest's main, patched loader/suite and a result class): contexts per imported module and per module's tests including `setUpModule`/`setUpClass` and their teardowns; per-module status from the result class (a module that ran no test passes when it is a helper, and is `unknown` when it looks written for pytest or the run stopped early, `--failfast` or an interrupt, as is the module running then); `isolate: true` = one process per module |
| go test | package | static import closure (exact for Go) incl. packages named in test string literals | `-test.testlogfile` via `GOFLAGS=-exec=smates` | one binary per package; build failures parsed from output |
| go test, `granularity: func` | top-level test function | coverage profile per function (`go test -c -cover -coverpkg=<linked repository packages>`) + declarations of the static closure | `-test.testlogfile` of the function's process | one process per function (`-test.run '^Name$'`) |

Test files are what the runner itself would collect: without `tests:` in `smates.yml`,
the adapter reads its runner's configuration statically (jest `testMatch`/`testRegex`/
`roots`/`testPathIgnorePatterns`, vitest `include`/`exclude`/`dir` and projects united
into one suite, pytest `testpaths`/`python_files`/`norecursedirs`, Go modules nested in
the suite's, which belong to it only through the go.work workspace) into matchers relative to the
config's base directory, and falls back to its default globs, with a `doctor` warning,
when the config is not literal. Derived patterns do not mark the suite narrowed: a full
run still lets the runner discover its tests, so an approximation (a converted
extglob, a JS regex run by Go) can never make a full run skip a test the runner has.

Processes are linked to their test even through untraced intermediaries: every traced
process records `pid`/`ppid` and `SMATES_TRACE_PARENT` (inherited through the environment,
so `sh -c "node x | tr"` or `make` in between does not break the chain). A process
without a test of its own belongs to its nearest traced ancestor that has one; processes
under a Go test binary belong to that package. Helpers of the runner itself that execute
repository code (a server from global setup) are global dependencies; helpers that only
read files (crawlers, transformers) are ignored.

**Coverage tools sharing the same mechanisms.** V8 precise coverage is one per isolate:
vitest's v8 provider and jest's v8 coverage take it through `node:inspector` and then stop
it, so the `NODE_V8_COVERAGE` file of such a worker lists almost nothing; the Node shim
wraps `inspector.Session.prototype.post` and records the scripts of every
`Profiler.takePreciseCoverage` result (left untouched). c8 adopts a set
`NODE_V8_COVERAGE` as its temporary directory and empties it on start (`--clean`), so
every runner invocation gets its own `v8`, `v8-1`, … directory. A `NODE_V8_COVERAGE` the
user set is replaced while recording and the files are copied there afterwards (never
overwriting); one inherited from an outer smates run is not the user's. The preload lives
under `shims/node_modules/` so coverage reports (which skip `node_modules`) never list it.
Python: smates takes a `sys.monitoring` tool id of its own (3, 4, 2, 5; coverage.py's
sysmon core takes 1 and moves up), or chains to an existing `sys.setprofile` function;
the pytest plugin switches to a `session:end` context (attributed to nothing) once the
test loop is over, before pytest-cov reads every `--cov` module for its report. Go:
`-coverpkg` builds the named packages into every test binary of the invocation, so their
source files (`go list -find`) are added to every package's dependencies.

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
proxied too; vitest's `import.meta.env` reads through `process.env`. A proxy cannot be
cloned: `structuredClone`, `MessagePort`/`BroadcastChannel` `postMessage`, a Worker's
`workerData` and `v8.serialize` get the environment object itself instead (as the value
or one level down, `{env: process.env}`), a copy rather than a read. Bun's `Bun.env` is
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
with literal or template prefixes, `path.join(__dirname, …)`, `new URL(…, import.meta.url)`,
mocks and their manual mocks in `__mocks__/`; tsconfig paths and workspace packages (falling
back to the whole package when unresolvable). Python imports including relative, dynamic
with literal prefixes, conftest chains (and the directories pytest's prepend import mode puts
on `sys.path` for them). Go imports, embeds, cgo headers found through `-I${SRCDIR}/…`, and
package directories or `.go` files named in test string literals (`go run ../../cmd/tool`).

## 3. Status and shadow mode

Per-file status comes from reporters (vitest, jest — injected only when the user configures
none), the pytest plugin, process exit codes (node:test, bun, go), or a JUnit file. When only
the run's exit code is known, failures are *inferred*: such units are re-run next time but
not reported as definite misses; so is a process exit code that covers several units (node
`--test-isolation=none`). Units that ran but left no trace in a failing run are
`unknown` (re-run next time), and so are files the vitest reporter saw cut short (`--bail`,
an interrupted run: never started, or tests that were declared to run but did not
finish). Go packages that fail to build are failures.

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
`--report` lists the rest under `not_run` and the summary and log count them. An
interrupted run (Ctrl-C) stops the same way: the batch it cuts short is not recorded
either (a killed runner fails its tests or reports nothing), what finished is recorded
and reported (`interrupted` in `--report`), and smates exits 1.
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

**Worktrees.** The state is per working tree by default. A linked worktree (its git dir
differs from the common dir) without a state is seeded on first use (`stateStore` in
`internal/cli/statestore.go`): from the main worktree's state file (the first entry of
`git worktree list --porcelain`), else from the remote state ref, fetched under the
hook's once-an-hour stamp (refs live in the common dir, so one worktree's fetch serves
all). Per-test entries are valid in any tree (each one is re-checked against its own
dependency hashes), but the full-tree snapshot is not: the snapshot of another branch
would report that branch's differences as orphans here. So the seeded snapshot is kept
only when its commit is an ancestor of HEAD; otherwise it is dropped with its commit and
time, and changed files come from the merge base with the default branch, as for a state
without a snapshot. (No snapshot time makes the hook treat the state as stale and merge
in a newer remote one, as for any state without a snapshot.) A `.state.json.gz.seeded`
marker records the source (for `state show`) and keeps a cleared state from being seeded
again; `state_seed: false` / `SMATES_NO_STATE_SEED` turn it off.

With `state_shared: true` the state is split by what it describes. The entries, the
agent cache and the run records describe the code and are shared: one file,
`<git common dir>/smates/state.json.gz`. The snapshot (with its commit and time)
describes one working tree and stays in it: `.smates/state.snapshot.json.gz`, read in
place of the shared file's snapshot. A worktree without one has none (no files, commit
or time), and judges changed files against the default branch.

Loading reads the shared file once and decodes it twice: the process's working copy and
`base`, what it started from. (Two reads without the lock could see two versions; a
`base` newer than the working copy would make an entry another worktree just updated
look changed by this process, and the save would put the older copy back.) Writes are
load-merge-save under an exclusive lock file (`O_CREATE|O_EXCL`, portable to every OS
and file system git runs on; a lock older than two minutes is stale, since a save takes
seconds). Inside the lock the shared file is re-read and `State.MergeChanges(base, ours)`
applies this process's changes, a three-way merge per entry:

- an entry ours left unchanged keeps the disk's version (another worktree may have
  updated it), and stays deleted when the disk no longer has it (`state clear`, or
  another process's full run pruned it);
- an entry only ours added or changed replaces the disk's outright (so `flaky --clear`
  is not undone);
- an entry both changed is combined like `state merge` (newer `At` wins, histories
  unioned); flake evidence one side cleared stays cleared unless the other side
  recorded a flip after `base` (new evidence);
- an entry ours deleted (a full run drops units that no longer exist) is deleted unless
  the disk's changed since `base`. A test that exists only on another branch's worktree
  is thus pruned by a full run here and recorded again by the next run there.

Agent answers and run records this process added are added; ones it only inherited are
not, so a cleared state does not get them back. After a save, the working copy takes
the merged entries and `base` is re-read under the lock. The shared file also carries
the last writer's snapshot, so it stays a complete state for `state push` (which in
shared mode pushes the entries with this worktree's snapshot) and for older smates
versions. `state pull` in shared mode merges into the shared entries instead of
replacing them.

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
budget; answers are validated against the test inventory and cached per change. Unknown
tests are dropped (the built-in backend asks once for a correction); an answer that
names tests but none that exist (or decides `tests` without listing any) is a failure,
never an empty selection, and is not cached. Any failure falls back to the orphan
policy. An external agent CLI can be plugged in instead (`agent.command`).

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
coverage reports (the `coverage/` directory of the repository or of a suite, and report
files such as `lcov.info` anywhere: a `coverage` package elsewhere is source code),
JUnit reports, or editor temp files; anything else does, including files the
orphan check ignores (a test may read them). As a guard against tests that write
non-ignored files, a change set seen during three runs in a row is taken for test
output: it stops starting runs (until it changes while idle).

**Orphans.** Every cycle after the first applies the orphan policy only to orphans among
the files that changed since the previous cycle (the rest were handled at start, or are
branch changes that a state without a full-run snapshot reports on every run); only
those are written into the snapshot as handled, so a `smates run` still applies the
policy to the rest. The agent is called only with `--agent`, so a burst of saves never
turns into a burst of LLM calls; without it `orphans: agent` behaves like
`smates run --no-agent`.

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

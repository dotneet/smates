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

    T -> { deps: {key: hash}, status: pass|fail|skip|unknown, duration_ms, commit }

A test is **skipped** iff it last passed and every input it depends on hashes the same
today. Otherwise it is selected, with the reason. Two suite settings select tests that
would be skipped (`runAnyway`): `volatile` globs (matched like a rule's `tests`; compile
checks excluded) run every time, for inputs smates cannot track such as the date
(reason `volatile`), and `max_age` selects a unit whose entry is older than it (reason
`expired`; a unit with no entry is new anyway). This is the rule of Go's test cache and
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
comparing), so editing them selects nothing. A suite's `volatile`, `max_age` and
`requires` are left out of the hash too (they choose when tests run, not how), and so is an empty
rule `reaches`, so an upgrade does not change the hash. Diff mode (no recorded state) still
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
ignores it: a tracked file keeps the index's mode, an untracked one is not executable),
so a `chmod -x` on a script a test runs selects the test, committed or not. git status
and git diff report a mode-only change as a modification, so the snapshot, diff mode
and watch mode see it too. A symlink hashes as its target path (as git stores it), a
submodule as `commit:` and its pinned commit.

Compatibility: an older smates recorded executable files by their blob alone, and may
still be writing the state (CI on an older version, a shared or cached state). Entries
carry no version of their own, so a recorded hash without the marker matches the
current hash with it (`gitx.SameHash`, used for the snapshot's changed files and the
`--suite` markers, `Hasher.Unchanged` for dependency keys, the flaky comparison and
failure hints): a `dir/**` tree or an `@set:` declaration recorded that way matches
while the same sum over its files' blobs without the marker does. Otherwise every
executable file (`gradlew`, `scripts/*.sh`, `.husky/*`) would be a change, an orphan or
a changed tree after each such recording. The price: a file that only gains the
executable bit, with its content unchanged, is no change, against any state; losing
the bit is one against an entry that recorded it (against an older smates' entry it
goes unnoticed until the test is recorded again). A test that failed because the bit was missing is selected anyway
(its last run failed). The orphan agent's diff (`git_diff`) reads the recorded blob
without the marker and names a change of the bit alone ("mode changed (+x removed),
no content change").

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

A rule's deps are expanded once per selection (`newSuiteRules`) and added to `Known` as
inputs. `tests:` narrows a rule by path; `reaches:` by what the unit reaches, ANDed with
`tests:` when both are set (`suiteRule.applies`): the unit's file-level static closure
(before `MapStatic` keys it by declaration, so a Go function unit matches by its
package's test files and imports), or a file the runner loads for every test through
its configuration (`runner.ConfigOf`: setup files and their closure), holds a file a
glob matches (memoized per suite, as a file is in many closures; a glob without a
wildcard naming a directory also matches `dir/**`), or, for a `$NAME` pattern, the
unit's recorded entry holds an environment key it matches or a file of its closure
names such a variable (a word of its source: Go's test log starts in `m.Run`, so reads
in `TestMain`, `init` and package initializers, and `os.Environ` scans, never reach the
entry). Files are never matched against the entry: it records
the rule's own deps, so a rule reaching them would keep applying to every unit it ever
applied to. A rule cannot only add here: once one unit has the deps, a change to them
is covered and no longer an orphan, so a unit the rule misses is skipped on such a
change. It therefore applies whenever it cannot tell: static analysis is off, no
analyzer reads the unit's files (`static.Graph.Analyzes`), a file of the closure has an
import `static.Graph.Unresolved` reports (an alias only a bundler knows), or, for a
`$NAME` pattern, the unit has no entry yet (it then records the deps once and drops them
on its next recording) or the pattern may match a variable `env_ignore` or the default
list drops before recording (`envBlind`). `smates doctor` counts the units each rule
with `reaches:` applies to (`selector.RuleReach`), warns about a rule that applies to
none, and about each glob that matches no file (`selector.UnmatchedReaches`).

Dependency keys:

| key | hash | recorded when |
|---|---|---|
| `path` | blob SHA-1, `+x` appended for an executable file (`""` = absent) | a file is executed or read (also a symlink the path went through) |
| `dir/` | hash of the entry names (of a directory git ignores entirely: from disk) | a directory is listed (readdir, os.listdir, Bun.Glob) |
| `path?` | `exists` / `absent` | existence is checked (exists/stat/access, Go `stat`) — also for files that do not exist yet |
| `dir/**` | hash of every non-ignored file below (of a directory git ignores entirely: paths and sizes on disk) | a command smates cannot see into ran in `dir` |
| `$NAME` | `env:` + truncated SHA-256 of the value (`""` = unset) | test code read the environment variable (see below) |
| `f.go#decls` | `decls:` + SHA-1 of the file's tokens without function bodies, comments other than directives left out (of a test file: its imports, types, method signatures and what runs for every test; the blob when it does not parse) | Go test functions: every Go file of the static closure (see "Go test functions") |
| `f.go#decl:N` | `decl:` + SHA-1 of the tokens of top-level declaration `N` (function `N`, method `T.N`, a variable, constant or type with its whole group), with the imports of its file, comments other than directives and example output left out (`""` = absent; the blob when the file does not parse) | Go test functions: a function body it executed, a test-file declaration it reaches |
| `f.go#sig:N` | `sig:` + SHA-1 of the same declaration without function bodies, with the file's imports | Go test functions: a declaration of its own package it names (from its code, the test-file declarations it reaches, the bodies it executed) |
| `dir/#shape` | `shape:` + SHA-1 over the Go files of the package directory that contribute what runs for every test (`init`/`TestMain`, initializers that call code, blank imports, shadowed predeclared names, with their build constraints; a file that does not parse or a cgo file whole) and the non-Go files compiled into the package (`golang.PackageParts`) | Go test functions: every function of the package |
| `dir/#methods:T` | `methods:` + SHA-1 of the sorted method signatures of type `T` across the package's Go files | Go test functions: a type of its package it reaches |
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
fixtures, `build/`) is listed from disk too, without vendored entries, as the run left
it like content (a test that adds a timestamped report to the directory it lists settles
after one run). The `ignore` globs do not apply to such a listing: listing the directory
is the read, so a markdown fixture (`**/*.md` is ignored by default) added to a
git-ignored spec directory that a test iterates selects the test. The listing of a directory the
snapshot has files below is the snapshot's alone: entries git ignores inside a tracked
directory are not part of it. That is a known gap (a git-ignored file added next to
tracked ones selects no test that lists the directory, unless the test read it), kept
on purpose: with `granularity: func` every Go test function depends on its package's
listing, so a log a test writes into the package, a `go test -c` binary, `cover.out`,
profiles, `.DS_Store` or an editor's swap file would rerun the whole package (on every
fresh checkout, for files a test writes there), where only the test reading the file
should run. A `dir/**` tree of a directory git ignores entirely is the relative paths
and sizes of its files on disk, without vendored ones (the `ignore` globs do not apply
here either: the command ran there); contents are not read (build output can be gigabytes, and a build that is not
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
handles are recorded the same way. A suite with no recorded entry compares nothing with
a marker: in state mode its tests are decided against the base revision by static
analysis (locally, or in CI with `--base`) or run as new. Nor does one whose entries all
come from selective runs while no full run took a snapshot (`State.Commit` unset) and
no marker names it: when changes are judged against a base revision it counts as never
recorded too (its entries are decided by their own dependencies, which markers do not
touch), so its first ordinary run does not bring the flood back; without a base its
markers stay changes, and orphans, as before. So a marker every recorded
suite of the run saw (and some suite saw) is no change for that run either
(`selection.markerFilter`); otherwise one `--all --suite` recording would make every
file of the tree an orphan of the next unscoped run. The files dropped this way that
changed since the base revision, that no test explains and that a never-recorded suite
owns are its orphans, as in diff mode (`neverRecordedOrphans`), and a note names those
suites. In diff mode (no suite has an entry) the snapshot's changes decide every test,
so markers keep their meaning there.

Orphans go through, in order:

1. **Name references**: tests whose own dependencies mention the orphan's file name
   (fixtures, scripts, schemas referenced as strings) are selected. Files every test of a
   suite depends on (package.json, lockfiles, runner config) only count when no
   test-specific file mentions the orphan.
   The names of all orphans are searched in one pass over each file (Aho-Corasick), so
   the cost does not grow with their number.
2. **Untested code** (`untested: skip`, the default): an orphan in a language a suite
   analyzes statically (Go, Python, JS/TS) under that suite's root is imported by no
   test, directly or transitively (every test's import closure is in its `D(T)`), and
   was neither read nor executed by any test in its last run. No test can be affected,
   so it selects nothing and is reported as a change to untested code. Code configuring
   tools (`*.config.*`, `*rc.*`, `setup.py`, setup files, `.d.ts`), what a Vitest or
   Jest configuration loads by name (`runner.RunnerConfig`: every repository file a
   string literal in a configuration file names, such as setupFiles, plugins and alias
   targets, plus the static closure of both), source under data directories
   (`testdata/`, `fixtures/`, `__mocks__/`, ...) and files a suite excludes are left
   out, and so are files under the root of a suite with a test that has no recorded
   entry yet or whose configuration may run tests in a browser (never traced); so is everything without a recorded state, where `D(T)` holds only the
   import closure and files tests read as data are not known yet. Code run by another
   process must be declared (rules), as for any dependency tracing
   cannot see.
3. **The agent** (Claude Haiku by default) explores the repository with read-only tools and
   adds tests or requests a full run. It can never remove deterministic selections, so a
   wrong or manipulated answer can at worst fail to add tests for the orphan. A sharded
   run (`--shard I/N`, `selectOptions.sharded`) only reuses a cached decision: shards
   asking on their own can get different answers, and a test only one shard's answer
   picked but another shard owns would run nowhere while every shard marks the orphan
   handled. Without a cached decision (one an unsharded `smates select` on the change
   saved into the state the shards restore) it goes on to step 4, with a note.
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
not ask again (and the agent's answers are cached per change). A run that runs nothing
(`--dry-run`, or the Claude Code Stop hook when nothing is selected) still saves the
agent's new answer, as `smates select` does (`cycle.saveWithoutRun`); it records no
outcome. Only the Stop hook's empty run also marks the orphans it handled, and only when
the orphan policy itself left nothing to run: not for a dry run (what it selected did
not run), a shard that owns none of the tests selected for them, or an `--orphans`
override (`--orphans=skip` selects nothing for them, and a later run under the
configured policy must still see them).

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
| bun | test file | `bun test --coverage` lcov + `require.cache` per process, read by a global `afterAll` registered from the preload (bun test emits no exit event; lcov leaves out JSON loaded at run time) (no lcov: bun covered nothing, or the file failed; only a passing file whose static closure has source files is then not recorded) | preload fs + `Bun.file`/`Bun.Glob` hooks (a glob lists the literal directory before its first glob segment when only the last segment is one, else may read everything below it); the test file's `__snapshots__/<file>.snap` when it exists after the run, else its existence (bun reads snapshots natively, past the hooks) | one process per file while recording; shim prepended to bunfig preloads (top-level `test.*` dotted keys and `test = {...}` move into the `[test]` table); bun children preload it through `BUN_OPTIONS` (bun ignores `NODE_OPTIONS --require`) |
| pytest | test file | `sys.monitoring` PY_START (3.12+) / `sys.setprofile` | audit hook `open`, `os.listdir`/`scandir`, wrapped `os.stat/lstat/access` | contexts per collected module, test and fixture; module-level reads via `ReadsBy`; results keyed by each item's file (node ids of files outside the rootdir are relative to the arguments); a file some of whose collected tests did not run (`-x`, `--maxfail`, `pytest.exit()`, an interrupt; with pytest-xdist counted by the controller) is `unknown` |
| unittest | test module | as pytest | as pytest | selected modules passed by dotted name (relative to the discovery top-level dir) and run in one process by `python -m smates_unittest` (unittest's main, patched loader/suite and a result class): contexts per imported module and per module's tests including `setUpModule`/`setUpClass` and their teardowns; per-module status from the result class (a module that ran no test passes when it is a helper, and is `unknown` when it looks written for pytest or the run stopped early, `--failfast` or an interrupt, as is the module running then); `isolate: true` = one process per module |
| go test | package | static import closure (exact for Go) incl. packages named in test string literals | `-test.testlogfile` via `GOFLAGS=-exec=smates`, and of re-executions of the binary; other children as opaque trees (see below) | one binary per package; build failures parsed from output |
| go test, `granularity: func` | top-level test function, plus a compile check per package | coverage profile per function (`go test -c -cover -coverpkg=<linked repository packages>`, re-executions of the binary included) + declarations of the static closure; the check: content of the static closure | `-test.testlogfile` of the function's process and of re-executions of the binary; other children as for a package | one process per function (`-test.run '^Name$'`); checks in one `go test -run '^$'` with the binaries not run |

Test files are what the runner itself would collect: without `tests:` in `smates.yml`,
the adapter reads its runner's configuration statically (jest `testMatch`/`testRegex`/
`roots`/`testPathIgnorePatterns`, vitest `include`/`exclude`/`dir` and projects united
into one suite, pytest `testpaths`/`python_files`/`norecursedirs`, Go modules nested in
the suite's, which belong to it only through the go.work workspace) into matchers relative to the
config's base directory, and falls back to its default globs, with a `doctor` warning,
when the config is not literal. Derived patterns do not mark the suite narrowed: a full
run still lets the runner discover its tests, so an approximation (a converted
extglob, a JS regex run by Go) can never make a full run skip a test the runner has.
A vitest suite command's `--project` drops only the projects known to be named
otherwise (a literal `test.name`, not a browser project, no pattern in the filter).

**Browser projects.** A vitest project with `test.browser` (or a configuration
`mentionsBrowser` when it cannot be read) or Storybook's Vitest plugin is a browser
project (`Matcher.Browser`). Storybook's stories become units: `storybookMatchers` finds
`storybookTest(...)` calls in the source (also inside `mergeConfig`, which
`ConfigObject` cannot evaluate), evaluates `configDir` (`js.CallArgs`,
`js.PathValue`), reads `stories` and `staticDirs` from `<configDir>/main.*` and converts
each entry to a matcher (MDX excluded). Unreadable stories fall back to
`**/*.stories.*` under the project, marked `Whole`: `Result.WholeSuites` then selects
the whole suite whenever one of them is selected, so the runner, unfiltered, runs every
story whatever the guess. Browser projects give their units `Deps` (the configuration
directory and static directories, as globs) and `Starts` (`preview.*`, whose static
closure is added), and a unit that runs only in browser projects also depends on what
the suite's configuration loads (`RunnerConfig.Files`). Such a unit leaves no trace:
`observeAll` makes its reported pass `unknown` (re-run every time) unless the suite sets
`trust_browser_passes`, which records it as a `StaticOnly` pass with `Known` (plus the
runner's global code) as its dependencies; `Record` keeps it `unknown` when
`Suite.Untrusted` names a reason: a matcher's `Unsure` (stories or `staticDirs` not
read), no static analysis, or a known dependency with an import `Graph.Unresolved`
reports (no repository file, no installed package, no builtin: a Vite alias, a virtual
module).

A Go package unit is the directory of a test file, unless build constraints exclude every
Go file of that directory in the suite's configuration (`go list -find` with the suite's
build flags, as for test functions; when go list fails, when the command is not `go test`
itself or passes words after `--` (a wrapper's flags are unseen), or when any Go file of
the directory may compile, it stays a unit): `go test` would fail such a directory
("build constraints exclude all Go files") and `./...` skips it. Widening `tests` to
non-test files (`**/cmd/**/*.go`) makes packages without tests units that `go test`
compiles; their `[no test files]` line (or a package-level `-json` skip event) is
recorded as a pass whose dependencies are the known ones, as for a compile check, since
no test binary runs to leave a trace. When the directory has `*_test.go` files, the
line means the configuration left them out, and it is recorded as a skip instead: the
environment's `GOFLAGS`, `GOOS`, `GOARCH` and `CGO_ENABLED`, `go env -w`, the toolchain
and the platform are not dependencies, so a pass would hide those tests from a
configuration that compiles them. A unit kept because a wrapper's flags are unseen,
which go test then fails with "build constraints exclude all Go files" (read from its
stderr), is recorded as a skip too, and the exit code 1 is forgiven when every other
package of the invocation is reported as passing or without test files.

Processes are linked to their test even through untraced intermediaries: every traced
process records `pid`/`ppid` and `SMATES_TRACE_PARENT` (inherited through the environment,
so `sh -c "node x | tr"` or `make` in between does not break the chain). A process
without a test of its own belongs to its nearest traced ancestor that has one; processes
under a Go test binary belong to that package. A process started inside a test of a
process with contexts (`SMATES_TRACE_CTX`) is that test's helper even when it runs test
files itself: a pytest session a test runs in a subprocess (which inherits the context
and then records none of its own) belongs to the test, even through an untraced chain
of parents. smates itself clears the inherited context for the runners it starts, so a
`smates run` from inside a traced test records a run of its own. A forked child records the
context of its parent only when the parent exported it itself: one only inherited (the
multiprocessing forkserver, started during the first test using it, forks the workers
of later tests too) leaves the child to the context its nearest traced ancestor with
contexts is in when it starts. Helpers of the runner itself that execute
repository code (a server from global setup) are global dependencies; helpers that only
read files (crawlers, transformers) are ignored. A process smates started that names a
test file on its command line is the runner even when that test left no trace (a jest
worker force-killed before writing its coverage, with one runner per test file): only
its code is global, not its reads (jest's watchman probe and `find` crawl stand for the
whole tree). A setup script the suite command runs first (`sh -c "node setup.js &&
jest"`) names no test: what it uses is global.

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

**Reading V8 coverage.** A recorded Node run leaves one coverage file per process dump,
mostly function range data. `trace.Collect` scans them in parallel (GOMAXPROCS workers, at
most twice that many files scanned ahead of the in-order merge, so the output is the same
whatever the scheduling) with a streaming scanner (`v8Scanner`, bounded buffer, no
decoding of ranges) that validates the document as `json.Valid` does and collects the
`"url"` string values the earlier whole-file byte scan found (fuzzed against it). A file
that is not one complete JSON value (cut short by a killed process) is skipped, as
before. The number and timing of dumps are not touched: fewer dumps lose data (vitest
kills its workers shortly after asking them to stop).
Python: smates takes a `sys.monitoring` tool id of its own (3, 4, 2, 5; coverage.py's
sysmon core takes 1 and moves up), or chains to an existing `sys.setprofile` function;
the pytest plugin switches to a `session:end` context (attributed to nothing) once the
test loop is over, before pytest-cov reads every `--cov` module for its report. Go:
`-coverpkg` builds the named packages into every test binary of the invocation, so their
source files (`go list -find`) are added to every package's dependencies.

**Subprocesses smates cannot trace** (shell scripts, grep, make, compiled binaries, and
Node, Bun or Python children that do not load the shims: their environment lacks the
runtime's shim, as the exec event's `rt` says, or Python runs with `-I`, `-E` or `-S`)
are analyzed from their command line when the child starts (`child_process.*` in Node,
`subprocess.Popen`/`os.system` audit events in Python). Every recording runner exports
the tracing environment of all three runtimes, so a pytest test's `node` child traces
itself like a Node test's `python3` child:

- read-only commands (`cat`, `grep`, `jq`, ...) depend on the paths and shell globs they name
  (and the files of `sed` `r` commands; `diff` of directories on their trees; `find -exec`
  is analyzed as the command it runs);
- shell scripts are read and analyzed the same way, following `cd`, `pushd`/`popd`,
  `source`, variables, for-loops, brace expansion, `case`, input redirections, the bodies
  of the functions they call (where they are called, once per state; a function no call
  reaches, such as a trap handler, where it is defined), `$(dirname "$0")`, the commands
  of command substitutions (`$(<file)` reads the file) and the script's positional
  parameters (`$1`, `"$@"`; a function's body sees its call's arguments; not followed in
  a script or function that uses `shift`, `getopts` or `set --`); an unresolved input
  redirection (`< "$X"`) is opaque;
- anything else may read anything below its working directory: the test depends on
  `cwd/**` (commands running outside the repository only on the paths they name), and on
  the program itself when it is named by path (`../bin/tool`) and the paths in its option
  values and response files (`--config=cfg/app.ini`, `-Dk=v`, `@args.txt`); so does a
  command line the shims cut short (beyond 256 Ki characters).

Go has no hook on process creation, but a test log shows every program a test starts:
`os.StartProcess` logs it as an `open` of the program, followed by a `stat` of `cmd.Dir`
when the command has one (and no `SysProcAttr`). An `open` of an executable file
(`trace.GoTestLogs`; on Windows `.exe`, `.com`, `.bat`, `.cmd`) is taken for a start: one
opened only to be read counts too (more dependencies, never fewer), and a program removed
before the test ends (built into `t.TempDir()`) is not seen. The command line is not in
the log, so such a child is analyzed as the last case above without the paths it names:

- Node, Bun and Python (by name, or a `#!` script running on them) trace themselves;
- a re-execution of the test binary run as a test binary (the helper-process pattern)
  runs the package's own test code and keeps a test log of its own, read like the
  parent's (see "Test logs of re-executions" below). When it is cut short, what that
  child read is unknown, and the test depends on the tree of its Go module (the
  repository's when the module encloses it, through symlinks too): the test code may
  read anything there, `../../testdata` included, as the parent's own log would show;
- anything else (`sh -c`, `git`, a compiled tool, the binary run with arguments of its
  own) depends on the tree of the test's working directory (the package directory, or
  where a `chdir` took it) and of the `stat`ed `cmd.Dir` when one follows (a `stat` the
  test makes itself right after starting a program looks the same; both trees are kept),
  and on the program when it is in the repository. Only the line right after the `open`
  is read: in a parallel test another goroutine's line can come between, and a `cmd.Dir`
  elsewhere is then missed, as it is with `SysProcAttr` set. Paths on its command line
  outside those trees (`sh -c "cat ../../testdata/x"`) are not seen: declare them
  (`depends_on`, rules).

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
main-module packages it links (`go list -deps -test`), lists its tests from the test
files `go list` says it compiles (`TestGoFiles`, `XTestGoFiles`; examples without an
output comment are compiled, never run, by go/doc's rule as go test applies it; only
when that fails does it ask the binary, `-test.list`, which runs `TestMain`), and runs
each selected function in its own process (`-test.run '^Name$'`, a coverage
profile, a test log, `GOCOVERDIR` and the Node/Python tracing environment in a trace
directory of its own), bounded by `record_jobs` per package (default 1, like `go test`)
and by `-p` packages at once (command or `GOFLAGS`, default GOMAXPROCS, like `go test`). A
function declared only in a test file the suite's configuration does not compile (build
constraints: a `//go:build linux` file on darwin, a tag neither `GOFLAGS` nor the command
passes; `go list -find` over the packages of the test files that may be constrained by
name or a build line, cached by file stat) is not a unit there: go test would run nothing
for it, and a pass would hide it from a configuration that compiles it. A selected
function go test does not run (an example without output) passes; one go list says is
not compiled (build flags passed to this run only) is not recorded.
Test flags of `GOFLAGS` and of the command are translated to `-test.*` flags of the
binary, `GOFLAGS` first so the command's win. Its dependencies are the functions and
methods whose body holds an executed block (`f.go#decl:N`, found by mapping the block's
start to the declarations of the file as it is on disk; a block outside any body, in a
function literal initializing a package-level variable, is part of that variable's
declaration, or of `f.go#decls` in another package; a block in a file that does not
parse or has `//line` directives makes the whole file a dependency, and one placed by
`//line` in a file that is not there every Go file of the package); for the other
packages of the static closure, the declarations (`f.go#decls`) of every Go file, which
is what makes this sound there: coverage only sees statements inside function bodies,
so constants, variable initializers, types, signatures and method sets are hashed
separately (as tokens, so formatting does not count) and a change to them selects every
function that could see them, while an edit inside a body selects only the functions
that executed it; the content of Go files of packages the binary does not link (a test
that runs `go run ../cmd/tool`); and, in its own package (`funcDeps`), what it reaches
by name, starting from its own declaration, from what runs for every test of the binary
(`TestMain`, `init`, package-level variables whose initializer calls something other
than a conversion, a pure builtin, `errors.New`, `fmt.Errorf`, `time.FixedZone` or
`regexp.MustCompile` of a literal that compiles (no side effect, no panic), declarations
shadowing a predeclared name, methods
the test files add to a type they do not declare) and from the bodies it executed, and
following the identifiers each declaration mentions that the parser leaves unresolved or
resolves to a top-level declaration (not locals, parameters, fields, nor the name a
selector selects), within the package clause (an external test package also follows the
names it uses unqualified, as with a dot import, and those it selects on an identifier
it does not declare, `p.Name`, into the package): test-file declarations count whole
(`f.go#decl:N`, all their names followed: coverage does not instrument test files), the
others without their bodies (`f.go#sig:N`, only the names outside bodies followed,
except for bodies it executed); every type it reaches brings its method set
(`dir/#methods:T`) and the methods test files declare for it; and the package's shape
(`dir/#shape`), so that adding what runs for every test selects every function while
adding a function, type, constant, test or data file nothing names selects none (the
package listing is no dependency: a test that lists the directory records it); and
everything the package unit has besides (test log reads and
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

*Re-executions of the test binary.* The helper-process pattern runs part of a test in a
child of the same binary (`exec.Command(os.Args[0], "-test.run=^TestX$")` with a
variable telling the child what to do). A test binary writes its counters only where
`-test.gocoverdir` says (otherwise to a temporary directory it removes; `GOCOVERDIR` is
used only by the exit hook of a binary whose tests did not finish, an `os.Exit` in a
test), and the child's arguments are the test's, so recording adds one test file to the
package through `-overlay` (merged with a user `-overlay` of the command or
`GOFLAGS`, which ours on the command line replaces; never written into the package): its `init` calls `testing.Init` and sets the flag `test.gocoverdir` to
`$SMATES_GOCOVERDIR` (`MainStart` calling `Init` again keeps it; `os.Args` is left
alone, since a re-execution may run the program's own command line, a `TestMain`
dispatching to a CLI that rejects unknown flags) and leaves a start marker named after
the process ID there. The function runs
with that variable set to its coverage directory, so its own process and every
re-execution inheriting its environment write counters into the same directory, from
which go builds the function's profile when its tests end. A marker without a counter
file of its process (a child that panicked or was killed: no exit hook runs) makes the
function's coverage uncertain, as a missing profile does: every Go file of its closure
becomes a content dependency (otherwise a failing run would record less than the code
the child ran, and the run fixing it would look like a flake). A child given an
environment of its own (`cmd.Env` without `os.Environ()`) or started from a copy of the
binary is not seen; neither is a child of a binary built without coverage.

*Test logs of re-executions* (either granularity). A test log is per process too, and a
re-execution's arguments lack `-test.testlogfile`. The same test file (which package
recording adds to every package with test files it runs, through `GOFLAGS`, named by
`go/build` with the suite's `GOOS`/`GOARCH`/`CGO_ENABLED` and `-tags`) acts in every
process of the binary whose command line has no `-test.testlogfile` (the process smates
started has one). A process run as a test binary (no arguments, or a `-test.` flag
first: the helper-process pattern) gets `test.testlogfile` set to
`$SMATES_GOTESTLOGDIR/<pid>.testlog` and writes its working directory to `<pid>.test`;
the test depends on what that log records, resolved against that directory, and on what
the programs the child started may read (see "Subprocesses smates cannot trace").
testing writes the log when the tests end, 4096 bytes at a time: a child that exits
earlier (`os.Exit`, as the classic `TestHelperProcess` does; a panic, a kill, a
`TestMain` that never calls `m.Run`) leaves it empty or cut at a multiple of 4096 bytes
(a complete log of exactly such a size counts as cut), and a cut log makes the test
depend on its Go module's tree. A process run with arguments of its own (a `TestMain`
dispatching to the program's command line; the binary used as an `-exec` wrapper by a
`go test` the test runs) writes only its directory, to `<pid>.cmd`, and counts as any
other program: the tree of that directory. So do opens of the binary that no marker
accounts for (a test reading its own binary with `debug/buildinfo`; a child given an
environment of its own, a `-test.testlogfile` of its own, or of a package without the
file: one `go/build` cannot name, a user `-overlay` on the command line replacing ours)
and a child started from a copy of the binary.

smates sets `PWD` to the directory of every command it runs (`exec.Cmd` does so only
for an inherited environment): the go command trusts `$PWD` when it names its
directory, and the user's (through a symlink, `/tmp` on macOS) would make the absolute
paths of the overlays, smates' and the user's, made against the resolved repository
root, match nothing.

*Compile checks.* A function's dependencies are the bodies it executed and the
declarations around them, which is what its outcome depends on once the package builds.
But go test fails a package, and every package importing it, when any file of the
package or of its test files does not type-check, misses an import or fails go test's
vet checks — whichever bodies its tests run: an edit inside a body no test executes
(an unused helper now returning the wrong type) would select nothing while `go test`
reports `[build failed]`. Every package with function units (any compiled test file)
therefore also has the unit `dir#(build)` (`runner.BuildCheck`; not an identifier, so no
test function has the name; listed after the functions). Its test files are all the
package's test files, and `MapStatic` leaves its static closure as content: it depends
on every Go and test file of the package, the Go files of the in-repository packages it
imports (transitively), embedded files and `go.mod`, as a package unit does — but not on
depends_on/serves declarations, rules or files read at run time (`knownDeps`: they
cannot change whether it compiles), and the reference scan never pulls it in. Running
the selected checks is one `go test -run '^$' -count=1 -exec=<smates> <packages>`
(chunked like other argument lists) with `SMATES_GOCHECK=1`, in which smates exits at
once: go builds, links and vets each test binary as go test does, and never runs it, so
`TestMain`, `init` functions and package variables of the tests do not run. It takes the
build flags of the suite command and the arguments passed through and the `-vet`
setting, and `GOFLAGS` without its test flags (`-run`, `-json`, `-exec`, profiles), so it
builds what go test builds. Without a usable path to smates (not known, or quoted) the
binaries run with `-run '^$'` instead. A package go test reports as `[build failed]` or
`[setup failed]` fails its check, with the compiler's output (a package that fails only
because one it imports does not build gets the invocation's output); when go test
fails before reporting any package (a go.mod that does not parse), every check fails.
Checks are recorded with their known dependencies, so a passing check is skipped until
a file of its closure changes, and a failed one runs until it passes. With static
analysis off (`static: false`) a check knows only its test files and the suite's
inputs, as a package unit then does.

The check carries the package's failure; the function units do not take it over. Their
recorded outcomes still hold for their dependencies (a broken body none of them ran is
in none of their deps), so after a fix only the checks run again, and a body edit that
breaks the build selects the cheap checks, not every function of the package and its
importers. Functions of a non-compiling package that are selected anyway fail as before
(the coverage build fails, the package is recorded as a whole and go test reports
`[build failed]`); in shadow mode they are no escapes when their package's check was
selected and failed. The cost is a compile of the test binaries of the changed package and
its importers per change to them (go's build cache keeps it small); package granularity
needs no check, since its unit runs the whole package.

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
on `sys.path` for them). Go imports, embeds, cgo headers found through `-I${SRCDIR}/…`,
package directories or `.go` files named in test string literals (`go run ../../cmd/tool`),
and the files that pin dependency versions: the module's `go.mod`/`go.sum` and the `go.mod`
of its local `replace` targets, and in a `go.work` workspace `go.work`/`go.work.sum` plus
the `go.mod`/`go.sum` of every module it `use`s and the `go.mod` of their local replace
targets (minimal version selection picks one version per dependency across them all, so a
`require` bump in a module a package never imports can change what it links; another
module's `go.sum` counts because the go command verifies checksums against all of them, so
a removed line can fail the build). A `go.work` whose directives do not parse, or that
uses nothing, counts every module below its directory. Every `go.work` at or above the
file counts, not only the nearest: the go command takes the one nearest its working
directory, which may be above a nested one. A `go.work` named by `GOWORK` (suite env or
smates' own; `auto` means the search, as unset) is invisible to static analysis, so the go runner makes its pins (the same
set) suite inputs. These are content keys: any edit selects every test of the workspace.

**Type-only imports.** The JS/TS scanner is token-level and turns every `from "<literal>"`
into an edge; in `.ts`/`.tsx`/`.mts`/`.cts` files it marks the declarations TypeScript
erases (`typeOnlyDecl`): `import type` / `export type … from` always (`import type from
"m"` and `import type, { a } from "m"` import a default named `type`: values), and a
specifier list whose specifiers all carry `type` (`import { type A } from`, following
TypeScript's reading of `type as as B`) unless the file's compiler options keep it as a
side-effect import. Those options (`verbatimModuleSyntax`, `preserveValueImports`,
`importsNotUsedAsValues`) are looked up in every config `dirConfigs` returns for every
config directory at or above the file (sibling `tsconfig.*.json` and references
included), through extends chains (installed configs read from `node_modules`); an
unreadable config, or one extending a config that cannot be found while leaving an option
unset, counts as keeping it. `typeof import("x")` and `import("x").T` cannot be told from
value expressions at token level and stay. The analyzer returns both edge sets
(`host.TypeAnalyzer`), and the graph serves `Closure` and `RuntimeClosure`. Each condenses
its edges into strongly connected components once per process (iterative Tarjan over the
files reached so far; a type-only import can split a component, so the two have their own)
and walks the component DAG, so a closure costs the components it reaches rather than
every file edge (a Go file lists every sibling and every file of the packages it imports).
`Warm` resolves every file the starts reach in parallel first. A suite's closures are
covered as one union (`unitDeps`), and a suite `--suite` leaves out builds no `D(T)`:
it only covers. Selection uses
the runtime closure only for a suite whose runner is known to erase types without checking
them (`runner.TypeEraser`): vitest run directly by the suite command, with no mention of
type checking (typecheck, tsc, tsd, ts-node, the options above, `tsconfigRaw`) in the
command, the arguments passed through or the suite env, nor in a vitest/vite config at or
below the suite root or above it or anything those import, no workspace file, no config
naming other configs by path (`projects:`, `workspace:`, `extends:`) except an aggregator
`vitestAggregator` reads in full (a single literal `test.projects` of existing JS/TS files,
which are scanned in turn, and inline projects with `extends: true`), no run-time import
of a package other than vitest, vite, their official plugins (`@vitest/*`, `@vitejs/*`,
`vite-tsconfig-paths`), `@tailwindcss/vite`, Node's builtins and packages the analyzer
resolves to a source file of the workspace package of that name (`Sources.Resolve`,
`packageSource`; their files are in the closure scanned; a `.d.ts`, a `__mocks__` file or
a tsconfig path to another package's file is not what loads at run time) — a preset, or a plugin whose config hook may return test settings — and no
`*.test-d.*` / `*.spec-d.*` file in the suite; or `bun test` itself. Storybook's Vitest
plugin applies the Vite settings of `<configDir>/main.*` and of the addons and framework
it names as strings, which no import reaches: `storybookKnown` accepts it only from
`@storybook/addon-vitest` 10 on (which leaves react-docgen out), with a configuration
directory it can tell and a `main.*` without `viteFinal` or `typescript` naming only
allowlisted addons and frameworks, and scans that `main.*` as configuration. The rule is deliberately coarse: any
mention keeps the type edges, which costs precision, never correctness (a type-only change
can fail a type-checking run: vitest typecheck, ts-jest, ts-node). jest, node and custom
suites always keep them. The files only the full closure reaches are added to `Covered`
(a change to them is explained, not an orphan) but not to D(T) or `Known`, so recording
does not write them either. A file the runner loads through its configuration
(`runner.RunnerConfig`, a Vite plugin serving a mock API that a test imports only for its
types) stays in the closure: erasing the import does not stop the runner loading it.

## 3. Status and shadow mode

Per-file status comes from reporters (vitest, jest — injected only when the user configures
none), the pytest plugin, process exit codes (node:test, bun, go), or a JUnit file. When only
the run's exit code is known, failures are *inferred*: such units are re-run next time but
not reported as definite misses; so is a process exit code that covers several units (node
`--test-isolation=none`). Units that ran but left no trace in a failing run are
`unknown` (re-run next time), and so are files the vitest reporter saw cut short (`--bail`,
an interrupted run: never started, or tests that were declared to run but did not
finish) and files the pytest plugin saw only some tests of run. Go packages that fail to
build are failures (with `granularity: func`, of the package's compile check
`dir#(build)` and of the functions that ran).

A test that skips itself is not proven to pass. Go test functions recorded one per
process run with `-test.v` (added unless the flags already print every result line:
`-test.v`, or `test2json`, whose framing byte is dropped) and `funcOutcome` reads the
output of a process that exited 0: a top-level `--- SKIP` in any `-count` iteration, or
no `=== RUN` line for the function (a `TestMain` that exits before `m.Run`), records
`skip`; a missing result line `unknown`; only `--- PASS` a pass. Lines are matched
anywhere, since output without a final newline precedes the result line; text a test
prints that looks like a skip can only cause a rerun. go test's own output (plain
`--- SKIP` lines, `-json` test events) maps a top-level skip to `skip` the same way,
fail > skip > pass across iterations; a package-level skip event (no test files) and a
package with no tests to run stay passes. A `skip` entry is selected with reason
`unproven` until a run passes; flake detection ignores it (`decisive`), and the
outcome line counts the run's skips. Package granularity, and other runners, record a
skip as a pass (the package's or the file's summary).

`smates run --shadow` runs everything while evaluating the selection and reports
*escapes*: tests that failed but would have been skipped (and, separately, unselected
tests whose failure was only inferred).

With `--shard I/N` a shadow run runs every unit its shard owns (`cycle.units`), selected
or not, so the shards of one run together run every unit once. `checkShadow` judges only
owned units, against the selection before `applyShard` (`cycle.preShard`): a failure the
runner reports for a unit another shard owns is never an escape here, since its owner
reports it. A Go function failed because its package did not build
(`Observation.BuildFailed`, from go test's build failure lines) while its package's
selected compile check is another shard's is a possible escape: that shard reports the
build failure. A function that ran and failed is an escape.

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
A batch whose runner cannot start (not installed, a failed preflight check) does not end
the run: its units count as not run (`not_run`, they stay selected), later batches run,
and what ran is recorded and reported; smates then exits 1 when tests failed, else 4
(the exit code `--report`, the summary and the GitHub outputs give, `cycle.exitCode`).
For the snapshot its suite counts as one `--suite` left out: a full run refreshes it only
for the suites that ran (markers for files the others also own), and the orphans a
selective run handled are marked seen by those suites alone. A suite command line that
cannot be parsed still ends the run as a configuration error (exit 3).
`--fail-fast` is rejected with `--shadow`, whose point is to run everything.

### `--max-time` and `requires`: units left for later

Both leave selected units unstarted on the same path as above, so neither can record a
pass: the units get no observation, keep their old entry and are selected again.

`--max-time D` needs calls short enough to stop between. Each suite's calls (after the
`--fail-fast` split) are cut into consecutive calls whose recorded durations add up to
about D/4; a unit with no measured duration (no entry, an entry without a pass, fail
or skip, such as an outcome inferred from a failing batch, or one recorded at 0 ms)
counts as the mean of the measured ones, and a suite with none measured stays one call of unknown length. A Go
package adds one second per call that builds it (`goPackageCost`): recorded durations
are the test binaries' run time, without compiling and linking them. Before each call
smates checks that it would end within D (elapsed time plus its estimate plus the mean
per-call overrun of the calls of its suite that ran, or of all calls before the suite's
first: startup and builds are a cost per call, so a ratio taken from a short call would
blow up a long one; a call that requires trimmed is re-estimated from the units it
kept, so its overrun is measured on what ran); an unknown call starts only while less than
D/2 is used. The first call always starts, so repeated capped runs make progress: what
passed is deselected, and the units left out stay selected for a later run, in the
usual order (no boost: new direct changes still come first).
Like `--fail-fast`, the cap never kills a running call, and a stopped run leaves the
snapshot alone. Where the cap stopped the run and everything that ran passed, smates
exits 0: the caller asked for a partial run, and the report (`max_time_reached`,
`not_run`), the summary and the log say what is left. Exit 4 would claim an environment
problem that does not exist. The coding agent Stop hooks default to `--max-time 8m`, the
600 s hook timeout less a margin for selection, recording and estimates that are off.

A suite's `requires` are checked when a call of it is about to start, once per run for
each distinct check (cached by root, check and env). A check that cannot tell (it fails
to start, times out, its address does not parse) counts as unmet: running the tests
anyway would record outcomes of a broken environment. Only the units a requirement's
`tests` globs match (`UnitMatches`, as for rules) are taken out of the call; the call
then passes explicit units. Their suite counts as left out for the snapshot, like a
suite whose runner could not start, so the orphans it would have covered stay unseen
for it. The run exits 4 when no test failed (`requiresError` inside an `envError`); a
runner that could not start or a state that could not be saved is reported first. The
Stop hooks tell `requiresError` apart and print one line instead of an error: in a
worktree without the database, every turn would otherwise end with the same message.
`--dry-run` does not run the checks (a `cmd` may be slow or have side effects).

### Shard counts

`--shard I/N` hashes suite and unit ID (FNV-1a) modulo `N`: ownership needs no
coordination and no state, but it depends on `N`, so every shard of a run must use the
same count. `smates select --max-shards N` sizes `N` for a CI matrix (`shardCount`): `0`
when nothing is selected, else the recorded duration of the selected units over
`--shard-time`, rounded up and kept within `1..N`. A selected unit without a duration
weighs its suite's median; a suite with selected units and no duration at all yields `N`
(nothing to size by). On a full-run branch it sizes for every unit, as the shards'
`smates run` will run them all. For selective legs the count never changes what runs,
only how many jobs share it, so a wrong estimate costs time, not coverage. It is sized
for the selection, so it must not feed `run --shadow` legs, which run every unit their
shard owns: with nothing selected it is `0`, and no shadow leg would run. The `shard-matrix` output carries
the count in each value (`"2/3"`) so legs cannot disagree on it. Balancing shards by
duration (instead of the hash) is not done: it would need a plan file every shard reads,
and a single point of failure where the hash needs none.

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
policy. An external agent CLI can be plugged in instead (`agent.command`). Sharded runs
never ask it and only reuse a cached answer (see "Orphans" in §1).

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

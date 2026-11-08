# How smates decides

smates skips a test only when nothing it depends on has changed since it last passed.
This page explains what "depends on" means, how a change is detected, and what each
selection reason says.

## An example

Take one test unit, `src/cart.test.ts`. When it last ran and passed, smates recorded:

```text
src/cart.test.ts        the test file itself
src/pricing.ts          imported, and executed by the test
src/money.ts            imported by src/pricing.ts
fixtures/prices.json    read by the test at runtime
$TZ                     environment variable the test read
package.json, vitest.config.ts, pnpm-lock.yaml   inputs of the suite
```

Now you change files:

| You change | Result for `src/cart.test.ts` |
|---|---|
| `src/money.ts` | selected: `[changed: src/money.ts]` |
| `fixtures/prices.json` | selected: `[changed: fixtures/prices.json]` |
| `TZ=UTC` to `TZ=Asia/Tokyo` | selected: `[env changed: TZ]` |
| `src/admin/users.ts` (not among its inputs) | skipped |
| nothing, but it failed last time | selected: `[failed: last run: fail]` |

A *test unit* is what smates selects or skips as a whole: a test file, a test module for
`unittest`, a package for Go (or a test function with
[`granularity: func`](../guides/go-test-functions.md)).

## What a test depends on

For every test unit `T` smates keeps a dependency set, `D(T)`, the union of four parts:

| Part | What it contains | Where it comes from |
|---|---|---|
| traced | Files executed or read while `T` ran (including by its child processes), directories it listed, files whose existence it checked, environment variables its code read | The last recorded run |
| static | The import-graph closure of `T` | Computed now, so a new import counts at once |
| inputs | Lockfiles, manifests, runner config, your `inputs:` and `rules:` globs, and the suite's own settings in `smates.yml` | The suite configuration |
| depends_on | Code of a server the tests talk to in another process | Declared in `smates.yml` ([servers](../guides/servers.md)) |

Tracing finds what static analysis cannot: a fixture loaded by a computed path, a config
file read at startup, a script started as a subprocess. The static graph covers what a
recording has not seen yet. Declarations cover what neither can see.

## The skip rule

`T` is **skipped** only if both hold:

1. `T` passed the last time it ran, and
2. every entry of `D(T)` hashes the same now as it did then.

Otherwise `T` is **selected**. A dependency that was not recorded at all (a newly added
import, a new `conftest.py`) counts as changed.

Editing a setting of `smates.yml` that cannot change a test's outcome (`ignore`, `agent`, `orphans`, another
suite) selects nothing.

## Why a test was selected { #why-a-test-was-selected }

`smates run --explain` and `smates select` print the reason next to each selected test:

```text
  web (vitest): 4/380
    + src/cart.test.ts  [changed: src/pricing.ts]
    + src/dates.test.ts  [env changed: TZ]
    + src/report.test.ts  [failed: last run: fail]
    + src/new.test.ts  [new: no recorded run]
```

| Reason | Meaning |
|---|---|
| `changed: <files>` | An input changed since the test last ran. |
| `env changed: <vars>` | An environment variable the test read has a different value. It can follow `changed:` in the same brackets. |
| `failed: last run: <status>` | The test did not pass last time (`fail`, or `unknown` when its result could not be determined). |
| `new: no recorded run` | The state has no entry for the test. |
| `diff: <files>` | No recorded run, and a file the test statically depends on changed since the base branch (see below). |
| `reference: references <file>` | An orphan: the test names a changed file that is not among any test's inputs. |
| `orphan: unattributed change: <file>` | An orphan the policy handled by running the suite in full. |
| `agent: …` | The LLM agent added the test for an orphan. |
| `caller: added by caller` | Added with `smates run --add`. |
| `only: --only` | Named with `smates run --only`. |
| `all` | A full run: `--all`, or the default branch in CI. |

??? note "Details: less common reasons"
    | Reason | Meaning |
    |---|---|
    | `unproven: skipped last run: not proven to pass` | Go only: last time the test function skipped itself (`granularity: func`), or the build configuration left the package's tests out, so its pass is not proven. Any other skip (pytest, vitest, jest, a Go package whose tests all skip) is recorded as a pass. |
    | `volatile: listed in volatile: runs every time` | The suite's [`volatile`](../reference/configuration.md#suite-keys) lists it. |
    | `expired: last run <age> ago, older than max_age <max_age>` | Its last recorded run is older than the suite's [`max_age`](../reference/configuration.md#suite-keys). |
    | `new: no recorded run, and tests read <file> (changed since the base revision)` | No recorded run and nothing it imports changed since the base branch, but recorded tests of its suite read a file that did, and this test may read it too (see below). |
    | `all: runs with <test>, of a project whose test files smates cannot list` | A selected test belongs to a project whose test files are a guess (Storybook stories smates cannot read), so the whole suite runs. |
    | `rerun: rerun` | `smates watch` reran the previous selection. |
    | `failed: rerun of failed tests` | `smates watch` reran the tests that have not passed (the `f` key). |

    The reason codes in `smates select --format json` are listed in
    [JSON output](../reference/json-output.md#selection-reasons).

??? note "Details: `new` versus `diff`"
    In CI a test with no recorded run is selected as `new`. On your machine the state only
    knows the tests that ran there, so a test with no entry is judged by static analysis
    against the base branch instead: it is selected (`diff`) when its test file or a file
    it imports changed since then. A test file you just added is such a change, so it
    still runs. The same applies to every test when there is no state at all, and in CI
    when you pass `--base` (to adopt suites one at a time in one job). It is also
    selected (`new`) when a data file that recorded tests of its suite read changed since
    then, since it may read that file too.

## Content hashes, not git diff

Every input is stored with its content hash (the git blob hash) at the time the test ran,
and compared with the current content. Because nothing is computed from git history:

- selection needs no history: a shallow clone (`fetch-depth: 1`) is fine;
- uncommitted and untracked files count, so it works on a dirty working tree;
- rebases, merges and amended commits do not confuse it.

Two details are part of a file's hash. The **executable bit**: `chmod -x` on a script a
test runs selects the test. **Symlinks**: a file read through a link inside the
repository depends on the link too, so retargeting the link selects the tests that read
through it.

??? note "Details: gaining the executable bit"
    Only *losing* the bit is detected. A state written by an older smates recorded files
    without the bit, and smates keeps accepting such states (also while CI keeps writing
    them with that older version), so a file that only gains the bit (with unchanged
    content) selects nothing. See
    [Limitations](../reference/limitations.md#files-and-file-systems).

## Safety principles

Every uncertainty resolves to running *more* tests:

- **Failed tests always rerun**, until they pass. So do tests whose result is unknown.
- **New tests always run.** A dependency smates has not recorded counts as changed.
- **Opaque commands widen the dependency.** A test that runs a command smates cannot
  see into (`make`, a compiled binary) depends on everything below the directory it ran
  in.
- **Errors fall back to running.** If the agent fails, times out or has no API key, the
  affected suites run in full.
- **Nobody can remove a test.** The LLM agent, and a coding agent passing `--add`, can
  only add tests to what the deterministic selection picked.

## Changes no test explains

A changed file that is in no test's `D(T)` and not ignored (a Dockerfile, a CI script, an
SQL file applied by a service) is an *orphan*. smates handles it in steps:

1. select the tests that mention the file by name;
2. skip changes to source code that no test reaches (*untested code*);
3. ask the LLM agent about the rest, when an API key is set;
4. otherwise run the suites the file can affect in full.

Frequent orphans belong in `ignore:`; `smates orphans` suggests the globs. See
[Changes no test explains](../guides/orphans.md).

## How tests are traced

smates runs your own test command, adding its tracing hooks through environment
variables (`NODE_OPTIONS`, `PYTHONPATH`, `GOFLAGS`) and a few arguments (see
[What smates adds to the command](../guides/runners.md#what-smates-adds-to-the-command))
for that run only. Nothing is installed into your project: apart from `.smates/`, the
only file it writes there is a temporary copy of its vitest reporter, removed after the
run.

| Language | How dependencies are recorded |
|---|---|
| JavaScript / TypeScript (vitest, jest, `node --test`) | V8 coverage per worker or process, plus file system hooks |
| Bun | `bun test --coverage` per file, the module cache, and preload hooks for `node:fs`, `Bun.file`, `Bun.Glob` |
| Python (pytest, unittest) | `sys.monitoring` (3.12+) or `sys.setprofile`, plus audit hooks; per test, fixture and module |
| Go | The static import closure (exact for Go), plus the files and variables the test log records through `go test -exec` |
| Child processes | Node, Bun and Python children trace themselves; shell scripts, `grep`, `cat` and similar are analyzed from their command line; children of Go tests through Go's test log ([details](../guides/runners.md#child-processes)) |

Per-runner details, including coverage tools and package managers, are in
[Languages and runners](../guides/runners.md).

## Likeliest failures first

`smates run` starts the selected tests in this order:

1. tests that did not pass last time;
2. tests whose own file, or a file they import directly, changed;
3. tests reached through an indirect dependency (a transitive import, traced data,
   configuration, a name reference, the agent);
4. new tests and the rest.

Within a group, the shortest recorded duration goes first. Suites run in the order of
their best test, and each runner gets its files in that order. pytest and `go test`
start them in that order; vitest and jest use their own sequencers, which also put
previously failed files first.

With `--fail-fast`, each suite is split into one runner call per group, and the calls
run group by group across suites. After a call fails, no further call starts (the running
one finishes). Tests that did not start are not recorded, so they stay selected for the
next run; the log, the summary and `--report` (under `not_run`) list them. The extra runner start-ups are why this
is not the default. `--fail-fast` cannot be combined with `--shadow`.

## Next steps

- [Configure suites](../guides/configuration.md): adjust what smates detected, and declare
  what tracing cannot see.
- [Changes no test explains](../guides/orphans.md): the orphan steps in detail.
- [Environment variables](../guides/environment.md): which variables are tracked or
  ignored.
- [Shadow mode, flaky tests and stats](../guides/trust.md): measure how far to trust the
  selection.
- [Design](../DESIGN.md): the internals, dependency keys and the soundness argument.

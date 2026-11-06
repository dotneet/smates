# Configure suites

`smates.yml` tells smates which test suites your repository has, how to run them and
what they depend on beyond what tracing sees. This page walks through the settings you
are likely to need; every key is listed in the [smates.yml reference](../reference/configuration.md).

```yaml
version: 1
suites:
  - name: web
    runner: vitest
    root: packages/web
  - name: api
    runner: go
    root: services/api
ignore: ["design/**"]
```

A *suite* is one test runner invocation rooted at a directory. A *test unit* is what
smates selects or skips inside it: a test file, or a Go package (or test function).

## When you need smates.yml

You may not. Without a config file, smates detects suites from the repository layout
(package.json scripts and runner configs, `go.mod`, pytest or unittest files) and uses
each runner's defaults. Check what it found:

```bash
smates list
```

```text
web	runner=vitest	root=packages/web	units=42	command=./node_modules/.bin/vitest run	tests=vitest.config.ts test.include
api	runner=go	root=services/api	units=9	command=go test	tests=runner defaults
```

Write a `smates.yml` when detection misses or merges something, when the runner needs a
specific command, or when tests depend on things tracing cannot see. `smates init`
writes one from what detection found, which is the easiest starting point. smates looks
for `smates.yml`, `smates.yaml`, `.smates.yml` or `.smates.yaml` at the repository root.

## Define a suite

Each entry under `suites:` needs a `runner`; the rest is optional.

| key | meaning | default |
|---|---|---|
| `runner` | `vitest`, `jest`, `node`, `bun`, `pytest`, `unittest`, `go` or `custom` | required |
| `root` | directory the command runs in, relative to the repository root | `.` |
| `name` | used in output, `--suite`, `--only suite:unit` and `depends_on` | the root, or the runner name for a root of `.` |

Names must be unique. What each runner traces and how it reports results is on
[Languages and runners](runners.md).

## Choose the test files

By default smates reads the test files from the runner's own configuration, so it
counts the same files the runner would run. Override it with `tests` and `exclude`,
both globs relative to the suite root:

```yaml
  - name: web
    runner: vitest
    root: packages/web
    tests: ["src/**/*.test.ts"]
    exclude: ["src/legacy/**"]
```

!!! note
    The `custom` runner has no defaults: a `custom` suite needs `tests:`, or it has no
    test units.

### Which files are tests

Without `tests:`, smates reads the runner configuration statically. Config files are
never executed.

| runner | read from | options |
|---|---|---|
| `jest` | `--config` of the command, else the nearest `jest.config.{js,ts,mjs,cjs,mts,cts,json}` or `package.json` `"jest"` from the suite root up (jest stops at the nearest `package.json`) | `rootDir`, `roots`, `testMatch` (micromatch extglobs such as `?(x)` and `+(spec\|test)` are converted; `!` patterns exclude), `testRegex`, `testPathIgnorePatterns` (`<rootDir>` substituted) |
| `vitest` | `--config` of the command, else the nearest `vitest.config.*` / `vite.config.*` from the suite root up | `root`, `test.root`, `test.dir`, `test.include`, `test.exclude` (`...configDefaults.exclude` understood), and projects: `test.projects`, `test.workspace` or `vitest.workspace.*` |
| `pytest` | the first of `pytest.ini`, `.pytest.ini`, `pyproject.toml` (`[tool.pytest.ini_options]` / `[tool.pytest]`), `tox.ini` (`[pytest]`), `setup.cfg` (`[tool:pytest]`) from the suite root up | `python_files`, `norecursedirs`; `testpaths` when that file is in the suite root (pytest uses them only when started in its rootdir) |
| `unittest` | the `-s`/`-p`/`-t` of a `python -m unittest discover …` command | default: `test*.py` below the suite root |
| `go` | `**/*_test.go`, without `testdata/` and `vendor/` | a module nested in the suite's belongs to it only through the `go.work` workspace |
| `node` | node's default patterns: `*.test.*`, `*-test.*`, `*_test.*`, `test-*.*`, `test.*`, files under `test/` | helpers and fixtures under `test/` are excluded |
| `bun` | bun's default patterns: `*.test.*`, `*.spec.*`, `*_test.*`, `*_spec.*` | |

A JS/TS config is understood when it exports an object literal (`module.exports = {...}`,
`export default {...}`, `defineConfig({...})`, or a top-level `const` it names) and the
options are string, array or regex literals.

Anything else falls back to the runner's default globs, and `smates list` and
`smates doctor` warn with the reason. Examples: a function, another call such as
`mergeConfig(base, ...)`, `testMatch: dirs.map(...)`, a `const` changed later
(`config.roots.push(...)`), a `...spread` that may set the option, jest `projects`, or a
`testRegex` with lookaround. Set `tests:` (and `exclude:`) then.

??? note "Details: vitest workspaces, full runs and `smates list -v`"
    - A vitest workspace is one suite whose test files are the union of its projects,
      each project's include/exclude applied below its own root. A project whose config
      cannot be read keeps the defaults for its directory.
    - When a root vitest config does not count a nested package's tests (that package
      has its own `vitest.config.*` and no `projects` entry covers it), auto-detection
      makes the package a separate suite.
    - `smates list` shows where the patterns come from (`tests=jest.config.js testMatch`);
      `-v` adds the patterns. `smates doctor` shows the same.
    - Patterns derived from the runner config describe what the runner discovers by
      itself, so on a full run (`smates run --all`) smates lets the runner discover its
      tests. With `tests:` or `exclude:` in `smates.yml`, smates always passes the files
      explicitly, also on a full run.

## Set the command

Each runner has a default command (`./node_modules/.bin/vitest run`, `go test`,
`uv run pytest`, …). Set `command` when your project runs tests differently:

```yaml
  - name: web
    runner: vitest
    root: packages/web
    command: "pnpm exec vitest run --project unit"
```

smates appends the selected test files to the command. To put them somewhere else,
write `{tests}` where they belong:

```yaml
    command: "pnpm exec vitest run {tests} --reporter=dot"
```

Arguments you pass through `smates run -- <args>` go just before the test files. Such
runs may filter tests, so they never record a test as passed; put permanent flags in
`command`. The `custom` runner requires a `command`.

## Declare what tracing cannot see

Tracing records the files a test executes or reads. Two settings add dependencies it
cannot observe:

```yaml
  - name: api
    runner: pytest
    root: services/api
    inputs: ["/proto/**", "schema.graphql"]
    rules:
      - tests: ["tests/db/**"]
        deps: ["migrations/**"]
```

- `inputs`: globs every test of the suite depends on. They are relative to the suite
  root; a leading `/` makes a glob repository-relative. Lockfiles, manifests and the
  runner's own config are inputs by default.
- `rules`: dependencies for some tests only. `tests` are globs relative to the suite
  root, `deps` are repository-relative globs. `reaches` picks the tests by what they
  import instead of where they live (next section).

A server that tests reach over the network is a different case: declare its code with
`depends_on` or `serves` (see [Tests that talk to a server](servers.md)).

### Tests backed by a database

A database schema is usually applied before the tests run, by a migration tool in a
separate step (`make migrate`, a CI job), and test data may be loaded the same way. No
test process reads those files, so a change to a migration is an
[orphan](orphans.md): the agent decides, or the whole suite runs. Path globs rarely
describe "the tests that use the database": they sit next to tests that do not. What
they share is a helper they import to connect, so name the helper with `reaches`:

```yaml
  - name: go
    runner: go
    rules:
      - reaches: ["pkg/shared/dbtest/**", "$APP_DB_*"]
        deps: ["database/migrations/**", "database/fixtures/**"]
```

A test gets the migrations and fixtures as dependencies when its static import closure
reaches `pkg/shared/dbtest/` (directly, through another helper, or from another test
file of its Go package), or when its last recorded run read a variable matching
`APP_DB_*`. A change to a migration then selects those tests and is no longer an
orphan.

That last part is why the list must be complete. A test that connects some other way
(its own `sql.Open`, a helper the rule does not name, a child process) does not get the
deps, and once the migration is covered it is not run for it either. Name every helper,
and add the variables the connection reads: a test that reads them is kept even when it
imports nothing the globs match. When smates cannot tell, the rule applies to the test:
static analysis is off, no analyzer reads the test's files, or (for a `$NAME`
entry) the test has never run.
`smates doctor` prints how many test units each such rule applies to and warns when a
rule applies to none (a glob that names nothing the tests import).

## Set environment variables

`env` sets variables for the suite's command:

```yaml
    env: { TZ: UTC, APP_MODE: test }
```

The values are part of the suite's settings, so changing one reruns the suite. They
also override the shell when smates compares a variable a test read. See
[Environment variables](environment.md).

## One process per file while recording: `isolate`

Some runners share a worker process between test files, which merges their
dependencies. `isolate: true` runs each file in its own process while recording (runs
that do not record are unchanged):

| runner | when to set `isolate: true` |
|---|---|
| `jest` | for strictly one process per file (smates already recycles workers after each file) |
| `vitest` | when the project sets vitest's `isolate: false` |
| `node` | when the command uses `--test-isolation=none` |
| `unittest` | to run each module in its own process instead of one process for all |

`bun` always runs one process per file while recording. `record_jobs` bounds how many of
these processes run at once (default: 1 for bun and for commands that run files serially
such as `--runInBand`; the CPU count otherwise). For Go suites, `record_jobs` means
something else: see [Go test functions](go-test-functions.md#what-recording-costs).

## Per-file results from a JUnit report: `junit`

The `custom` runner only knows the exit code of its command. When that command runs many
files, a single failure marks all of them failed, so they all run again next time. If
your command writes a JUnit XML report, point `junit` at it (relative to the suite root)
and smates reads per-file results from it:

```yaml
  - name: e2e
    runner: custom
    root: e2e
    tests: ["tests/**/*.spec.ts"]
    command: "npx playwright test --reporter=junit"
    env: { PLAYWRIGHT_JUNIT_OUTPUT_NAME: reports/junit.xml }
    junit: reports/junit.xml
```

Files are taken from the `file` attribute of test cases or suites, else from the suite
name or class name when it names an existing file.

## Several suites in one repository

List one suite per runner invocation. In a monorepo that is usually one per package or
service:

```yaml
version: 1
suites:
  - name: web
    runner: vitest
    root: packages/web
  - name: shared
    runner: jest
    root: packages/shared
  - name: api
    runner: go
    root: services/api
  - name: tools
    runner: pytest
    root: tools
```

The static analyzer follows imports across suites, including npm/pnpm/yarn/bun
workspaces and tsconfig `paths`, so a change in `packages/shared` selects the `web`
tests that import it. Two suites may share a root (for example unit and e2e tests of
one package) when their `tests` globs differ. Run one suite with
`smates run --suite web`.

## Repository-wide settings

These top-level keys apply to all suites:

```yaml
ignore: ["design/**", "deploy/**"]
inputs: [".github/workflows/test.yml"]
full_run_branches: [main, release]
```

- `ignore`: changed files that are never *orphans* (changes no test is known to depend
  on; see [Changes no test explains](orphans.md)). They add to built-in defaults for
  docs, licenses, editor settings and lint or formatter configs. A test that reads an
  ignored file still depends on it.
- `inputs`: repository-relative globs every test of every suite depends on, such as a
  CI workflow or a toolchain pin.
- `full_run_branches`: branches on which `smates run` runs everything on CI (push
  builds, not pull requests; default: the repository's default branch). A full run
  there keeps the recorded state complete. Locally, `smates run` always selects.

Other top-level keys (`orphans`, `untested`, `env_ignore`, `env_track`, `state*`,
`flaky`, `agent`) are covered on their own pages and in the
[reference](../reference/configuration.md).

## Which edits rerun tests

Every test depends on its suite's settings in `smates.yml`. In `--explain` this shows
as `[changed: smates.yml]`. What counts is the suite's own entry as written, plus the
top-level `env_track`, not the whole file:

| you edit | effect |
|---|---|
| a suite's entry (`command`, `env`, `tests`, `isolate`, …) | every test of that suite runs again |
| top-level `env_track` | every test runs again |
| another suite's entry | nothing of this suite |
| `ignore`, `orphans`, `untested`, `agent`, `flaky`, top-level `env_ignore` | nothing runs |
| a suite's `volatile` or `max_age` | only the units they select now: they choose when tests run, not how |
| `inputs` or `rules` | the files they add are new dependencies: tests that gain one run once |

The defaults smates fills in (a detected command, test globs) are not part of the
hash. They depend on the machine, for example on whether a virtualenv exists, and a
state recorded on CI would otherwise rerun every suite on a laptop. A test that reads
`smates.yml` itself depends on the file's content like on any other file.

## Edit smates.yml from the command line

`smates config` changes the file without hand-editing YAML. It keeps comments and
layout and validates the result before writing it:

```bash
smates config add-ignore 'design/**' 'deploy/**'      # append to top-level ignore:
smates config add-input .github/workflows/test.yml     # append to top-level inputs:
smates config set orphans all                          # set a top-level scalar
```

Add `--dry-run` to print the diff without writing:

```text
$ smates config add-ignore 'design/**' --dry-run
would add to ignore: design/**

--- a/smates.yml
+++ b/smates.yml
@@ -4,3 +4,5 @@
     runner: custom
     root: e2e
     command: "npx playwright test"
+ignore:
+  - "design/**"
(dry run: smates.yml not changed)
```

`add-ignore` and `add-input` skip values already present. `set` accepts top-level
scalars: `version`, `orphans`, `untested`, `state`, `state_seed`, `state_shared`,
`flaky.retry` and the `agent.*` keys. Suite settings are edited by hand. `--format json`
gives a machine-readable result.

## See also

- [smates.yml reference](../reference/configuration.md): every key and default
- [Languages and runners](runners.md): what each runner traces
- [Tests that talk to a server](servers.md): `depends_on` and `serves`
- [Changes no test explains](orphans.md): `ignore`, `orphans` and `smates orphans --apply`

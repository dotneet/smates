# Languages and runners

smates works with the test runners you already use: vitest, jest, `node --test` and bun
for JavaScript and TypeScript, pytest and unittest for Python, and `go test`. This page
shows how each one is traced and what you may need to set for it.

## Supported runners

| runner | unit | how dependencies are traced | per-file status |
|---|---|---|---|
| `vitest` | test file | V8 coverage per worker + fs hooks; globalSetup reads count for every test | injected reporter |
| `jest` | test file | V8 coverage + fs hooks per worker, workers recycled after each file (`isolate: true` for strictly one process per file) | injected reporter |
| `node` (`node --test`) | test file | V8 coverage per test process + fs hooks (`isolate: true` for one process per file under `--test-isolation=none`) | process exit code |
| `bun` | test file | per-file `bun test --coverage` + module cache, preload hooks for `node:fs`, `Bun.file`, `Bun.Glob` (while recording) | process exit code |
| `pytest` | test file | `sys.monitoring` (3.12+) / `sys.setprofile`, audit hooks; per test, per fixture, per collected module, plus module-level reads | plugin |
| `unittest` (`python -m unittest`) | test module | same Python tracing; selected modules run in one process through a wrapper with contexts per module | wrapper result class |
| `go` | package, or test function with `granularity: func` | static import closure (exact for Go) + `-test.testlogfile` via `go test -exec`; Node/Python children of tests | binary exit code, build failures |
| `custom` | test file | any Node/Python processes your command starts | overall exit code, or a [JUnit report](configuration.md#per-file-results-from-a-junit-report-junit) |

A *test unit* is what smates selects or skips. Tracing only happens while smates
*records* a run (the default for `smates run`; `--no-record` turns it off).
Which files count as tests is read from each runner's own configuration; see
[Which files are tests](configuration.md#which-files-are-tests).

## JavaScript and TypeScript

smates injects a preload (`--require`) into Node processes. It collects V8 coverage for
the code each test file executes and hooks `fs` for the files it reads, lists and
probes. The static analyzer adds the import graph, so a new import counts at once (see
[What the import graph understands](#what-the-import-graph-understands)).

Code the runner process runs itself, outside any test file, is a dependency of every
test of the suite: its configuration, global setup and what they import, custom
reporters and resolvers. So is everything such a helper uses (its code and the files it
reads) when it runs repository code but belongs to no test, such as a server started by
global setup. Files the runner process only reads are not dependencies, except reads
made in global setup: the runner reads every test and source file to crawl or transform
them.

### vitest

Vitest runs each file in a fresh worker by default, so coverage per worker attributes
exactly. Reads made in `globalSetup` count for every test. If your project sets vitest's
`isolate: false` (shared workers), set `isolate: true` on the suite so that smates runs
each file on its own while recording.

smates adds a reporter next to yours for per-file results. Older Vitest loads reporters
only from inside the project, so smates copies its reporter into
`node_modules/.cache/smates` for the run and removes it afterwards. When you configure
`reporters` yourself, or the command names a configuration (`--config`, `-c`) or a root
(`--root`, `--dir`) whose settings smates cannot see, or there is no `node_modules`
directory to place the reporter in, it does not add one, and a failing run then marks
every recorded file failed. See also
[What smates adds to the command](#what-smates-adds-to-the-command).

Vitest and Vite configuration files in the suite directory (`vitest.config.*`,
`vite.config.*`, per-project files such as `vitest.e2e.config.ts` or
`vite.storybook.config.ts`, and
`vitest.workspace.*`) are inputs of every test. The files a configuration loads by
name, such as `setupFiles`, `globalSetup`, local Vite plugins and alias targets, and
whatever they import, are never treated as untested code: a change to one goes to the
[orphan policy](orphans.md) unless a test depends on it.

### Browser mode and Storybook

A vitest project that runs its tests in a browser (`test.browser`) or with Storybook's
Vitest plugin (`storybookTest()` from `@storybook/addon-vitest`, or the older
`@storybook/experimental-addon-test`) is read like any other,
with two differences.

- **Stories are tests.** The plugin reads the stories of the Storybook configuration
  directory, and so does smates: the `configDir` option of the plugin (a literal, or
  `path.join` / `path.resolve` of `__dirname`, `import.meta.dirname` or `dirname`),
  else `.storybook`. Like Storybook, smates resolves a literal and the default against
  the directory Vitest runs in (the suite root), and the `path.join` forms against the
  file's directory; a project file elsewhere whose directory is resolved that way gets
  stories that are never trusted (see below). It reads the `stories` of its
  `main.*` (strings, directories and `{ directory, files }` specifiers; MDX pages are
  not tests). This works when the Vitest configuration itself cannot be read
  (`mergeConfig(...)`), so a package whose only tests are stories gets a suite. When
  the stories cannot be read, smates takes every `*.stories.*` file under the project
  and, because those are a guess, runs the whole suite whenever one of them is
  selected; `smates list` and `smates doctor` say so.
- **Nothing in the browser is traced.** After every run such a test's outcome is
  unknown, so it runs again next time. Set `trust_browser_passes: true` on the suite
  to record its passes instead, with what smates knows statically: the test's import
  closure, what the Vitest configuration loads (setup files, plugins, alias targets),
  and for a story the configuration directory, the import closures of its `preview.*`
  and `main.*`, and the `staticDirs` of `main.*`. A change to any of them selects the
  test again. A story is never trusted when its known dependencies include an import
  smates cannot resolve (an alias only Vite's configuration knows, a virtual module), or
  when `main.*` sets `viteFinal` or `previewAnnotations` or names a local addon or
  preset, which may load files smates cannot follow. An addon or framework counts as a
  package only when it is a package name, written as a string or resolved by
  `getAbsolutePath(...)` or `require.resolve(...)`; any path (`./preset`,
  `path.join(__dirname, "..", "preset")`, a template literal) or other expression
  counts as local, and so does an `{ name, options }` object with a `...spread`, which
  may override the name or bring options holding paths. smates reports the stories it does
  not trust, and why, when it records them. Check the
  setting with `smates run --shadow` before relying on it; see
  [Limitations](../reference/limitations.md#what-tracing-reaches).

A suite command that runs some projects with `--project <name>` gets the test files of
those projects. A project whose name smates cannot read (no literal `test.name`, so
Vitest names it after its package or directory), a browser project and every project
when the name is a pattern are kept, so that no test of a project the command runs is
left out.

### jest

Jest workers run many files each, which would merge their dependencies. While
recording, smates makes jest restart a worker after every file
(`--workerIdleMemoryLimit=0` on Jest 30+, a tiny limit on Jest 29). Jest occasionally
still runs two files back to back in one worker; their dependencies are then merged,
which is still correct, only coarser.

smates does not recycle workers on Jest 28 or older (or when it cannot find jest's
version in `node_modules`), when you set `workerIdleMemoryLimit` yourself, when the
tests run in band (`--runInBand`, `-i`, or `runInBand` in `jest.config.*`), or when the
command names a configuration smates cannot see (`--config`, `-c`, `--projects`,
`--rootDir`). Each test file then depends on everything its worker ran (in band, on
everything the run used), and smates warns that dependencies were merged.

`isolate: true` makes it exact: one jest process per file while recording, at the cost
of one jest start per file. `record_jobs` bounds how many run at once (default: the CPU
count, or 1 with `--runInBand`).

smates reads `testMatch` and the other test patterns from your jest configuration, not
from a `preset` it names. When a preset sets them, set `testMatch` in your own
configuration, or `tests:` on the suite.

### node --test

`node --test` runs every file in its own process, so exit codes are per file. smates
always passes the files explicitly, because node's own discovery would also run
helpers smates does not consider tests. If the command uses `--test-isolation=none`
(one process for all files), set `isolate: true`.

`smates init` gives Node's runner a suite where a package announces it (`node --test`
in the `test` script), and also where no package does: test files that import
`node:test` outside every JS package with a test runner (scripts run by CI or a task
runner) become a `node` suite at their package, or at their directory, with `tests:`
globs that match them.

### bun

While recording, smates runs `bun test --coverage` once per file and reads bun's
coverage and module cache. A preload, put first in a copy of your `bunfig.toml`, hooks
`node:fs`, `Bun.file` and `Bun.Glob`. Runs that do not record use one `bun test` as
usual (per-file status then needs a JUnit report).

### Package managers, workspaces and paths

- npm, pnpm, yarn and bun all work. smates runs local binaries from
  `node_modules/.bin` directly; otherwise it uses `pnpm exec`, `yarn`, `bunx` or
  `npx --no-install`, depending on the lockfile.
- smates never lets `npx` or `bunx` download a runner. If the command would fetch a
  tool from the registry, the run stops with a message to install it first.
- npm/pnpm/yarn/bun workspaces and tsconfig `paths` are resolved by the static
  analyzer, so imports between packages of a monorepo are followed (see
  [What the import graph understands](#what-the-import-graph-understands)). A package of
  the repository imported by name (a workspace package, a `file:` dependency) makes its
  `package.json` a dependency too: its `main`, `exports` and `type` decide what the
  import loads, also when the package is installed as a copy.
- Lockfiles, manifests and tsconfig files are inputs of every test; see
  [Default inputs](../reference/configuration.md#default-inputs) for the exact list. A
  tsconfig that is not among them (a nested package's, or one they extend) still steers
  the import graph through its `paths`.
- In vitest and bun suites, imports used only for types (`import type`) are not
  dependencies; see [Limitations](../reference/limitations.md#what-tracing-reaches).

### What the import graph understands { #what-the-import-graph-understands }

The static analyzer resolves imports the way TypeScript, Node and bundlers do, and when a
specifier may mean several files, the test depends on all of them.

??? note "Details: what the analyzer reads"
    - `.js`, `.mjs`, `.cjs`, `.jsx`, `.ts`, `.mts`, `.cts`, `.tsx`, and in `.vue`,
      `.svelte` and `.astro` components the `<script>` blocks, Astro's frontmatter and
      relative `src` attributes;
    - `import`, `export … from`, `import()`, `require()`, `require.resolve()` and
      `import.meta.resolve()`;
    - tsconfig and jsconfig `paths` and `baseUrl`, through `extends`, of the nearest
      config directory: `tsconfig.json`, `jsconfig.json`, sibling `tsconfig.*.json` files
      (such as `tsconfig.app.json`) and the projects they list in `references`;
    - package.json `imports` (`#internal/x`);
    - workspace packages by name, through `exports` (every condition), `main` and
      `module`. Every directory with a `package.json` counts, so no workspace
      configuration is read. When the entry point does not exist in the working tree (an
      unbuilt `dist/`), the import depends on all of the package's source files. The
      package's `package.json` is a dependency of the import either way;
    - `@/x` and `~/x` as `src/x` (else `x`) of the nearest package, and `$lib/x` as
      `src/lib/x`, when nothing else resolves them (aliases usually set in a bundler
      config);
    - `import.meta.glob(...)` patterns, `require.context(dir)`, template-literal imports
      (`` import(`./locales/${lang}.json`) `` depends on every file that matches),
      `new URL("./file", import.meta.url)` and `/// <reference path="...">`;
    - `jest.mock`, `vi.mock` and their relatives (`doMock`, `requireActual`, …): the
      module, and its manual mock in the adjacent `__mocks__` directory;
    - for any bare import (`lodash`, `@scope/pkg`), `__mocks__/<name>` at the package root
      and at the repository root, which Jest and Vitest may load instead.

    Packages in `node_modules` are no dependencies (the lockfile stands for them). An
    alias only a bundler config defines, beyond the ones above, is not resolved: tracing
    still sees the files it loads while recording.

### The cost of recording

Recording makes a Node run slower than the same run without smates. V8 collects precise
coverage of every script each process runs and writes it out as JSON whenever a process
ends (and, for vitest, before a worker is stopped), the `fs` hooks see every file access,
and smates reads all those coverage files after the run. jest also restarts a worker after
every file, and bun runs one process per file. How much slower depends on how much code
the tests load and on how many worker processes start.

`smates run --no-record` runs the selected tests without tracing, at the runner's own
speed. Selection is the same, but the state is left as it was: the tests that ran are not
recorded (neither their new dependencies nor their results), so the next recorded run
selects them again, and tests that have never been recorded stay new. Use it for quick
local iterations, and let a recorded run (CI, or a plain `smates run`) bring the state up
to date. Runners that report per-file status without tracing (vitest, jest, pytest,
`go test`, and any suite with a `junit:` report) still report which files failed.

### Tests that measure time { #tests-that-measure-time }

V8 coverage does not slow all code down by the same factor: it changes how V8 compiles
and optimizes it. A test that asserts on a duration (`expect(elapsed).toBeLessThan(50)`)
or on a ratio of two measurements (a run on 4× the input takes less than 6× as long) can
then pass under the runner and fail under `smates run`, every time, while it passes with
`smates run --no-record`.

List such tests in the suite's `untraced` (globs relative to `root`):

```yaml
suites:
  - name: web
    runner: vitest
    untraced: ["test/perf.test.ts", "**/*.bench.test.ts"]
```

While recording, smates runs them without code coverage (no `NODE_V8_COVERAGE`, no
`bun test --coverage`), one runner process per test file, after the other tests. The `fs` hooks still see the files they
read, the directories they list, the environment variables they read and the processes
they start; the code they run is taken from their static import closure, with what the
runner's configuration loads (setup files, plugins, alias targets). A pass is recorded
with those dependencies, so the test is skipped until one of them changes.

When smates cannot tell all the code such a test runs, its pass is recorded as unknown
and it runs again next time, and the log says why
(`untraced test(s) passed but smates cannot tell all they depend on without coverage`):

- static analysis is off (`static: false`), or the runner's configuration cannot be read;
- a test file is not JavaScript or TypeScript;
- a file in its import closure imports something smates cannot resolve, or imports
  `child_process`, `worker_threads`, `cluster`, `module`, `wasi`, `sqlite` or `bun`
  (with or without `node:`), uses `Bun.file`, `Bun.spawn`, `Bun.$` or a Deno file or
  process API, or calls `import()` or `require()` with a name computed at run time.
  These run code, or read files, that neither the hooks nor the import graph see.

Packages under `node_modules` are not dependencies (the lockfile stands for them). A
package that reads files of the repository at run time does so through `fs`, which the
hooks see.

Only vitest, jest, `node --test` and bun suites can record untraced tests this way. In a
pytest, unittest, Go or custom suite the tests `untraced` lists run without any tracing
while recording, and run again every time.

`smates doctor` warns about JavaScript and TypeScript test files that read a clock
(`performance.now()`, `process.hrtime`, `Date.now() - ...`) and assert on the order of
values, unless they use fake timers or `untraced` already lists them
(`suite-timing-tests`). `untraced` is left out of the suite's settings hash: adding or
removing a glob reruns nothing. To keep such a test out of smates entirely, add it to
the suite's `exclude` and run it on its own.

## Python

smates puts its shims on `PYTHONPATH`. They trace which functions run through
`sys.monitoring` (Python 3.12+) or `sys.setprofile` (3.8–3.11), and use audit hooks for
`open`, directory listings and `stat`.

### pytest or unittest

A Python suite uses `pytest` when anything points to it, and `unittest` otherwise:

- `pytest.ini` or a `conftest.py`;
- pytest named in a manifest, lockfile or task file of the project or a directory above
  it (`pyproject.toml`, `setup.cfg`, `setup.py`, `tox.ini`, `noxfile.py`, `Pipfile`,
  `poetry.lock`/`pdm.lock`/`uv.lock`, `*requirements*.txt`/`.in`, files under
  `requirements/`);
- a GitHub workflow running pytest;
- a test file written for pytest (`import pytest`, module-level `def test_…` functions).

A directory that a CI or task file at the repository root (a GitHub workflow,
`.gitlab-ci.yml`, a Makefile, `tox.ini`, `package.json`, ...) runs with
`python -m unittest discover -s <dir>` gets a `unittest` suite even without a Python
project file, with the command's pattern (and its top-level directory, when it names
one).

### pytest

pytest runs with a plugin that attributes what runs to each test, each fixture and each
collected module. Data a module reads at import time, or behind a cache such as
`functools.cache`, belongs to that module and counts for every test that imports it.
What happens outside any test counts for every test of the process: data read there (by
a `conftest.py` at import time), processes started there (a server a session hook
starts), and fixtures no test declares (requested with `request.getfixturevalue`).
pytest-xdist (`-n auto`) works: the plugin loads in every worker.

### unittest

The test files follow unittest discovery: `test*.py` below the suite root by default, or
the `-s`/`-p`/`-t` (start directory, pattern, top-level directory) of a
`python -m unittest discover …` command:

```yaml
  - name: legacy
    runner: unittest
    root: legacy
    command: "python3 -m unittest discover -s tests -p '*_check.py'"
```

smates always passes the selected files as dotted module names relative to the
top-level directory, also on a full run, so a directory without `__init__.py` is not
skipped. Files whose path is not a module name are not tests.

While recording, the selected modules run in one process through
`python -m smates_unittest`, a wrapper shipped with the shims. It records per-module
contexts (import, `setUpModule`/`setUpClass`, tests) and turns an exception raised while
importing a module into a failure of that module instead of the end of the run.
`isolate: true` runs one process per module instead.

??? note "Details: modules that run no test"
    A module that runs no unittest test is told apart by its source. A helper matching
    the pattern passes, with a warning suggesting `exclude`. A module written for pytest
    is never recorded as passed: its status is unknown, so it runs again, with a message
    suggesting `runner: pytest`. A run in which no module ran a test fails (unittest's
    exit code 5) only when one of them is written for pytest.

### How imports are resolved { #how-python-imports-are-resolved }

The static analyzer resolves a module name against every directory that pytest or a
typical environment may put on `sys.path`, and keeps every match. When your layout puts
code on `sys.path` some other way, add the directory to the suite's
[`python_roots`](../reference/configuration.md#suite-keys).

??? note "Details: import roots and dynamic imports"
    The roots are:

    - the repository root, every Python project directory (`pyproject.toml`,
      `setup.py`, `setup.cfg`) and its `src/`, and the repository's `src/`;
    - pytest's `pythonpath` (in `pyproject.toml`, `pytest.ini`, `tox.ini` or
      `setup.cfg`) and the package locations of setuptools (`packages.find.where`,
      `package-dir`), poetry (`packages = [{ from = … }]`) and hatch (`packages`);
    - the suite's `python_roots`;
    - the parent of the importing file's top-level package and, for a test file, the
      directory of every `conftest.py` pytest loads for it (pytest's default `prepend`
      import mode);
    - the importing file's own directory.

    `from a import b` depends on both `a` and the submodule `a.b`.
    `importlib.import_module`, `__import__` and `pytest.importorskip` with a literal name
    are followed; with a literal prefix (`import_module(f"app.plugins.{name}")`) the test
    depends on every module under it.

### Interpreters and virtualenvs

The default command is `uv run pytest` when the project uses uv (`uv.lock`, or a
`[tool.uv` section in `pyproject.toml`), else the Python of a local `.venv` or `venv`,
else `python3` from `PATH`. smates looks in the suite directory and the directories
above it. Set `command` to use anything else.

### Python versions

| version | tracer | overhead while recording |
|---|---|---|
| 3.12+ | `sys.monitoring` | low |
| 3.8–3.11 | `sys.setprofile` | noticeable |

Before 3.12, a profiler installed later with `sys.setprofile` replaces smates' tracer.

## Go

A Go unit is a package. Its dependencies are the static import closure, which is exact
for Go, plus what the test binary reports through `-test.testlogfile` (files read,
directories listed, environment variables), via `go test -exec`. Node and Python
processes a test starts are traced too. Build failures are attributed to their packages.
The files that pin versions (`go.mod`, `go.sum`, `go.work`, `go.work.sum`) count for
every package that builds with them.

??? note "Details: the static closure"
    The static closure holds the package's own files, those of every package it imports
    (through the repository's `go.mod` files, or `vendor/` when the module or workspace
    vendors its dependencies), the files its `//go:embed` directives name, and the C
    headers a cgo preamble includes from a `-I${SRCDIR}/…` directory outside the
    package. Build constraints are not evaluated there: files for every platform and tag
    count. The files that pin versions (`go.mod`, `go.sum`, `go.work`, `go.work.sum`,
    and `vendor/modules.txt` when vendoring) are static dependencies of every package of
    the module. `go.mod` and `go.sum` of the suite directory, and `go.work`,
    `go.work.sum` and `.tool-versions` at the repository root, are also inputs of every
    test.

In a `go.work` workspace, a dependency bump in any module the workspace uses selects
every test of the workspace.

??? note "Details: `go.work` workspaces"
    Go picks one version of each dependency for all the modules the workspace uses, so a
    `require` bump in one module can change what another builds even when it never
    imports the first. Every Go file of the workspace therefore depends on the `go.mod`
    and `go.sum` of every module listed by `use`, and on the `go.mod` of each local
    `replace` target. A `go.work` smates cannot parse makes every module below it count.
    Go uses the `go.work` nearest the directory it runs in, so a file counts the pins of
    every `go.work` at or above it (a nested `svc/go.work` and the root's). A `go.work`
    that `GOWORK` names elsewhere (in the suite's `env:` or smates' environment) has its
    pins made inputs of every test of the suite. `GOWORK=auto` is the same as leaving it
    unset: the nearest `go.work` is used, and the nested modules it lists stay in the
    suite.

When one package holds most of the tests, set `granularity: func` to select individual
test functions instead. See [Go test functions](go-test-functions.md).

### Go packages without tests

By default only directories with `_test.go` files are units, so a selective run never
compiles a package without tests (a `main` package under `cmd/`): when a library it
imports changes, the library's tests run but the `main` package is not built, and a
change to the `main` package itself is [untested code](orphans.md). `smates run --all`
does compile it (`go test ./...`). To compile such packages in selective runs too, widen
the suite's `tests` to their files:

```yaml
suites:
  - name: go
    runner: go
    tests: ["**/*_test.go", "**/cmd/**/*.go"]   # or ["**/*.go"] for every package
```

Each matching directory is then a unit: `go test ./cmd/app` compiles it and reports
`[no test files]`, or fails it on a build error, and it depends on its static import
closure like any package. smates records `[no test files]` as a pass (with `go test
-json` too), so the unit runs when a package it imports changes, or when its own files
do. With `granularity: func` the directory becomes a compile check (`cmd/app#(build)`)
with no function units.

A directory whose Go files the suite's build constraints all exclude is no unit, and
one whose test files they all exclude is recorded as a skip, so it runs again next time.

??? note "Details: build constraints"
    A directory whose Go files the suite's build constraints all exclude (`//go:build`
    lines, `_linux.go` names, with the tags of `GOFLAGS`, the command and arguments
    passed through) is no unit. When smates cannot tell, it stays one: go list fails, or
    the command is not `go test` itself (`gotestsum -- -tags=...`, `make test`,
    `sh -c "..."`) or passes words after `--`, where tags reach go test unseen.

    When go test then reports "build constraints exclude all Go files" for such a
    directory, that is no test failure: the unit is recorded as a skip (its test
    functions too, with `granularity: func`). The run exits 0 only when smates can tell
    nothing else failed: the command exits with go test's own code 1 (`make` exits 2 when
    a recipe fails), go test's summary lines (plain, or the events of `go test -json`)
    reach smates and report every other package as passing or without test files, and
    each `[setup failed]` of the directory follows such a report. Output a wrapper
    reformats (gotestsum's own lines) or another exit code keeps the run failing. A
    wrapper that runs go test again with the tag compiles the directory, and a test or
    build that fails there fails the run.

    A directory whose test files the configuration all leaves out (a tag in `GOFLAGS`, a
    `_linux_test.go` name on another platform) reports `[no test files]` too, but it is
    recorded as a skip, not a pass, so it runs again on the next run: the environment's
    `GOFLAGS`, `GOOS`, `GOARCH` and `CGO_ENABLED`, `go env -w`, the toolchain and the
    platform are no dependencies, and a configuration that compiles those tests must run
    them.

A package that compiles only after code generation fails like any build failure (exit
1), not as an environment error: smates cannot tell a missing generated file from a real
break. Generated files that git ignores (code produced by a generator step) make these
units fail locally until you generate them. Run the generator first, or leave such
packages out with `exclude:`.

## Custom commands

The `custom` runner runs any command, with the selected test files appended or put in
`{tests}`. It needs `command` and `tests`:

```yaml
  - name: e2e
    runner: custom
    root: e2e
    tests: ["tests/**/*.spec.ts"]
    command: "npx playwright test"
```

While recording, the Node and Python processes the command starts are traced. Status
comes from the overall exit code unless you give a [JUnit report](configuration.md#per-file-results-from-a-junit-report-junit).

## What smates adds to the command { #what-smates-adds-to-the-command }

Besides the test files, smates adds a few arguments to your command, for that run only:

| runner | added | when |
|---|---|---|
| vitest | `--reporter=default`, `--reporter=<smates' reporter>`, and `--reporter=github-actions` on GitHub Actions | unless you configure reporters, or the command names a configuration or root (`--config`, `-c`, `--root`, `--dir`), or there is no `node_modules` directory to place the reporter in (see [vitest](#vitest)) |
| jest | `--reporters=default --reporters=<smates' reporter>` | unless you configure `reporters`, or the command names a configuration (`--config`, `-c`, `--projects`, `--rootDir`) |
| jest | `--workerIdleMemoryLimit=0` (Jest 30+), `=1KB` (Jest 29) | while recording, see [jest](#jest) |
| jest | `--runTestsByPath` | when not running everything, so that the selected files are taken as paths, not patterns |
| bun | `--config=<copy of bunfig.toml>` (smates' preload first, no coverage threshold), `--coverage --coverage-reporter=lcov --coverage-dir=…` | while recording, once per file |
| pytest | `--continue-on-collection-errors` | unless the command has `-x`/`--exitfirst`: one module that fails to import does not stop the other files |
| go (package granularity; see [Go test functions](go-test-functions.md) for `granularity: func`) | `-exec=<smates' wrapper> -count=1` in `GOFLAGS`, and `-overlay=<file>` that adds one test file to each package with tests (your own `-overlay` merged in) | `-exec` and `-overlay` while recording (the overlay lets re-executions of the test binary keep a test log of their own); `-count=1` while recording and with `--shadow`, so no cached result is reused |

`node --test`, unittest and custom suites get only the test files (a unittest run that
records goes through `python -m smates_unittest`, see [unittest](#unittest)). When not
running everything, pytest's exit code 5 (no tests collected) is no failure.

A vitest or jest run that fails before any test file ran and without loading smates'
reporter (for example because the runner rejects an added argument) is run again without
the arguments smates added, except `--runTestsByPath`. Per-file status then falls back to
the overall exit code, and smates says so.

## Child processes

Tests that start other processes are followed everywhere:

- Node, Bun and Python children trace themselves, in any suite (a pytest test running
  `node`, a Node test running `python3`, `pypy3` or `bun`), also when started through a
  shell (`sh -c "node x | tr"`) or `npm`, `npx`, `pnpm` and `yarn`, and are attributed
  to the test that started them.
- Launchers (`uv run`, `uvx`, `bunx`) and the commands of `find -exec` are analyzed as
  the command they start.
- Shell scripts, `grep`, `cat` and other read-only commands are analyzed from their
  command line and script contents: the test depends on the files they name. A script
  called with arguments follows its positional parameters (`$1`, `"$@"`).
- A command smates cannot see into (`make`, a compiled binary) makes the test depend on
  everything below the directory it runs in (`dir/**`), on the program itself when it
  is named by path, and on files named in its option values (`--config=cfg/app.ini`,
  `-Dk=v`) and response files (`@args.txt`).
- A Python child run with `-I`, `-E` or `-S`, or a Node, Bun or Python child whose
  environment drops smates' variables, cannot trace itself and is handled like a
  command smates cannot see into.
- Children of Go tests: see [Child processes of Go tests](#child-processes-of-go-tests).

### Child processes of Go tests { #child-processes-of-go-tests }

Go has no hook in the processes a test starts, so smates follows them through Go's test
log:

- A re-execution of the test binary run as a test binary (the helper-process pattern,
  `exec.Command(os.Args[0], "-test.run=^TestHelperProcess$")`) writes a test log of its
  own, which is merged into the test's dependencies. If it ends in `os.Exit`, panics or
  is killed, its log is lost and the test depends on every file of its Go module:
  return from the helper instead of exiting to keep it precise.
- Any other program (`sh`, `git`, a compiled tool, the binary run with its own
  arguments by a `TestMain` that dispatches to a CLI) makes the test depend on the tree
  of the directory it runs in, its `cmd.Dir`, and the program file when it is in the
  repository.
- Node, Bun and Python children trace themselves, as everywhere.

Paths such a program names outside those directories (`sh -c "cat ../../testdata/x"`)
are not seen; see [Limitations](../reference/limitations.md).

A server the tests reach over the network is not a child process. Declare its code with
`depends_on` (see [Tests that talk to a server](servers.md)).

## Works with coverage tools

Keep collecting coverage in the suite `command`. smates records on top of it, and the
reports stay the same as without smates:

| tool | how both keep working |
|---|---|
| `vitest --coverage` (v8 provider), jest `--coverage --coverageProvider=v8` | They take V8's coverage through `node:inspector` and stop it, which would leave smates' `NODE_V8_COVERAGE` files empty; smates records the scripts listed in the coverage they take instead. |
| `c8` | c8 uses `NODE_V8_COVERAGE` as its temporary directory when it is set, so it reads smates' files for its report (`--reports-dir` output as usual, but no `coverage/tmp`). Every runner invocation gets a directory of its own, so c8's `--clean` never deletes another invocation's files. |
| `node --test --experimental-test-coverage` | Works as is; smates' preload lives in a `node_modules` directory, which coverage reports leave out. |
| your own `NODE_V8_COVERAGE` (environment or suite `env`) | Replaced while recording; the raw coverage files are copied into your directory after the run (existing files are never overwritten or deleted). |
| pytest-cov / coverage.py | Every core: `sys.settrace` (ctrace, pytrace) and `sys.monitoring` (`COVERAGE_CORE=sysmon`). smates takes a `sys.monitoring` tool id of its own (3, 4, 2 or 5; coverage.py uses 1 and moves up when it is taken), and before 3.12 chains to any profile function already installed. What pytest-cov reads for its report after the tests is no dependency of any test. |
| pytest-xdist (`-n auto`) | The plugin loads in every worker; dependencies and results are per test file as without xdist. |
| `go test -cover`, `-coverpkg`, `-coverprofile`, `-covermode` | Unaffected by smates' `-exec` wrapper; the profile is written as usual. With `-coverpkg` (on the command, after `--` or in `GOFLAGS`), go builds the packages it names into every test binary, so every test package also depends on their source files (a compile error there fails them all). |

!!! warning
    A command that sets `NODE_V8_COVERAGE` itself (`env NODE_V8_COVERAGE=… node`, or a
    script that exports it) hides the code the processes below it execute from smates.
    Set it in the environment or the suite `env` instead. c8 writes no `coverage/tmp`
    while smates records, so let c8 report in the same command rather than running
    `c8 report` afterwards.

## See also

- [Configure suites](configuration.md): `command`, `tests`, `isolate`, `junit`
- [Go test functions](go-test-functions.md): `granularity: func`
- [Environment variables](environment.md): what counts as a read in each language
- [Limitations](../reference/limitations.md)

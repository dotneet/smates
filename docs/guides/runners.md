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
probes. The static analyzer adds the import graph, so a new import counts at once.

### vitest

Vitest runs each file in a fresh worker by default, so coverage per worker attributes
exactly. Reads made in `globalSetup` count for every test. If your project sets vitest's
`isolate: false` (shared workers), set `isolate: true` on the suite so that smates runs
each file on its own while recording.

smates adds a reporter next to yours for per-file results. When you configure
`reporters` yourself, it does not add one, and a failing run then marks every recorded
file failed.

### jest

Jest workers run many files each, which would merge their dependencies. While
recording, smates makes jest restart a worker after every file
(`--workerIdleMemoryLimit=0` on Jest 30, a tiny limit on Jest 29). Jest occasionally
still runs two files back to back in one worker; their dependencies are then merged,
which is still correct, only coarser.

`isolate: true` makes it exact: one jest process per file while recording, at the cost
of one jest start per file. `record_jobs` bounds how many run at once (default: the CPU
count, or 1 with `--runInBand`).

### node --test

`node --test` runs every file in its own process, so exit codes are per file. smates
always passes the files explicitly, because node's own discovery would also run
helpers smates does not consider tests. If the command uses `--test-isolation=none`
(one process for all files), set `isolate: true`.

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
  analyzer, so imports between packages of a monorepo are followed.
- Lockfiles, `package.json`, `tsconfig*.json`, `.npmrc`, `.nvmrc` and similar files, in
  the suite directory and every directory above it, are inputs of every test.

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

### pytest

pytest runs with a plugin that attributes what runs to each test, each fixture and each
collected module. Data a module reads at import time, or behind a cache such as
`functools.cache`, belongs to that module and counts for every test that imports it.
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
`go.mod`, `go.sum`, `go.work` and `.tool-versions` are inputs.

When one package holds most of the tests, set `granularity: func` to select individual
test functions instead. See [Go test functions](go-test-functions.md).

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

## Child processes

Tests that start other processes are followed everywhere:

- Node, Bun and Python children trace themselves, in any suite (a pytest test running
  `node`, a Node test running `python3` or `bun`), also when started through a shell
  (`sh -c "node x | tr"`), and are attributed to the test that started them.
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

# smates.yml

Every key of `smates.yml`, with its type, default and meaning. To learn how to set up
suites step by step, read [Configure suites](../guides/configuration.md).

smates reads the first of `smates.yml`, `smates.yaml`, `.smates.yml` and `.smates.yaml`
at the repository root (or the file `--config` names). Without one, it detects the
suites itself (`smates list` shows them; `smates init` writes them down).

## Full example

```yaml
version: 1
suites:
  - name: web
    runner: vitest
    root: packages/web
    tests: ["src/**/*.test.ts"]        # default: read from the runner's config
    exclude: ["src/legacy/**"]
    command: "pnpm exec vitest run"     # selected files are appended (or put in {tests})
    inputs: ["/proto/**"]              # "/" = repository root; otherwise relative to root
    rules:                             # dependencies tracing cannot see
      - tests: ["src/db/**"]
        deps: ["migrations/**"]
    env: { TZ: UTC }
    isolate: true                      # one process per test file while recording
    junit: reports/junit.xml           # per-file status from a JUnit report you produce
  - name: api
    runner: go
    root: services/api
    granularity: func                  # one unit per test function (default: package)
    record_jobs: 4                     # with func: functions recorded at once (default 1)
    requires:                          # left for later where these are not met
      - tcp: 127.0.0.1:5432            # something must accept connections here
        tests: ["internal/store/**"]   # only these tests need it (default: all)
      - env: DATABASE_URL              # must be set and non-empty
  - name: e2e
    runner: custom
    root: e2e
    command: "npx playwright test"
    depends_on:                        # code of the server(s) the tests talk to
      - api                            # = {suite: api}
      - paths: ["docker-compose.yml"]  # repo-relative globs
        tests: ["admin/**"]            # only these tests (relative to root)
    serves: ["services/worker/**"]     # shorthand for depends_on: [{paths: [...]}]
  - name: tools
    runner: pytest
    root: tools
    python_roots: ["src"]              # extra import roots for static analysis
  - name: legacy
    runner: unittest
    root: legacy
    command: "python3 -m unittest discover -s tests -p '*_check.py'"

orphans: agent                         # agent | all | ignore
untested: skip                         # skip | orphan
ignore: ["design/**"]                  # added to the built-in defaults
inputs: [".github/workflows/test.yml"] # every test of every suite depends on these
env_ignore: ["BUILD_*", DEPLOY_ID]     # "*" turns environment tracking off
env_track: [LANG]                      # track a variable the defaults ignore
full_run_branches: [main, release]     # default: the default branch
state: .smates/state.json.gz
state_seed: true
state_shared: false
flaky:
  retry: 0                             # 0..5
agent:
  enabled: auto                        # auto | always | never
  provider: auto                       # auto | anthropic | openai | openrouter
  # model: claude-haiku-5-5            # default depends on the provider
  effort: low
  max_turns: 12
  timeout: 120s
  instructions: "Files under deploy/ configure production only."
  explain_failures: true
  explain_max: 3
  # command: "my-agent --json"         # an external agent instead of the built-in one
```

Unknown keys are an error, and so are the invalid values smates checks when it loads
the file, such as a value outside a key's list (`orphans`, `granularity`, an unknown
`runner`), a negative `record_jobs`, `max_age` or `explain_max`, a `flaky.retry` outside
0–5, a bad glob where [Globs](#globs) says it is checked, or a malformed `requires`
entry. smates then exits with code 3 and names the key (see
[Exit codes](exit-codes.md)). Some values are not checked: the globs listed as unchecked
under [Globs](#globs), and `agent.model` and `agent.effort`, which go to the provider
as written. `smates config set`, `smates config add-ignore` and `smates config add-input`
edit the file and validate it before writing (see [Commands](cli.md#smates-config)).

!!! note "What an edit reruns"
    Editing a suite's entry reruns that suite's tests: every test depends on its suite's
    settings as written (and on the top-level `env_track`), its `inputs`, `rules`,
    `depends_on` and `serves` included. The exceptions are `volatile` and `max_age`,
    which only decide when tests run, `untraced`, which decides how a run is observed,
    and `requires`, which decides where they can run.
    A glob added to the top-level `inputs` makes the files it matches new dependencies
    of every test, so every test runs once. Editing the other top-level keys (`ignore`,
    `orphans`, `agent`, ...) or another suite reruns nothing. A test that reads
    `smates.yml` itself depends on its content as usual.

## Globs

Globs use `/` as the separator, and `**` matches any depth. Write them relative to the
directory the key names (the repository root or the suite `root`), without a leading
`./`.

The globs of `rules`, `depends_on`, `serves`, `volatile`, `untraced` and the `tests` of `requires`
are checked when smates loads the file: an invalid one is a configuration error
(exit 3), and a leading `./` is removed. The globs of the top-level `ignore` and
`inputs`, and of a suite's `tests`, `exclude` and `inputs`, are used as written: one that
is invalid or starts with `./` matches nothing, without an error.
(`smates config add-ignore` and `add-input` refuse such globs.) A `requires` `file` is
a path that may hold a glob: an invalid glob there is a configuration error, and a
leading `./` does no harm.

## Top-level keys

| Key | Type | Default | Meaning |
|---|---|---|---|
| `version` | int | `1` | Config format version. Only `1` is supported. |
| `suites` | list of [suites](#suite-keys) | auto-detected when there is no config file | The test suites. A config file without suites is an error when smates needs them. |
| `ignore` | list of globs (repo-relative) | `[]` | Changed files that are never orphans (changes no test depends on), added to the [built-in defaults](#built-in-ignore-globs). A test that does read such a file is still selected when it changes. See [Changes no test explains](../guides/orphans.md). |
| `inputs` | list of globs (repo-relative) | `[]` | Files every test of every suite depends on: CI workflow, toolchain pins, shared fixtures. |
| `orphans` | `agent`, `all`, `ignore` | `agent` | What to do with changes no test depends on: ask the [agent](../guides/agent.md) (falls back to `all` when it is unavailable or fails), run the suites they may affect in full, or do nothing. `smates run --orphans` overrides it for one run; the flag's values differ: `agent`, `full` (as `all`) and `skip` (like `ignore`, noted as "skipped by caller" in the output). |
| `untested` | `skip`, `orphan` | `skip` | A changed source file that no test imports or executed: `skip` reports it as untested code and runs nothing for it; `orphan` handles it like any other orphan. |
| `env_ignore` | list of names or globs | `[]` | Environment variables whose changes select nothing, added to the [built-in list](../guides/environment.md#variables-ignored-by-default). `"*"` turns environment tracking off. Patterns use `path.Match` syntax and are case-sensitive. |
| `env_track` | list of names or globs | `[]` | Variables to track although the built-in ignore list has them (e.g. `LANG`). It only takes names out of the built-in list: a name `env_ignore` lists stays ignored. |
| `full_run_branches` | list of branch names | the default branch | Branches on which `smates run` runs every test, when it detects a CI and the build is not a pull request (a push, scheduled or manually started build; a GitHub `merge_group` build counts as a pull request). Names match exactly; globs are not supported. |
| `state` | path (repo-relative) | `.smates/state.json.gz` | Where the state file lives. A `.gz` name is gzip-compressed. With `state_shared`, the shared file takes its file name, in `smates/` under the git common directory, and each worktree's snapshot sits at this path with `.snapshot` inserted before the first extension (by default `.git/smates/state.json.gz` and `.smates/state.snapshot.json.gz`). |
| `state_seed` | bool | `true` | In a linked git worktree without a state, or when the shared file of `state_shared` does not exist yet, start from the main worktree's state (or the remote one) instead of cold. `SMATES_NO_STATE_SEED=1` turns it off too. See [Parallel worktrees](../guides/worktrees.md). |
| `state_shared` | bool | `false` | Keep the per-test entries in one file under the git common directory, shared by all worktrees; each worktree keeps its own snapshot. |
| `flaky` | [map](#flaky) | | How to treat tests known to be flaky. |
| `agent` | [map](#agent) | | The LLM agent smates calls for orphans and failure hints. |

## Suite keys

One entry of `suites:`.

| Key | Type | Default | Meaning |
|---|---|---|---|
| `name` | string | the runner, or `root` when it is not `.` | Name used by `--suite`, `--only`, `depends_on` and the output. Must be unique. |
| `runner` | `vitest`, `jest`, `node`, `bun`, `pytest`, `unittest`, `go`, `gradle`, `custom` | **required** | How the suite runs and is traced. See [Languages and runners](../guides/runners.md). |
| `root` | path (repo-relative) | `.` | Directory the command runs in. Must be inside the repository. |
| `command` | string | inferred from the runner | Base command line. Selected test files are appended, or substituted for `{tests}` when the command contains it. Required for `custom`. |
| `tests` | list of globs (relative to `root`) | read from the runner's configuration, else the runner's defaults | Which files are test files. See [Which files are tests](../guides/configuration.md#which-files-are-tests). |
| `exclude` | list of globs (relative to `root`) | `[]` | Files that are never test files. Without `tests:`, added to what the runner configuration excludes. With `tests:`, the test files are not taken from the runner configuration: only the runner's default excludes (such as `node_modules/`) and this list apply. |
| `inputs` | list of globs | `[]` | Files every test of the suite depends on, added to the runner's [default inputs](#default-inputs) (lockfiles, manifests, the runner's config). Relative to `root`; a leading `/` makes a glob repo-relative. |
| `rules` | list of [rules](#rules) | `[]` | Dependencies of some tests that tracing cannot see. |
| `depends_on` | list of [dependencies](#depends_on) | `[]` | Code the tests exercise across a process boundary (a server started by docker compose, an HTTP API). See [Tests that talk to a server](../guides/servers.md). |
| `serves` | list of globs (repo-relative) | `[]` | Shorthand for `depends_on: [{paths: [...]}]`. |
| `env` | map of string to string | `{}` | Environment variables for the suite's tests. They override the shell, and changing a value reruns the suite. |
| `isolate` | bool | `false` | While recording, run each test file in a process of its own. Honored by `vitest`, `jest`, `node` (under `--test-isolation=none`) and `unittest`. `bun` always records one process per file. `gradle` records one JVM per test class unless it is set to `false`. |
| `record_jobs` | int (≥ 0) | see meaning | How many per-file processes run at once while recording with one process per file: `1` for `bun` and for commands that run files serially (`--runInBand`, `--no-file-parallelism`, `--test-isolation=none`, `--test-concurrency=1`), else the CPU count. With `granularity: func`: how many test functions of one package are recorded at once (default `1`). `0` means the default. |
| `granularity` | `package`, `func` | `package` | Go only. `func` makes each top-level test function a unit (`function` is accepted as an alias). See [Go test functions](../guides/go-test-functions.md). |
| `static` | bool | `true` | Use the static import graph in addition to tracing. |
| `trust_browser_passes` | bool | `false` | vitest only. Record a pass of a test that runs only in a browser (browser mode, Storybook stories), which leaves no trace, with the dependencies smates knows statically, so that it is skipped until one of them changes. Off, such a test runs again on every run. See [Browser mode and Storybook](../guides/runners.md#browser-mode-and-storybook). |
| `junit` | path (relative to `root`) | | A JUnit XML report the command writes. smates reads per-file pass/fail from it when the runner cannot report it otherwise. A report that is missing or cannot be read is a warning, not an error. When the runner's reporter also has a result for a file, the worse one wins (fail beats unknown, which beats pass). |
| `python_roots` | list of paths (relative to `root`) | `[]` | Extra import roots for Python static analysis (for example `src` in a src layout). |
| `env_ignore` | list of names or globs | `[]` | Like the top-level key, for this suite only (added to it). |
| `env_track` | list of names or globs | `[]` | Like the top-level key, for this suite only (added to it). |
| `volatile` | list of globs (relative to `root`) | `[]` | Test units that run every time, whatever changed (reason `volatile`): tests that depend on something smates cannot track, such as the date. Matched like a rule's `tests`: a unit ID, and for a Go test function (`pkg#TestName`) also its package directory and its test file. Not for compile checks. |
| `untraced` | list of globs (relative to `root`) | `[]` | Test units that must run without code coverage while recording, because tracing changes what they measure (timing and ratio assertions). They run in a batch of their own; a pass is recorded with the files they read and their static import closure, or stays unknown (and runs again) when smates cannot tell all the code they run. vitest, jest, node and bun only; in other suites they run without tracing and run every time. Matched like `volatile`. See [Tests that measure time](../guides/runners.md#tests-that-measure-time). |
| `max_age` | duration (`168h`, `30m`) | `0` (never) | A test unit whose last recorded run is older than this runs again, whatever changed (reason `expired`). |
| `requires` | list of [requirements](#requires) | `[]` | What the environment must provide for the tests to run (a database, a browser). Tests whose requirements are not met do not run and stay selected. |

### Default inputs

Every test of a suite depends on these files without your listing them. Names are
looked up in the suite root unless the table says otherwise.

| runner | default inputs |
|---|---|
| `vitest`, `jest`, `node`, `bun` | `package.json`, `pnpm-lock.yaml`, `pnpm-workspace.yaml`, `package-lock.json`, `npm-shrinkwrap.json`, `yarn.lock`, `bun.lock`, `bun.lockb`, `.npmrc`, `.yarnrc.yml`, `.nvmrc`, `.node-version`, `.tool-versions` and `bunfig.toml`, in the suite root and every directory above it up to the repository root; `tsconfig*.json`, `.env` and `.env.*` in the suite root; `tsconfig*.json` at the repository root |
| `vitest` (also) | `vitest.config.*`, `vite.config.*`, `vitest.*.config.*`, `vite.*.config.*`, `vitest.workspace.*`, `vitest.setup.*`; `vitest.config.*` and `vitest.workspace.*` at the repository root |
| `jest` (also) | `jest.config.*`, `jest.setup.*`, `babel.config.*`, `.babelrc`, `.babelrc.*`, `.swcrc`; `jest.config.*` and `babel.config.*` at the repository root |
| `pytest` | `pyproject.toml`, `uv.lock`, `setup.cfg`, `setup.py`, `pytest.ini`, `tox.ini`, `requirements*.txt`, `.python-version`; at the repository root `pyproject.toml`, `uv.lock`, `.python-version`, `requirements*.txt`, `pytest.ini`, `.tool-versions` |
| `unittest` | as `pytest`, without either `pytest.ini` |
| `go` | `go.mod`, `go.sum`; at the repository root `go.work`, `go.work.sum`, `.tool-versions`; with `GOWORK` set (in the suite's `env` or the environment), that `go.work`, its `go.work.sum`, the `go.mod` and `go.sum` of its modules, and the `go.mod` of each local `replace` target |
| `gradle` | of the Gradle build the suite belongs to (the nearest directory with `settings.gradle(.kts)`): the settings and `settings-gradle.lockfile`, every `build.gradle(.kts)`, `gradle.properties`, `gradle.lockfile` and `gradle/dependency-locks/**`, `gradle/*.versions.toml`, the wrapper (`gradlew`, `gradlew.bat`, `gradle/wrapper/gradle-wrapper.properties`), `buildSrc/**` and the included builds (`includeBuild`); at the repository root `.tool-versions`, `.java-version`, `.sdkmanrc` |
| `custom` | none |

### rules

| Key | Type | Meaning |
|---|---|---|
| `tests` | list of globs (relative to the suite `root`) | The tests the rule applies to. |
| `reaches` | list of globs (repo-relative) or `$NAME` patterns | The rule applies only to the tests whose static import closure reaches a file a glob matches, or, for a `$NAME` entry (a glob over variable names, such as `$DB_*`), whose last recorded run read that environment variable. With `tests`, a test must match both. |
| `deps` | list of globs (repo-relative) | Files those tests depend on. **Required.** |

A rule needs `tests` or `reaches`. A leading `./` is removed from its globs.

```yaml
rules:
  - tests: ["src/db/**"]
    deps: ["migrations/**"]
  # Every test that imports the database test helper, wherever it lives.
  - reaches: ["internal/testdb/**", "$APP_DB_*"]
    deps: ["db/migrations/**"]
```

`reaches` follows the file-level closure, including what other helpers and the other
test files of a Go package import, and the files the runner loads for every test through
its configuration (Vitest `setupFiles` and `globalSetup`, Jest `setupFilesAfterEnv` in
`jest.config.*`, JSON included, or the `jest` key of `package.json`, bun's `preload` in
`bunfig.toml` or `--preload`, and what they import); for `granularity: func` it matches
at the package level. A glob without a wildcard that names a directory
(`internal/testdb`) matches what is below it. A `$NAME` entry also matches a test
whose closure's source names such a variable (`os.Getenv("APP_DB_DSN")`), or names a
prefix of it in a string literal (`envconfig.Process("APP", &cfg)`,
`viper.SetEnvPrefix("app")`, `env_prefix="APP_"`): Go records no read made before
`m.Run` (in `TestMain`, `init` or a package-level initializer), nor a scan of
`os.Environ`; shell, Bun and in-browser reads are not traced; settings loaders that
scan the environment are not counted as reads. A loader that builds the name another
way leaves no trace in the source: name such tests with `tests:`. When smates cannot
tell, the rule applies: static analysis is off (`static: false`), no analyzer reads the
test's files, the closure (or a setup file the runner loads) has a JavaScript or
TypeScript import smates cannot resolve (a bundler alias, a virtual module, a missing
file), the runner's configuration cannot be read (a JSON file that does not parse, a
bun preload that is no string, a `--preload` from a variable), or, for a `$NAME` entry,
the test has no recorded run yet, the pattern may name a variable `env_ignore` (or the
default ignore list) keeps out of every recorded run, or the test's source cannot be
read (static analysis off, or no analyzer for its files), whatever its runner. Imports in Python, Go and
Java that smates cannot resolve are dropped from the closure without a trace: a Python
test that reaches a helper through `sys.path` (`from dbkit import reset`, with
`support/` added to the path) does not reach `support/dbkit.py` as far as the rule can
tell. Add the directory to the suite's `python_roots` (or to pytest's `pythonpath`,
which smates reads too), name such a test with `tests:` in another rule, or import the
helper by its repository path. A test that reaches the database in a way the rule does not
name (its own `sql.Open`, a helper not in the list, a child process) does not get the
deps, so name every entry point, and add the variables the database connection reads.
`smates doctor` reports how many test units each such rule applies to, warns about one
that applies to none, and about each glob that matches no file of the repository. See [Tests backed by a database](../guides/configuration.md#tests-backed-by-a-database).

### depends_on

Each entry has exactly one of `suite` and `paths`. A plain string is a suite name.

| Key | Type | Meaning |
|---|---|---|
| `suite` | suite name | The tests depend on that suite's code: the files under its root except test files (of any suite), ignored and vendored files, and files of suites rooted below it. A suite cannot depend on itself. |
| `paths` | list of globs (repo-relative) | The tests depend on these files. No leading `/` or `..`. |
| `tests` | list of globs (relative to the suite `root`) | Narrow the tests that get the dependency (default: all). |

See [Globs](#globs) for how globs are written and checked.

### requires

Preconditions of the environment: a database the tests connect to, a browser they
start, a variable they read. `smates run` checks them before it starts the suite's
tests. Where one is not met, the tests that need it do not run: they are not recorded
(never as passed) and stay selected, so the next run where it is met (or CI) runs them.
The rest of the run goes on. Each entry has exactly one of `tcp`, `cmd`, `env` and
`file`.

| Key | Type | Meaning |
|---|---|---|
| `tcp` | `host:port` | Something must accept a TCP connection there. smates retries until `timeout`, so a server that is still starting can make it. |
| `cmd` | shell command | Must exit 0 within `timeout`. It runs with `sh -c` (`cmd /C` on Windows) in the suite `root`, with the suite's `env`. Use it for checks a port cannot show, such as a database schema at the expected migration. |
| `env` | variable name | Must be set to a non-empty value, in the environment or in the suite's `env`. |
| `file` | path or glob | Must match an existing file or directory. Relative to the suite `root`, or absolute. |
| `tests` | list of globs (relative to the suite `root`) | Only these tests need it (default: all of the suite). Matched like [`rules`](#rules). |
| `timeout` | duration | For `tcp` (default `2s`) and `cmd` (default `10s`). |

`tcp` and `file` values may use `$VAR` and `${VAR}`, expanded from the environment and
the suite's `env`. A variable they name that is not set (or empty) makes the requirement
not met: `${DB_HOST}:5432` would otherwise dial this machine. No other shell form is
expanded: a default (`${DB_HOST:-localhost}`, `${DB_HOST-localhost}`), `${DB_HOST:?}`,
`$1`, `$$` or a `$` on its own is a configuration error (exit 3), since it would read as
a variable that is never set. For a default, set the variable in the environment that
runs smates (`DB_HOST=localhost` in your shell or `.envrc`, while CI sets its own), or
use a `cmd` requirement, whose shell expands it
(`cmd: 'nc -z "${DB_HOST:-localhost}" 5432'`). The suite's `env` is no default: it
overrides the environment, so `env: {DB_HOST: localhost}` would also replace the
`DB_HOST` CI sets, for the check and for the tests.

```yaml
requires:
  - tcp: 127.0.0.1:5432
    tests: ["internal/store/**", "internal/repo/**"]
  - cmd: ./scripts/schema-is-current.sh
    tests: ["internal/store/**"]
    timeout: 30s
  - file: ${HOME}/.cache/ms-playwright
    tests: ["e2e/**"]
```

A check that cannot tell (a command that is not found or times out, an address with
variables that does not parse once they are expanded) counts as not met. Mistakes smates
can see without running anything are configuration errors (exit 3) instead: a `tcp`
address without variables that is not `host:port`, an `env` that is not a variable
name, a `file` that is not a valid glob, a `timeout` on an `env` or `file` entry, a
negative `timeout`, and an entry written as a plain string (`- 127.0.0.1:5432`; write
`- tcp: 127.0.0.1:5432`). Each check runs once per run (`--dry-run` runs none). When
tests were left out and no test failed, `smates run` exits 4 (an environment problem); see
[`smates run`](cli.md#unmet-requires). The git hooks and the Claude Code and Codex Stop
hooks print one line about them instead of blocking. Adding a requirement or changing
one reruns nothing: it is not part of the suite's settings hash.

## agent

The [LLM agent](../guides/agent.md) smates calls for orphans, and after a failing run
to explain a few failures.

| Key | Type | Default | Meaning |
|---|---|---|---|
| `enabled` | `auto`, `always`, `never` | `auto` | `never` turns the agent off. `auto` uses it when credentials (or `command`) are present; `always` currently behaves like `auto`. |
| `provider` | `auto`, `anthropic`, `openai`, `openrouter` | `auto` | Built-in backend. `auto` takes the first of Anthropic, OpenAI and OpenRouter whose API key is set (see [Variables smates reads](environment-variables.md#agent-credentials)). |
| `model` | string | per provider: `claude-haiku-5-5`, `gpt-6-luna`, `anthropic/claude-haiku-5.5` | Model name. |
| `effort` | string (`low`, `medium`, `high`, ...) | `low` | Reasoning effort, passed to the provider as written: smates does not check the value. On OpenAI and OpenRouter, smates retries without it when the model rejects it; on Anthropic, a rejected value makes the call fail. |
| `max_turns` | int | `12` | Tool-use turns per call. `0` means the default. |
| `timeout` | duration (`120s`, `2m`) | `120s` | Wall-clock limit per call. |
| `instructions` | string | | Appended to the agent's system prompt: project-specific hints. |
| `command` | string | | Run an external agent instead of the built-in one. It gets the prompt on stdin and prints a JSON object with a `decision` field on stdout. |
| `explain_failures` | bool | `true` | Let `smates run` ask the agent for the probable cause of failing tests ([Failure hints](../guides/failure-hints.md)). Built-in backend only. |
| `explain_max` | int (≥ 0) | `3` | Failing tests explained per run. `0` means the default; use `explain_failures: false` to turn explanations off. |

## flaky

| Key | Type | Default | Meaning |
|---|---|---|---|
| `retry` | int, `0` to `5` | `0` | When every failure of a suite run is a test already known to be flaky, rerun that suite run with the same tests up to this many times. If it then passes, the flip is reported and the run does not fail. `0` never hides a failure. See [Shadow mode, flaky tests and stats](../guides/trust.md). |

## Built-in ignore globs

These changes are never orphans, in addition to your `ignore:` (a test that reads one
of them is still selected when it changes). The list is `DefaultIgnore` in
[internal/config/config.go](https://github.com/dotneet/smates/blob/main/internal/config/config.go).

| Kind | Globs |
|---|---|
| Documentation | `**/*.md`, `**/*.mdx`, `**/*.rst`, `docs/**`, `doc/**` |
| Licenses and ownership | `**/*.txt.license`, `**/LICENSE*`, `**/CHANGELOG*`, `**/CODEOWNERS` |
| GitHub metadata | `.github/ISSUE_TEMPLATE/**`, `.github/PULL_REQUEST_TEMPLATE*`, `.github/dependabot.yml` |
| Editors and git | `.vscode/**`, `.idea/**`, `.editorconfig`, `.devcontainer/**`, `**/.gitignore`, `**/.gitattributes` |
| Formatters and linters | `**/.prettierrc*`, `**/.prettierignore`, `**/.eslintrc*`, `**/eslint.config.*`, `**/.markdownlint*`, `**/biome.json`, `**/biome.jsonc`, `**/.golangci.y*ml`, `**/ruff.toml`, `**/.pre-commit-config.yaml` |

The built-in list of environment variables smates does not track (`DefaultEnvIgnore`)
is described in [Environment variables](../guides/environment.md#variables-ignored-by-default).

## See also

- [Configure suites](../guides/configuration.md)
- [Commands](cli.md)
- [Changes no test explains](../guides/orphans.md)

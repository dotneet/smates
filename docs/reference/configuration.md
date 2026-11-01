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

Unknown keys are an error. So is any invalid value: smates exits with code 3 and names
the key (see [Exit codes](exit-codes.md)). `smates config set` and
`smates config add-ignore` edit the file and validate it before writing (see
[Commands](cli.md#smates-config)).

!!! note "What an edit reruns"
    Editing a suite's entry reruns that suite's tests: every test depends on its suite's
    settings as written (and on the top-level `env_track`). Editing the other top-level
    keys (`ignore`, `orphans`, `agent`, ...) or another suite reruns nothing. A test
    that reads `smates.yml` itself depends on its content as usual.

## Top-level keys

| Key | Type | Default | Meaning |
|---|---|---|---|
| `version` | int | `1` | Config format version. Only `1` is supported. |
| `suites` | list of [suites](#suite-keys) | auto-detected when there is no config file | The test suites. A config file without suites is an error when smates needs them. |
| `ignore` | list of globs (repo-relative) | `[]` | Changed files that are never orphans (changes no test depends on), added to the [built-in defaults](#built-in-ignore-globs). A test that does read such a file is still selected when it changes. See [Changes no test explains](../guides/orphans.md). |
| `inputs` | list of globs (repo-relative) | `[]` | Files every test of every suite depends on: CI workflow, toolchain pins, shared fixtures. |
| `orphans` | `agent`, `all`, `ignore` | `agent` | What to do with changes no test depends on: ask the [agent](../guides/agent.md) (falls back to `all` when it is unavailable or fails), run the suites they may affect in full, or do nothing. `smates run --orphans` overrides it for one run. |
| `untested` | `skip`, `orphan` | `skip` | A changed source file that no test imports or executed: `skip` reports it as untested code and runs nothing for it; `orphan` handles it like any other orphan. |
| `env_ignore` | list of names or globs | `[]` | Environment variables whose changes select nothing, added to the [built-in list](../guides/environment.md#variables-ignored-by-default). `"*"` turns environment tracking off. Patterns use `path.Match` syntax and are case-sensitive. |
| `env_track` | list of names or globs | `[]` | Variables to track although the built-in ignore list has them (e.g. `LANG`). |
| `full_run_branches` | list of branch names | the default branch | Branches on which `smates run` on CI (push builds, not pull requests) runs every test. |
| `state` | path (repo-relative) | `.smates/state.json.gz` | Where the state file lives. A `.gz` name is gzip-compressed. |
| `state_seed` | bool | `true` | In a linked git worktree without a state, start from the main worktree's state (or the remote one) instead of cold. `SMATES_NO_STATE_SEED=1` turns it off too. See [Parallel worktrees](../guides/worktrees.md). |
| `state_shared` | bool | `false` | Keep the per-test entries in one file under the git common directory, shared by all worktrees; each worktree keeps its own snapshot. |
| `flaky` | [map](#flaky) | | How to treat tests known to be flaky. |
| `agent` | [map](#agent) | | The LLM agent smates calls for orphans and failure hints. |

## Suite keys

One entry of `suites:`.

| Key | Type | Default | Meaning |
|---|---|---|---|
| `name` | string | the runner, or `root` when it is not `.` | Name used by `--suite`, `--only`, `depends_on` and the output. Must be unique. |
| `runner` | `vitest`, `jest`, `node`, `bun`, `pytest`, `unittest`, `go`, `custom` | **required** | How the suite runs and is traced. See [Languages and runners](../guides/runners.md). |
| `root` | path (repo-relative) | `.` | Directory the command runs in. Must be inside the repository. |
| `command` | string | inferred from the runner | Base command line. Selected test files are appended, or substituted for `{tests}` when the command contains it. Required for `custom`. |
| `tests` | list of globs (relative to `root`) | read from the runner's configuration, else the runner's defaults | Which files are test files. See [Which files are tests](../guides/configuration.md#which-files-are-tests). |
| `exclude` | list of globs (relative to `root`) | `[]` | Files that are never test files, added to what the runner configuration excludes. |
| `inputs` | list of globs | `[]` | Files every test of the suite depends on. Relative to `root`; a leading `/` makes a glob repo-relative. |
| `rules` | list of [rules](#rules) | `[]` | Dependencies of some tests that tracing cannot see. |
| `depends_on` | list of [dependencies](#depends_on) | `[]` | Code the tests exercise across a process boundary (a server started by docker compose, an HTTP API). See [Tests that talk to a server](../guides/servers.md). |
| `serves` | list of globs (repo-relative) | `[]` | Shorthand for `depends_on: [{paths: [...]}]`. |
| `env` | map of string to string | `{}` | Environment variables for the suite's tests. They override the shell, and changing a value reruns the suite. |
| `isolate` | bool | `false` | While recording, run each test file in a process of its own. Honored by `vitest`, `jest`, `node` (under `--test-isolation=none`) and `unittest`. `bun` always records one process per file. |
| `record_jobs` | int (≥ 0) | see meaning | How many per-file processes run at once while recording with one process per file: `1` for `bun` and for commands that run files serially (`--runInBand`, `--no-file-parallelism`, `--test-isolation=none`, `--test-concurrency=1`), else the CPU count. With `granularity: func`: how many test functions of one package are recorded at once (default `1`). `0` means the default. |
| `granularity` | `package`, `func` | `package` | Go only. `func` makes each top-level test function a unit (`function` is accepted as an alias). See [Go test functions](../guides/go-test-functions.md). |
| `static` | bool | `true` | Use the static import graph in addition to tracing. |
| `junit` | path (relative to `root`) | | A JUnit XML report the command writes. smates reads per-file pass/fail from it when the runner cannot report it otherwise. |
| `python_roots` | list of paths (relative to `root`) | `[]` | Extra import roots for Python static analysis (for example `src` in a src layout). |
| `env_ignore` | list of names or globs | `[]` | Like the top-level key, for this suite only (added to it). |
| `env_track` | list of names or globs | `[]` | Like the top-level key, for this suite only (added to it). |

### rules

| Key | Type | Meaning |
|---|---|---|
| `tests` | list of globs (relative to the suite `root`) | The tests the rule applies to. |
| `deps` | list of globs (repo-relative) | Files those tests depend on. |

```yaml
rules:
  - tests: ["src/db/**"]
    deps: ["migrations/**"]
```

### depends_on

Each entry has exactly one of `suite` and `paths`. A plain string is a suite name.

| Key | Type | Meaning |
|---|---|---|
| `suite` | suite name | The tests depend on that suite's code: the files under its root except test files (of any suite), ignored and vendored files, and files of suites rooted below it. A suite cannot depend on itself. |
| `paths` | list of globs (repo-relative) | The tests depend on these files. No leading `/` or `..`. |
| `tests` | list of globs (relative to the suite `root`) | Narrow the tests that get the dependency (default: all). |

Globs everywhere use `/` as the separator (`**` matches any depth). A leading `./` is
removed from `depends_on` and `serves` globs.

## agent

The [LLM agent](../guides/agent.md) smates calls for orphans, and after a failing run
to explain a few failures.

| Key | Type | Default | Meaning |
|---|---|---|---|
| `enabled` | `auto`, `always`, `never` | `auto` | `never` turns the agent off. `auto` uses it when credentials (or `command`) are present; `always` currently behaves like `auto`. |
| `provider` | `auto`, `anthropic`, `openai`, `openrouter` | `auto` | Built-in backend. `auto` takes the first of Anthropic, OpenAI and OpenRouter whose API key is set (see [Variables smates reads](environment-variables.md#agent-credentials)). |
| `model` | string | per provider: `claude-haiku-5-5`, `gpt-6-luna`, `anthropic/claude-haiku-5.5` | Model name. |
| `effort` | `low`, `medium`, `high` | `low` | Reasoning effort. It is dropped for models that reject it. |
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

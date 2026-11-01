# Changes no test explains

Some changes touch files that no test is known to depend on, such as a Dockerfile, a CI
script or a SQL file a service applies. smates calls these *orphans*. This page covers
how smates handles them, and how to make them rare.

## What an orphan is

```text
$ git status --short
 M src/pricing.ts
 M Dockerfile
 M db/seed.sql
 M README.md
```

- `src/pricing.ts` is a recorded dependency of `src/cart.test.ts`, so that test is
  selected.
- `README.md` matches the built-in `ignore:` list, so it needs no test.
- `Dockerfile` and `db/seed.sql` are dependencies of no test, and nothing ignores them.
  They are **orphans**: the deterministic analysis cannot say which tests they affect.

An orphan is a changed file that is in no test's dependency set (see
[How smates decides](../getting-started/how-it-works.md)) and that `ignore:` does not match.
Typical orphans:

| file | why tracing cannot see it |
|---|---|
| `Dockerfile`, `docker-compose.yml` | the tests run *inside* the container, but no test process reads the file |
| `scripts/ci-setup.sh` | CI runs it before the tests, outside of any test |
| `db/seed.sql` | a server the tests talk to loads it, in another process |
| `deploy/values.yaml` | used in production only; affects no test at all |

Some of these matter for tests, and some do not. smates cannot tell them apart by itself,
so it works through a fixed sequence of steps.

## How smates handles an orphan

Each step handles what the earlier steps left over:

1. **Name references.** smates selects tests whose sources or dependencies mention the
   file by name, for example a test that loads `"seed.sql"`, or a fixture list naming a
   migration.
2. **Untested code is skipped.** If the orphan is Go, Python or JS/TS source under a
   suite's root, and no test imports it (directly or through other code) or executed it,
   then no test can be affected. Nothing runs, and the summary and the pull request
   comment list the file under "changes to untested code". Examples are a `main` package,
   or a component without tests.
3. **The agent.** When an API key is set, the [LLM agent](agent.md) explores the
   repository with read-only tools. It then adds specific tests, requests a full run, or
   says the change affects no test. The agent can only *add* tests, so a wrong answer
   can, at worst, fail to add one.
4. **Full suites.** Otherwise, smates runs the suites the orphan can affect in full.

A run that asks the agent logs it:

```text
smates: agent: asking claude-haiku-5-5 (anthropic) about 2 unattributed change(s): Dockerfile, db/seed.sql
smates: agent: db/seed.sql is loaded by the repository tests; the Dockerfile only builds the release image. (4 turns, 5120 in / 210 out tokens, 3.1s)
```

smates writes each handled orphan into the state, so later runs on the same branch do not
handle it again. A coding agent that calls smates can make the decision itself instead
of steps 3 and 4: see [Letting the calling agent decide](coding-agents.md#letting-the-calling-agent-decide).

??? note "Details: which suites \"can be affected\""
    For step 4, the suites an orphan can affect are those whose `root` contains the file.
    A suite rooted at the repository root (`root: .`) contains every file. When it is the
    only suite that contains the file, every suite runs. A file outside every suite root,
    such as a top-level `Dockerfile` in a repository with suites under `packages/`, also
    makes every suite run.

??? note "Details: what step 2 does not count as untested code"
    Tool configuration written as code (`vite.config.ts`, `setup.py`, test setup files,
    `.d.ts`), files under `testdata/`, `fixtures/` or `__mocks__/`, and files a suite
    excludes all go on to step 3. So do files under the root of a suite that has a test
    with no recorded run yet. Without a recorded state, smates knows only imports, so
    step 2 skips nothing.

    Code that *another process* runs (a server an end-to-end suite talks to) also looks
    untested. Declare it with `depends_on` or `rules`, so its tests are selected; see
    [Tests that talk to a server](servers.md).

## Choose a policy

`orphans:` in `smates.yml` decides what happens to the orphans left after steps 1 and 2:

```yaml
orphans: agent     # agent | all | ignore
untested: skip     # skip | orphan
```

| `orphans:` | effect |
|---|---|
| `agent` (default) | ask the agent. Without an API key, or when the agent fails, run the affected suites in full |
| `all` | run the affected suites in full and never call an LLM |
| `ignore` | do nothing. The log says `ignored N unattributed change(s) (orphans: ignore)` |

`untested: orphan` turns step 2 off. Untested source then goes on to the agent or the
full suites like any other orphan. Use it if you would rather run too much than trust
the import graph.

For a single run, `smates run --orphans=agent|full|skip` overrides `orphans:`. See
[Use smates from a coding agent](coding-agents.md#letting-the-calling-agent-decide).

## Ignore files that never need tests

An orphan that shows up on most commits (`.env.example`, deploy configuration, a
frontend no suite covers) belongs in `ignore:`:

```yaml
ignore: ["deploy/**", ".env.example"]
```

Your globs are added to a built-in list:

- documentation: `**/*.md`, `**/*.mdx`, `**/*.rst`, `docs/**`, `doc/**`;
- licenses, changelogs and ownership: `LICENSE*`, `CHANGELOG*`, `CODEOWNERS`;
- GitHub metadata: issue and pull request templates, `dependabot.yml`;
- editor and dev-container settings: `.vscode/**`, `.idea/**`, `.editorconfig`, `.devcontainer/**`;
- git metadata: `.gitignore`, `.gitattributes`;
- formatter and linter configuration: Prettier, ESLint, Biome, golangci-lint, Ruff,
  markdownlint, pre-commit.

The exact globs are `DefaultIgnore` in
[internal/config/config.go](https://github.com/dotneet/smates/blob/main/internal/config/config.go).

!!! note
    `ignore:` only stops a file from being an orphan. If a test does read an ignored file
    (a test that parses `README.md`), changing that file still selects the test.

To add globs without editing YAML by hand, run
`smates config add-ignore 'deploy/**'` (`--dry-run` shows the diff first).

## Find what to ignore: `smates orphans`

`smates orphans` replays recent commits against the current dependency map and tells you
which files would have been orphans. Run it after `smates run --all`. Without recorded
dependencies it knows only the static import graph, so it overstates the orphans.

```text
$ smates orphans
Replayed the last 100 first-parent commit(s) against the current dependency map.
23 of 100 commit(s) changed a file no test depends on; the agent would have been asked (orphans: agent).

Orphans by number of commits:
  9  .env.example         ignore: .env.example
  6  deploy/values.yaml   ignore: deploy/**
  4  Dockerfile           container definition: consider inputs:/rules: instead of ignore

Suggested ignore globs (no test depends on these files):
  .env.example  9 commit(s), 1 file(s)
  deploy/**     6 commit(s), 3 file(s)

May change how tests run; declare them in inputs: (every test) or rules: (some tests) instead of ignoring them:
  Dockerfile (container definition, 4 commit(s))

With these globs, 6 of 100 commit(s) would still have orphans (2 once the files above are in inputs:/rules:).

Paste into smates.yml:

ignore:
  - ".env.example"
  - "deploy/**"
```

- Each commit is compared with its first parent, using the same rule as `smates run`.
- smates suggests a whole `dir/**` only when no file below it is a dependency or is named
  by a test.
- Files that can change how tests run (CI workflows, Makefiles, Dockerfiles, compose
  files, toolchain pins such as `.nvmrc` or `go.mod`) are never suggested for `ignore:`.
  Declare them in `inputs:` (every test depends on them) or `rules:` (some tests do).

| flag | effect |
|---|---|
| `--history N` | replay the last N first-parent commits (default 100; `0`: no limit) |
| `--since rev` | replay the commits after `rev` (no count limit unless `--history` is set) |
| `--apply` | add the suggested globs to `ignore:` in `smates.yml`, keeping its comments. Nothing else is changed |
| `--apply --dry-run` | print the diff instead of writing it |
| `--suite NAME` | limit the replay to a suite (repeatable) |
| `--format json` | machine-readable report |

## Orphans in a `--suite` run

When a run is limited with `--suite`, the tests of the other suites still count as
dependents. A file only another suite's tests depend on is no orphan here.

A change under the root of a suite left out of the run is left to the job that runs that
suite. This is how CI jobs split by suite can share a state: each one handles the
orphans of its own suites.

## See also

- [The LLM agent](agent.md): providers, cost and safety of step 3.
- [Use smates from a coding agent](coding-agents.md): let the caller decide orphans.
- [smates.yml reference](../reference/configuration.md): `ignore`, `inputs`, `rules`, `orphans`, `untested`.

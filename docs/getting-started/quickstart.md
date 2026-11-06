# Quick start

Five minutes from install to a run that only starts the tests your change can affect. You
need smates on your `PATH` ([Install](install.md)) and a git repository whose tests
already run.

## 1. Write the configuration

```text
$ smates init
wrote smates.yml with 2 suite(s):
  - web (vitest) in packages/web
  - api (go) in services/api

Next steps:
  smates run --all     # run everything once and record what each test uses
  smates run           # afterwards: run only what your changes can affect
  smates why <file>    # see which tests depend on a file
  smates hook install  # optional: run the affected tests before every git push
```

`smates init` detects your test suites and writes `smates.yml`. It also adds `.smates/`
(where the recorded state lives) to `.gitignore`. A *suite* is one test runner invocation
over one directory.

Use `smates init --github` to also write a GitHub Actions workflow
([what it sets up](#what-init-github-sets-up)).

!!! tip "No configuration needed to try it"
    Without `smates.yml`, smates detects the suites by itself on every command. Run
    `smates list` to see what it found. `smates init` pins that result in a file you can
    edit.

## 2. Record once

```bash
smates run --all
```

This runs every test once, through your usual test command, and records what each test
uses: the code it executed, the files it read, the environment variables it looked up.
The output is your runner's own, framed by smates' lines:

```text
smates: selected 412 of 412 test unit(s) across 2 suite(s) (0% skipped)
smates: web: running 380 test unit(s) with vitest
...
smates: done in 1m 52s: ran 412 of 412 test unit(s), exit code 0
```

The recording goes to `.smates/state.json.gz`, the *state*. In CI, the default branch
refreshes it on every push ([GitHub Actions](../ci/github-actions.md)).

## 3. Change something and run again

Edit a source file, then:

```text
$ smates run --explain
smates: selected 3 of 412 test unit(s) across 2 suite(s) (99% skipped)
  web (vitest): 3/380
    + src/cart.test.ts  [changed: src/pricing.ts]
    + src/checkout.test.ts  [changed: src/pricing.ts]
    + src/invoice.test.ts  [changed: src/pricing.ts]
  api (go): 0/32
smates: web: running 3 test unit(s) with vitest
...
smates: done in 2.1s: ran 3 of 412 test unit(s), exit code 0
```

Only the tests that use `src/pricing.ts` ran. `--explain` adds one line per selected test
with the reason in brackets; plain `smates run` prints only the `selected ... of ...` line.
The reasons are listed in [How smates decides](how-it-works.md#why-a-test-was-selected).

Run `smates run` again without changing anything: it selects 0 tests and finishes at once.

## 4. Inspect the dependency map

Which tests depend on a file:

```text
$ smates why src/pricing.ts
web	src/cart.test.ts	(static/inputs, recorded)
web	src/checkout.test.ts	(static/inputs, recorded)
web	src/invoice.test.ts	(recorded)
```

What a test depends on:

```text
$ smates deps src/cart.test.ts
$TZ	recorded
fixtures/prices.json	recorded
package.json	static,recorded
smates.yml	static,recorded
src/cart.test.ts	static,recorded
src/pricing.ts	static,recorded
```

`static` means the dependency is known without running the test (the import graph or
the suite's inputs, such as `package.json`); `recorded` means the last run observed it. `$TZ` is an environment variable the test read. Both commands take
`--format json`.

## 5. Check the setup

| Command | Shows |
|---|---|
| `smates list` | Suites, their runner, root, number of test units, command, and where the test file patterns come from. |
| `smates doctor` | Setup checks: suites, toolchains, missing `node_modules`, services that are not running, the recorded state, the agent's credentials. Exits 1 when a check fails. |

```text
$ smates list
web	runner=vitest	root=packages/web	units=380	command=pnpm exec vitest run	tests=vitest.config.ts test.include
api	runner=go	root=services/api	units=32	command=go test	tests=runner defaults
```

## What `init --github` sets up { #what-init-github-sets-up }

`smates init --github` writes `.github/workflows/smates.yml` with the
`dotneet/smates@v1` action, and reads your repository to fill it in:

- **Toolchains** from `mise.toml` / `.tool-versions` (through `jdx/mise-action`),
  `.nvmrc` / `.node-version` or `package.json` `engines.node` / `volta`, and
  `.python-version`.
- **Services and variables** as commented-out `services:` / `env:` blocks to check and
  enable: databases from `docker-compose.yml` / `compose.yaml` or `localhost` URLs in the
  code, and variables from `.env.example` that the code reads.
- **An existing workflow** that already runs the tests is not duplicated: smates prints a
  patch that replaces those steps with the smates action (`--patch FILE` writes it for
  `git apply`; `--force` writes a new workflow anyway). Steps it cannot stand in for
  exactly, such as targeted runs or matrix legs, are reported and left alone, and a
  workflow that runs only on pull requests also gets the triggers that record the state
  (see [Switch existing workflows](../ci/github-actions.md#switch-existing-workflows)).
- **An existing `smates.yml`** is left untouched; only the workflow (or patch) is
  generated from its suites. `--force` rewrites it.

Flags and variables of a Makefile `test` target become commented `command:` / `env:`
hints in `smates.yml`, as does the `env:` of the workflow steps running the tests. The
`go test` flags `-p`, `-tags`, `-race` and `-timeout` from either go into the suite's
`command:` itself. `smates init --format json` reports all of this, including the
items to review, for a script or a coding agent.

## Next steps

- [How smates decides](how-it-works.md): what makes a test run or skip, and why it errs
  on the side of running more.
- [GitHub Actions](../ci/github-actions.md): selective runs on pull requests, full runs on
  the default branch.
- [Git hooks and Claude Code](../guides/git-hooks.md): run the affected tests before every
  push.
- [Coding agents](../guides/coding-agents.md): let an agent run only the affected tests
  after each change.
- [Configure suites](../guides/configuration.md): adjust what `smates init` wrote.

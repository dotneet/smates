# smates

smates runs only the tests your change can affect, for TypeScript/JavaScript, Go,
Python and Java (Gradle), in CI and on your machine.

```text
$ smates run
smates: selected 7 of 412 test unit(s) across 3 suite(s) (98% skipped)
smates: web: running 5 test unit(s) with vitest
...
smates: done in 6.2s: ran 7 of 412 test unit(s), exit code 0
```

## Why

Most changes touch a small part of a repository, yet CI runs every test on every push.
You wait for minutes of tests that cannot fail, and a flaky test somewhere in the suite
fails your unrelated change and costs another full rerun.

smates records what each test actually uses, and on the next run skips every test whose
inputs have not changed since it last passed. Fewer tests run, so runs are shorter and
there are fewer unrelated tests to flake.

## How it works

<div class="grid" markdown>

<div class="card" markdown>

**1. Record what each test uses**

While your existing test command runs, smates traces the code each test executes, the
files it reads, the directories it lists and the processes it starts. A static import
graph is added as a safety net.

</div>

<div class="card" markdown>

**2. Hash every input**

Each input is stored with its content hash. Comparing by content needs no git history
and works on uncommitted changes, rebases and fresh CI checkouts.

</div>

<div class="card" markdown>

**3. Run only what changed**

A test is skipped only when it passed last time and every one of its inputs hashes the
same now. Everything else runs, with the reason (`smates run --explain`).

</div>

</div>

Changes that no test is known to depend on (a Dockerfile, a CI script) are not ignored:
smates looks for tests that name the file, can ask a small LLM agent that can only *add*
tests, and otherwise runs the affected suites in full. See
[How smates decides](getting-started/how-it-works.md).

## Key properties

- **One static binary** (about 5 MB, no runtime dependencies).
- **No code changes, no plugins.** Tracing hooks are injected through environment
  variables (`NODE_OPTIONS`, `PYTHONPATH`, `GOFLAGS`) only while smates runs your test
  command.
- **Sees what static analysis misses**: dynamic `import()`/`require()`, `importlib`,
  fixture and config files read at runtime, directories a test iterates over, scripts
  started as subprocesses, shared pytest fixtures.
- **Safe by construction**: failed tests always rerun, new tests always run, and every
  uncertainty resolves to running *more*.
- **Points at the cause of a failure**: each failing test comes with the changed files
  among its inputs ([failure hints](guides/failure-hints.md)).
- **Works in any CI**: a GitHub Action is included; elsewhere you use the same CLI and
  one state file.

## Supported runners

| Runner | Test unit |
|---|---|
| `vitest`, `jest`, `node --test`, `bun` | test file |
| `pytest` | test file |
| `unittest` | test module |
| `go` | package, or test function with `granularity: func` |
| `gradle` (Java, JUnit Platform) | test class file |
| `custom` (any command) | test file |

A *test unit* is what smates selects or skips as a whole. Tracing details per runner are
in [Languages and runners](guides/runners.md).

## Try it

```bash
curl -fsSL https://raw.githubusercontent.com/dotneet/smates/main/install.sh | sh
smates run --all     # in your repository: run everything once and record
# ... edit code ...
smates run           # run only what your change can affect
```

No configuration is needed to start: smates detects your suites. The
[Quick start](getting-started/quickstart.md) walks through it step by step.

## Where to go next

<div class="grid cards" markdown>

- **[Quick start](getting-started/quickstart.md)**

    ---

    Install smates, record once, and see a selective run in five minutes.

- **[How smates decides](getting-started/how-it-works.md)**

    ---

    The skip rule, selection reasons, and why it errs on the side of running more.

- **[GitHub Actions](ci/github-actions.md)**

    ---

    One step in your workflow, with the state cached per branch.

- **[Coding agents](guides/coding-agents.md)**

    ---

    Let Claude Code, Codex and others run only the affected tests after each change.

</div>

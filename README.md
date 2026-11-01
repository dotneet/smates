# smates

**Run only the tests your change can affect** — for TypeScript/JavaScript (vitest, jest,
`node --test`, bun), Go and Python (pytest, unittest, uv), in CI and locally.

**Documentation: <https://dotneet.github.io/smates/>** ([日本語](README.ja.md))

smates learns what every test *actually uses* by tracing real test runs (code executed,
files read, directories listed, child processes), adds a static import graph as a
safety net, and skips a test only when **nothing it depends on has changed since it
last passed**.

```text
$ smates run
smates: selected 7 of 412 test unit(s) across 3 suite(s) (98% skipped)
smates: web: running 5 test unit(s) with vitest
...
smates: done in 6.2s: ran 7 of 412 test unit(s), exit code 0
```

- **One static binary**, no runtime dependencies, no code changes, no plugins: tracing
  hooks are injected only while smates runs your existing test command.
- **Sees what static analysis cannot**: computed `import()`/`require()`, fixtures and
  config files read at runtime, directories a test iterates over, subprocesses.
- **Safe by construction**: content-hash fingerprints, failed and new tests always run,
  and every uncertainty resolves to running *more*.
- **Explains itself**: why each test was selected, and which changed inputs a failing
  test used.
- **Works anywhere**: a GitHub Action is included; any other CI uses the same CLI and a
  plain state file.

## Install

```bash
curl -fsSL https://raw.githubusercontent.com/dotneet/smates/main/install.sh | sh
```

Or download a [release](https://github.com/dotneet/smates/releases), or
`go install github.com/dotneet/smates/cmd/smates@latest`. See
[Install](https://dotneet.github.io/smates/getting-started/install/).

## Quick start

```bash
smates init          # detect suites and write smates.yml (--github: also a workflow)
smates run --all     # run everything once and record what each test uses
# ... edit code ...
smates run           # run only what your changes can affect
smates run --explain # ...and say why each test was selected
```

Continue with the [Quick start](https://dotneet.github.io/smates/getting-started/quickstart/)
and [How smates decides](https://dotneet.github.io/smates/getting-started/how-it-works/).

## Supported runners

| Language | Runners | Unit |
|---|---|---|
| TypeScript / JavaScript | vitest, jest, `node --test`, bun | test file |
| Python | pytest, unittest (uv, virtualenvs) | test file / module |
| Go | `go test` | package, or test function |
| anything else | `custom` command | test file |

## Documentation

| | |
|---|---|
| [Getting started](https://dotneet.github.io/smates/getting-started/quickstart/) | install, first run, the selection model |
| [Guides](https://dotneet.github.io/smates/guides/configuration/) | configuration, runners, orphans and the agent, git hooks, watch mode, worktrees |
| [CI](https://dotneet.github.io/smates/ci/github-actions/) | GitHub Actions, GitLab, CircleCI, Buildkite, Jenkins |
| [Coding agents](https://dotneet.github.io/smates/guides/coding-agents/) | using smates from Claude Code, Codex and others ([docs/AGENTS.md](docs/AGENTS.md), `smates guide`) |
| [Reference](https://dotneet.github.io/smates/reference/cli/) | commands, `smates.yml`, exit codes, limitations |
| [Design](docs/DESIGN.md) | how it works inside |

The site is built from [`docs/`](docs/) with MkDocs Material
(`uvx --with-requirements docs/requirements.txt mkdocs serve`).

## License

[MIT](LICENSE)

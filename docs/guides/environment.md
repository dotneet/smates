# Environment variables

A variable a test reads is a dependency like a file: when its value changes, the tests
that read it run again. This page covers what counts as a read, how values are
compared, and how to tune which variables are tracked.

```text
$ TZ=Asia/Tokyo smates run --explain
  web (vitest): 1/40
    + packages/web/src/dates.test.ts  [env changed: TZ]
```

```text
$ smates why '$TZ'
web	packages/web/src/dates.test.ts	(recorded)
```

In `smates deps <test>`, a variable shows as `$NAME`. Tracking is on by default.

## What counts as a read

| language | tracked | not tracked |
|---|---|---|
| Node.js | `process.env.X`, `"X" in process.env`, also through jest's per-file copy of `process.env` and vitest's `import.meta.env` | — |
| Bun | `process.env.X` | `Bun.env`, `import.meta.env` |
| Python | `os.environ["X"]`, `os.environ.get("X")`, `os.getenv("X")`, `"X" in os.environ` | — |
| Go | `os.Getenv`, `os.LookupEnv` (from the test log) | `syscall.Getenv` |

A lookup counts only when:

- **repository code is on the stack.** The runner's, reporter's and libraries' own
  bookkeeping lookups would otherwise make every test depend on them. A library the
  test calls (an SDK reading `AWS_REGION`) does count.
- **the test framework is not saving a value it is about to stub** (`vi.stubEnv`,
  `monkeypatch.setenv`).
- **the value is still the one the process started with.** A test that sets `TZ` and
  reads it back does not depend on the outer `TZ`.
- **it is not part of copying the whole environment** (`{...process.env}`,
  `dict(os.environ)`, `os.environ.copy()`, starting a child process). Children started
  with the copy record their own reads.

Reads made while a module is imported (once per process) count for every test that
depends on that module, like file reads.

## How values are compared

smates stores a hash of each value, never the value itself, so a state file does not
leak secrets. Unset and empty differ: setting a variable that a test probed selects it.

The value compared is the one the suite's tests see. A suite's `env:` in `smates.yml`
overrides the shell:

```yaml
  - name: web
    runner: vitest
    root: packages/web
    env: { TZ: UTC }
```

With this, `TZ=Asia/Tokyo smates run` selects nothing for `web`, because its tests
always see `UTC`. Changing the value in `env:` reruns the whole suite, since `env:` is
part of the suite's settings.

!!! warning "Guessable values"
    A hash protects a random token, but a guessable value (a weak password, a short
    enum) can be confirmed by whoever reads the state file. List such variables in
    `env_ignore`.

## Variables ignored by default

A state is usually recorded on CI and pulled to a laptop, where many variables differ
for reasons unrelated to the tests. These are ignored by default, so the first local
run does not select every test:

| category | examples |
|---|---|
| user, machine and shell | `PATH`, `HOME`, `USER`, `SHELL`, `PWD`, `TMPDIR`, `HOSTNAME`, `XDG_*`, `SSH_*`, `EDITOR`, Windows equivalents |
| locale | `LANG`, `LANGUAGE`, `LC_*` |
| terminal and colors | `TERM`, `COLUMNS`, `LINES`, `NO_COLOR`, `FORCE_COLOR`, `TMUX*`, `VSCODE_*` |
| CI systems | `CI`, `GITHUB_*`, `RUNNER_*`, `GITLAB_*`, `CI_*`, `CIRCLE*`, `BUILDKITE*`, `JENKINS_*`, … |
| proxies | `HTTP_PROXY`, `HTTPS_PROXY`, `NO_PROXY`, lower-case forms |
| toolchains and runner workers | `npm_*`, `NVM_*`, `VIRTUAL_ENV`, `CONDA_*`, `UV_*`, `GOPATH`, `GOCACHE`, `JEST_WORKER_ID`, `PYTEST_*`, … |
| set by smates to trace tests | `SMATES_*`, `NODE_OPTIONS`, `NODE_V8_COVERAGE`, `BUN_OPTIONS`, `PYTHONPATH`, `GOFLAGS`, … |

The full list is `DefaultEnvIgnore` in
[internal/config/config.go](https://github.com/dotneet/smates/blob/main/internal/config/config.go).
Names are matched case-sensitively, with `*` globs. What remains tracked are the
variables your project defines (`DATABASE_URL`, `APP_*`), `TZ`, `NODE_ENV` and the like.

## Tune tracking: `env_ignore` and `env_track`

```yaml
env_ignore: ["BUILD_*", DEPLOY_ID]   # never select tests when these change
env_track: [LANG]                    # track a variable the default list ignores
suites:
  - name: web
    runner: vitest
    env_ignore: [SENTRY_DSN]         # for this suite only
    env_track: [TERM]
```

- `env_ignore` adds names or globs to the default list. It applies when recording and
  when comparing, so adding a name stops it from selecting tests at once.
- `env_track` takes names out of the default list. It only matters for a variable that
  is read by name. It cannot override `env_ignore`: a name that `env_ignore` lists, at
  the top level or in the suite, stays ignored.
- Both work at the top level and per suite; a suite's lists add to the top-level ones.
- `env_ignore: ["*"]` turns environment tracking off.

??? note "Details: which edits rerun tests"
    A suite's `env:`, `env_ignore` and `env_track`, and the top-level `env_track`, are
    part of the suite's settings, so changing them reruns the suite. The top-level
    `env_ignore` is not: editing it reruns nothing.

    A variable that was ignored when a test ran was never recorded. Taking it out of the
    top-level `env_ignore` therefore selects nothing by itself; the tests that read it
    depend on it from their next run on. `smates run --all` records it for every test at
    once.

## Enumerating the environment is not tracked

Code that enumerates the environment and picks variables itself reads through the same
operations as a copy, so none of the variables it picks are recorded:

```ts
// not tracked: changing MYAPP_URL, or adding MYAPP_TOKEN, selects nothing
const config = Object.fromEntries(
  Object.entries(process.env).filter(([k]) => k.startsWith("MYAPP_")),
);
```

The same applies to `{k: v for k, v in os.environ.items() if k.startswith("MYAPP_")}`,
pydantic-settings and similar settings loaders. `env_track` does not help, and `inputs:`
and `rules:` take files, not variables. To make such settings count:

- read the variables by name in the code under test (`process.env.MYAPP_URL`,
  `os.environ["MYAPP_URL"]`);
- set them in the suite's `env:`, which reruns the suite when a value changes;
- load them from a file the tests read: a dotenv file is a file dependency, tracked or
  git-ignored;
- run `smates run --all` after changing the environment.

## Limitations

- **What the runtime consults itself is invisible**: `TZ` for date formatting, the
  locale, `GODEBUG`, and reads by native code. If your tests depend on the time zone,
  set `TZ` in the suite's `env:` so every machine runs them the same way.
- **Untraced commands**: variables that shell scripts or compiled binaries read are not
  seen.
- **Go `t.Setenv`** looks up the old value to restore it, so a test that sets a
  variable still depends on its outer value. It may run more often than needed, never
  less.
- **Bun**: `Bun.env` and `import.meta.env` are not traced; use `process.env`.

More in [Limitations](../reference/limitations.md).

## See also

- [Configure suites](configuration.md#set-environment-variables): the suite `env:`
- [Variables smates reads](../reference/environment-variables.md): smates' own settings
- [smates.yml reference](../reference/configuration.md)

# Watch mode

`smates watch` reruns only the tests a change can affect, every time you save. Use it
while you work, in any language and with any supported runner.

```text
$ smates watch
smates: watching ~/src/shop for changes (go, pytest); orphan policy: agent (...)

── 10:42:07 ── 1 changed: internal/store/testdata/orders.json
smates: selected 1 of 16 test unit(s) across 2 suite(s) (94% skipped)
    + internal/store  [changed: internal/store/testdata/orders.json]
ok  	example.com/shop/internal/store	0.170s
── ✓ PASS · 1 passed · 600ms
   watching for changes (a: run all · f: rerun failed · enter: rerun · q: quit)
```

A runner's own watch mode knows only that runner's imports. `smates watch` uses the same
traced dependencies as `smates run`, so it works for every runner (go test, pytest,
vitest, jest, node --test, bun, custom) and across languages. Editing
`testdata/orders.json` reruns the Go package whose test reads it. Editing a YAML file that
a pytest test loads reruns that test file.

## Keys

When stdin is a terminal:

| key | action |
|---|---|
| `a` | run every test |
| `f` | rerun the tests that have not passed |
| `Enter` | rerun the last selection |
| `q` | quit |
| Ctrl-C | stop the running tests (their whole process tree) and exit |

## How it works

- **Every cycle is a `smates run`**, recording included. The state stays fresh, and a
  failed test is retried until it passes. Watch starts with one run of the current
  selection.
- **Changes are found by polling `git status`.** There is no file system watcher and no
  extra dependency. `.gitignore` is respected, and git's own locks are never taken.
- **Bursts are debounced.** Atomic saves, formatters and checkouts become one run. Files
  saved while tests run start exactly one follow-up run.
- **Noise is filtered out.** smates' state, `node_modules`, Python and Vite caches,
  coverage, JUnit reports and editor temp files never start a run. A file that every run
  rewrites stops retriggering after three runs in a row.

## Options

```bash
smates watch --fail-fast -- -v
```

| flag | effect |
|---|---|
| `--interval 500ms` | how often to poll the working tree (default `500ms`) |
| `--debounce 300ms` | wait until no file changed for this long before running (default `300ms`) |
| `--fail-fast` | run each cycle with `smates run --fail-fast`: likeliest failures first, nothing more starts once one fails |
| `--agent` | ask the [LLM agent](agent.md) about orphans (see below) |
| `--clear` | clear the screen before each run |
| `-C`, `--config`, `--state`, `--suite`, `-v` | as for `smates run` |
| `-- <args>` | passed on to the runner |

## Orphans while watching

Orphans are changes no test is known to depend on (see
[Changes no test explains](orphans.md)). Watch applies the orphan policy only to files
changed while it watches.

The agent is not called unless you pass `--agent`, so a burst of saves never turns into a
burst of LLM calls. With `orphans: agent`, an orphan then runs the suites it can affect
in full, as with `smates run --no-agent`.

!!! tip "Large repositories"
    `git config core.fsmonitor true` and `git config core.untrackedCache true` make each
    poll cheap.

## See also

- [Quick start](../getting-started/quickstart.md)
- [Commands](../reference/cli.md)
- [Design: watch mode](../DESIGN.md#6-watch-mode)

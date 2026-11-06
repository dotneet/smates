# Limitations

What smates cannot see, and what to do about it. Most gaps have a workaround in
`smates.yml`; the rest are rare enough that a periodic full run (`smates run --all`, or
the default branch on CI) covers them.

## What tracing reaches

**A server in another process.** Tracing follows the test process and its children,
not a server the tests reach over the network (docker compose, a separately started
backend). A change to the server's code then selects none of those tests.
Workaround: declare the server's code with `depends_on` or `serves`. See
[Tests that talk to a server](../guides/servers.md).

**Data outside the repository.** What a test reads from databases or external services
is not tracked. Workaround: declare the files it depends on with `rules` or `inputs`,
or let such changes reach the agent as [orphans](../guides/orphans.md). For a schema
applied before the tests run, a rule with `reaches` gives the migrations to the tests
that import the database helper; a test that connects in a way the rule does not name
is then not run for a migration change (see
[Tests backed by a database](../guides/configuration.md#tests-backed-by-a-database)).

**Code that runs in a browser.** Tracing does not reach into a browser. A vitest test
that runs only in a browser (browser mode, a Storybook story run by Storybook's Vitest
plugin) leaves no trace, so after every run its outcome is unknown and it runs again
next time, whatever changed. With `trust_browser_passes: true` on the suite, a pass the
reporter gives is recorded instead, with the dependencies smates knows statically: the
test's import closure, what the Vitest configuration loads (setup files, plugins, alias
targets), and for a story the Storybook configuration directory, the import closure of
`preview.*` and the `staticDirs`. What the browser loads beyond that (a module reached
through an alias only Vite's configuration resolves, a fetched fixture, a CSS
`@import`) is missed: a story whose known dependencies include an import smates cannot
resolve is never trusted. Code a Playwright end-to-end test drives in a browser is
covered as [a server](../guides/servers.md).

**Commands smates cannot see into.** Shell scripts, `grep`, `cat` and similar commands
are analyzed from their command line. A command smates cannot see into (`make`, a
compiled binary) makes the test depend on everything below the directory it runs in,
so the test runs more often than needed. Paths such a command names outside that
directory are seen only when they appear in its arguments or option values; for a child
of a Go test, whose command line is not recorded, they are not seen at all
(`sh -c "cat ../../testdata/x"` from a package directory). A program a Go test deletes
before it ends (built into `t.TempDir()`) is not detected, and an executable file a Go
test only reads counts as a program start (extra dependencies, never fewer). See
[Child processes](../guides/runners.md#child-processes).

**Type-only imports.** In vitest and bun suites, an import TypeScript erases before the
code runs does not make a test depend on the module it names, so a change to a module
the tests only use for its types (`src/types.ts`) selects none of them, and is not an
orphan either. Erased imports are:

- `import type …` and `export type … from` declarations, always;
- `import { type A, type B } from` and `export { type A } from` (every specifier marked
  `type`), unless a `tsconfig.json` that may apply to the file sets
  `verbatimModuleSyntax`, `preserveValueImports` or `importsNotUsedAsValues`, extends a
  config smates cannot read, or cannot be read itself: those keep the import as
  `import {} from`, which loads the module.

`typeof import("./x")`, `import("./x").T`, a list mixing `type A` with a value, and
everything in `.js`, `.vue` and the like stay dependencies. smates keeps type-only
imports too whenever the run may check types: a suite command that does not run
`vitest` (or `bun test`) directly (`npm test`, a `&&` chain), any mention of
`typecheck`, `tsc` and similar in the command, the arguments after `--` or the suite
`env`, a `typecheck` setting (or those compiler options) in a vitest/vite config of the
suite or a file it imports, a vitest config smates cannot fully see, and `*.test-d.*` /
`*.spec-d.*` files in the suite. A config is fully seen when every config it names is
too: `test.projects` may list config files by literal path (no glob, no directory) and
inline projects with `extends: true`, but a workspace file, `workspace:`, any other
`extends:`, or projects inside `mergeConfig(...)` are not seen. It may import vitest,
vite, their official plugins, `@tailwindcss/vite`, Node's builtins, packages the static
analysis resolves inside the repository (whose files are then checked too), and any
package for its types only (`import type`), but not another package such as a shared
preset. Storybook's Vitest plugin is seen when it is `@storybook/addon-vitest` 10 or
later, its configuration directory can be told, and that directory's `main.*` sets no
`viteFinal` and no `typescript` options and names only Storybook's own addons (docs,
a11y, links, themes, onboarding, vitest) and the `react-vite`, `html-vite` or
`web-components-vite` framework; `STORYBOOK_COMPONENT_PATHS` in the suite `env` counts as
a mention. jest
(ts-jest reports type errors), `node --test` and custom suites always keep them. Tests
recorded by an earlier smates version keep the dependency until they are recorded again.
Type checking is not a test run: keep `tsc` (or your type checker) in CI.

## Environment variables

**Only reads by name are seen.** A variable is a dependency when code reads it through
`process.env` (not Bun's `Bun.env` or `import.meta.env` in Bun), `os.environ` or Go's
`os.Getenv`/`os.LookupEnv`. Invisible: what the runtime consults itself (`TZ` for
dates, the locale, `GODEBUG`), native code, `syscall.Getenv`, and commands smates
cannot trace (shell scripts, binaries).

**Enumerating the environment is not tracked**, even when the code then keeps only some
variables: `Object.entries(process.env).filter(...)`, a dict comprehension over
`os.environ`, pydantic-settings and similar settings loaders. Changing `MYAPP_URL` (or
adding a new `MYAPP_*`) then selects nothing. `env_track` does not help: it only
un-ignores a variable that is read by name. Workarounds:

- read the variables by name (`process.env.MYAPP_URL`, `os.environ["MYAPP_URL"]`);
- set them in the suite's `env:` (changing a value there reruns the suite);
- load them from a file the tests read (a dotenv file is a file dependency);
- run `smates run --all` after changing the environment.

**`t.Setenv` in Go** looks up the old value to restore it, so a test that sets a
variable still depends on its outer value. It may run more often than needed.

**Values are stored as SHA-256 hashes.** Whoever reads the state file can confirm a
guessable value (a weak password). Workaround: list such variables in `env_ignore`.

See [Environment variables](../guides/environment.md).

## Granularity

**The unit is the test file** (the package for Go, or the test function with
`granularity: func`). A change anywhere in a module reruns every test that loaded it.
For Python, data a module reads at import time or behind a cache belongs to that
module.

**Go dependency files count as a whole.** Any change to a `go.mod` or `go.sum` selects
every test of its module and of the modules importing it; in a `go.work` workspace, a
change to `go.work`, `go.work.sum` or the `go.mod`/`go.sum` of any module it uses
selects every test of the workspace, even when the bumped dependency is linked by few
of them. See [Go](../guides/runners.md#go).

**Go packages without tests** are not compiled by a selective run: when a library
changes, the tests importing it run, but a `main` package importing it is not built, and
a change to such a package itself is [untested code](../guides/orphans.md).
Workaround: widen the suite's `tests` to their files, see
[Go packages without tests](../guides/runners.md#go-packages-without-tests).

**Go test functions** (`granularity: func`):

- Recording costs one process per test function, so the default branch's full runs get
  slower. Selective runs only start the selected functions.
- Functions share no process while recording. A test that relies on in-memory state
  another test of its package left behind behaves differently than under `go test`.
- Code that matters without running (an unexecuted method that satisfies an interface)
  is covered through declarations. Code executed through reflection alone is not.
- Declarations of the test's own package are followed by name, without type
  information. A key in a composite literal (`{name: ...}`) counts as a use of a
  declaration of that name (it may be a constant used as a map key): more runs, never
  fewer. A call in a package-level variable's initializer that may run code (anything
  but a conversion to a type literal, a predeclared type or a type of the same file,
  `(*T)(nil)`, a builtin such as `len`, `errors.New`, `fmt.Errorf`, `time.FixedZone`,
  or `regexp.MustCompile` of a literal pattern that compiles) makes the variable a
  dependency of every test.
- A helper every test calls, or a type every test reaches (a server struct), is a
  dependency of every test: an edit inside the helper, or to the type's fields or
  method set, selects them all.
- Declarations of other packages count per file: a changed constant, type or signature
  in an imported package selects every function of the packages importing it.

See [Go test functions](../guides/go-test-functions.md).

## Runners

**Runner arguments may filter tests.** Arguments passed through to the runner
(`smates run -- -t name`) may skip tests, so such runs never record a test as passed.
Workaround: put permanent flags in the suite's `command`.

**A skipped test counts as passed, except for Go test functions.** A Go test function
recorded in its own process (`granularity: func`) that skips itself is recorded as not
proven and runs again (see [Skipped tests](../guides/go-test-functions.md#skipped-tests)).
Everywhere else a skip is a pass: a Go package (`granularity: package`, or a package
recorded as a whole) whose tests all skip, a pytest `skip`/`xfail`, a vitest or jest file
whose tests all skip. Such a test is not run again until its dependencies change, unless
the gate is an environment variable it reads (a dependency) or a file it probes.
Workaround: list it in `volatile`, or set `max_age`.

**Time is not tracked.** A test whose outcome depends on the date or the clock is not
selected when only the time changes. Workaround: list it in `volatile` (it runs every
time), or set `max_age` so recorded runs expire.

**Jest** occasionally runs two files back to back in one worker even when recycling
workers. Their dependencies are then merged: still correct, but less precise.
Workaround: `isolate: true` makes it exact, at the cost of one jest start per file
while recording.

**Runner caches** such as Vitest's `fsModuleCache` are outside smates' view. A cache
restored in CI can hide failures that a cold run would show, with or without smates.

**Python before 3.12** is traced with `sys.setprofile`, which slows tests noticeably
while recording. Python 3.12 and later use `sys.monitoring`, with low overhead. A
profiler installed later with `sys.setprofile` (before 3.12) replaces smates' tracer.

**coverage.py's `sysmon` core.** smates re-enables monitoring events at every test
switch, which also re-arms coverage.py's. Its results are unchanged, but it does some
work again per test.

**`NODE_V8_COVERAGE` set inside the command.** A command that sets it itself
(`env NODE_V8_COVERAGE=... node`, or a script that exports it) hides the code the
processes below it execute; only the static graph covers that code then. Workaround:
set it in the environment or in the suite's `env:` instead.

**c8** writes no `coverage/tmp` while smates records, so a later `c8 report` has
nothing to read. Workaround: let c8 report in the same command.

See [Languages and runners](../guides/runners.md).

## Files and file systems

**Gaining the executable bit alone** (`chmod +x` without a content change) selects
nothing: a state written by an older smates recorded every file without the bit, and
the two cannot be told apart. Losing it (`chmod -x`) is a change once a state written
by the current version recorded the bit; against an older smates' state it goes
unnoticed until the next recording.

**Symlinked modules.** A module Node or Bun loads through a symlink is traced by its
target only (the runtime resolves the link first), so retargeting such a link may be
an orphan. Files read through a link (`fs`, `open`, Go's `os.Open`) depend on the link
as well.

**Case-insensitive file systems.** An import whose case differs from the file on disk
is recorded under the imported spelling and may not match the file's changes.

**Git-ignored inputs edited during a run.** A git-ignored file that you edit by hand
while the tests are running is recorded with its new content, so the edit selects
nothing. See [Design](../DESIGN.md).

## Platforms

**Windows.** The CLI builds and runs there, and the GitHub Action runs on
`windows-latest`, but tracing has only been verified on Linux and macOS.

## See also

- [Troubleshooting](troubleshooting.md)
- [How smates decides](../getting-started/how-it-works.md)
- [Shadow mode](../guides/trust.md#shadow-mode): measure what the selection would miss

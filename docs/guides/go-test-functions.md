# Go test functions

With `granularity: func`, smates selects individual Go test functions instead of whole
packages. Use it when one package holds most of the tests and most changes reach it, so
selecting packages saves little.

```yaml
suites:
  - name: api
    runner: go
    granularity: func
```

A unit is then a top-level test function, `internal/api#TestCreateUser`, and a body
edit selects only the functions that executed it:

```text
$ smates run --explain
smates: selected 2 of 3 test unit(s) across 1 suite(s) (33% skipped)
  api (go): 2/3
    + internal/api#TestPlaceOrder  [changed: internal/api/order.go]
    + internal/api#(build)  [changed: internal/api/order.go]
```

`internal/api#(build)` is the package's compile check, explained
[below](#compile-checks). The default, `granularity: package`, keeps one unit per
package.

## What counts as a test function

Every top-level `TestXxx(*testing.T)`, `FuzzXxx(*testing.F)` (with its seed corpus) and
`ExampleXxx()` in the package's `_test.go` files. Subtests run with their parent. A
test file that does not parse makes the package itself a unit, which runs, and fails,
as a whole.

A function declared only in a test file the suite's build does not compile (a
`//go:build linux` file on macOS, a tag neither `GOFLAGS` nor the command passes) is
not a unit there.

## What a function depends on

While recording, smates builds each package's test binary once with coverage over the
repository packages it links (`go test -c -cover -coverpkg=...`, with the build flags
of the suite command such as `-tags` and `-race`). It then runs every function in its
own process (`-test.run '^TestX$'`, with a coverage profile and a test log). A function
depends on:

- **Files it executed.** Every Go file in which any of its blocks ran. Package
  initialization runs in every process, so it counts for every function.
- **Declarations it may reach.** For every other Go file of the import closure, the
  file without its function bodies. A changed constant, variable initializer, type,
  signature or method selects every function that could see it. An edit inside a body
  selects only the functions that executed it.
- **Test files.** Its own `_test.go` file, the test files declaring helpers it names,
  and test files with `TestMain`, `init` or package-level variables initialized by
  calls.
- **Everything a package unit has.** Files and directories its test log recorded,
  environment variables it read, embedded and other non-Go files, `go.mod`/`go.sum`, and
  what its child processes used. Go packages it builds or runs as a subprocess
  (`go run ../cmd/tool`) count in full.
- **The package directory listing.** A file added to the package selects every
  function of the package.

The unit of attribution is the file: a function that executed one line of `user.go`
depends on all of `user.go`. Declarations are compared as tokens, so reformatting does
not count.

```text
$ smates deps 'internal/api#TestPlaceOrder'
go.mod	static,recorded
internal/api/	static,recorded
internal/api/order.go	recorded
internal/api/order.go#decls	static,recorded
internal/api/user.go#decls	static,recorded
internal/api/user_test.go	static,recorded
smates.yml	static,recorded
```

## Re-executions of the test binary

The helper-process pattern runs part of a test in a child of the same binary:

```go
cmd := exec.Command(os.Args[0], "-test.run=^TestHelperProcess$")
cmd.Env = append(os.Environ(), "WANT_HELPER=1")
```

Code that such a child runs counts for the test that started it. While recording,
smates adds a small test file to the package through `-overlay` (merged with an
`-overlay` of your own; nothing is written into the package). It hands every
re-execution the function's coverage directory, so the child's coverage joins the
parent's.

A child that writes no coverage (it panics or is killed, so no exit hook runs) makes
the function depend on every Go file it could reach. The files the child reads are
recorded through its own test log; a child that ends in `os.Exit` loses that log, and
the function then depends on every file of its Go module (return from the helper to
keep it precise). A child started with an environment of its own (`cmd.Env` without
`os.Environ()`) or from a copy of the binary is not seen. Other children of Go tests
are described in [Child processes of Go tests](runners.md#child-processes-of-go-tests).

## Compile checks

`go test` fails a package that does not compile, whichever bodies its tests execute.
An edit inside a body that no test runs (an unused helper that no longer type-checks, an
unused import, a vet failure) would otherwise select nothing. So every package with test
functions also has a compile check, the unit `internal/api#(build)`:

- It depends on the content of the package's whole static closure: its Go and test
  files, those of the in-repository packages it imports, embedded files and `go.mod`.
- It runs `go test -run '^$'` with the test binaries built and vetted but not executed
  (smates is their `-exec` program; `TestMain` does not run). It uses the build flags
  and `-vet` setting of the suite command.
- A package that does not build fails its check with the compiler's output.

```text
$ smates run --explain
smates: selected 1 of 3 test unit(s) across 1 suite(s) (67% skipped)
  api (go): 1/3
    + internal/api#(build)  [changed: internal/api/legacy.go]
smates: api: running 1 test unit(s) with go
── smates: compile check of ./internal/api (exit 1, 75ms)
# example.com/shop/internal/api [example.com/shop/internal/api.test]
internal/api/legacy.go:4:9: cannot use "1" (untyped string constant) as int value in return statement
FAIL	example.com/shop/internal/api [build failed]
```

An ordinary body edit thus selects the functions that executed it plus these cheap
checks, for the changed package and the packages importing it. The check carries the
build failure; the functions keep their recorded outcomes, so after a fix only the
checks run again.

## How selected functions run

When not recording, selected functions run with one `go test` per group of packages:

```bash
go test -run '^(TestA|TestB)$' ./internal/api ./internal/store
```

The pattern covers as many packages as fit in 16 KiB; a function of the same name in
another of those packages runs too, and a package that alone needs more runs whole.
Outcomes come from go test's `--- FAIL`/`--- PASS` lines. When recording, each function
runs in its own process instead, as described above. `smates run --dry-run` prints the
commands.

## When smates falls back to the whole package

Some packages cannot be recorded per function:

- the test binary does not build with coverage, or `go list` fails;
- the suite command is not a plain `go test`;
- the command (or `GOFLAGS`, or arguments after `smates run --`) passes `-run`, `-skip`,
  `-bench`, `-fuzz`, `-list`, `-c` or `-o`.

Such a package is recorded as a whole and says so:

```text
smates: api: internal/api: recorded at package granularity (the suite command passes -run): each of its test functions depends on everything the package does
```

Each of its functions then depends on every Go file of the package's closure: package
granularity under function names. Selection stays correct, only coarser.

## What recording costs

Recording runs one process per test function, plus one coverage build per package. The
full runs on your default branch get slower; selective runs only start the selected
functions.

- Functions of one package run one at a time, as `go test` runs them. `record_jobs`
  allows more at once (`record_jobs: 4`) when the tests do not share state.
- Packages run as many at a time as `-p` allows (in the command or `GOFLAGS`; default
  GOMAXPROCS). A suite that runs `go test -p 1` because its packages share a database
  keeps them apart while recording too.

Functions share no process while recording. A test that relies on state another test
of its package left behind in memory behaves differently than under `go test`.

## Test flags

Test flags such as `-timeout`, `-short`, `-count`, `-parallel` or `-v`, in the command
or in `GOFLAGS`, reach every function's process. Those of the command win over
`GOFLAGS`. Without a `-timeout`, go test's default of 10 minutes applies per process.

```yaml
  - name: api
    runner: go
    granularity: func
    command: "go test -race -tags integration -timeout 2m"
    record_jobs: 4
```

## See also

- [Languages and runners](runners.md#go): package granularity
- [Configure suites](configuration.md)
- [Limitations](../reference/limitations.md)
- [Design, section 2](../DESIGN.md#2-tracing-per-ecosystem): the internals

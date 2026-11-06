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

- **Function bodies it executed.** Every function or method in which any of its
  blocks ran. Package initialization runs in every process, so the `init` functions it
  runs count for every function.
- **Declarations of its package it names.** From its own code, the test-file
  declarations it reaches and the bodies it executed, smates follows names to the
  types, constants, variables and function signatures of the package, without their
  bodies (a body it did not run cannot matter), and from a type to its fields' types.
  A changed constant, struct field or signature selects the functions that name it,
  directly or through what they reach; a new declaration nothing names selects none.
- **Test-file declarations it reaches.** Coverage does not instrument test files, so
  smates follows names instead: from the test function's own declaration, and from
  those that run for every test (`TestMain`, `init`, package-level variables
  initialized by a function call), to the helpers, variables, constants and types they
  mention, and from a type to its methods. Names are resolved as Go resolves them: a
  local variable, a parameter, a field or a method called `x.Name` does not reach a
  helper that happens to be called `Name`. An external test package (`pkg_test`)
  reaches the package's own declarations it uses as `pkg.Helper` (or as `Helper`,
  with a dot import of the package). Methods that test files add to a type of the
  package itself count for every function (they may change its method set).
- **The method set of every type it reaches.** Adding, removing or changing the
  signature of a method of such a type selects it (the type may satisfy an interface,
  or a `String` method change what is printed), wherever the method is declared.
- **The package's shape.** What runs for every test of the binary or changes what the
  package means: `init` functions and `TestMain` (that there are), package-level
  variables whose initializer calls code, blank imports, declarations that shadow a
  predeclared name, and files compiled into the package that are not Go (assembly,
  cgo sources). Adding such a declaration or file selects every function; adding an
  ordinary function, type, constant, test or data file selects none until something
  uses it (the [compile check](#compile-checks) still runs).
- **Declarations of the other packages it imports.** For every Go file of the import
  closure outside its package, the file without its function bodies: a changed
  constant, type or signature there selects every function of the importing packages.
  An edit inside a body selects only the functions that executed it.
- **Everything a package unit has.** Files and directories its test log recorded,
  environment variables it read, embedded and other non-Go files, `go.mod`/`go.sum`, and
  what its child processes used. Go packages it builds or runs as a subprocess
  (`go run ../cmd/tool`) count in full.

The unit of attribution is the top-level declaration: a function that executed one
line of `PlaceOrder` depends on the body of `PlaceOrder`, not on the other functions of
`order.go`. Declarations are compared as tokens, with the imports of their file (they
decide what the names mean): reformatting does not count, and neither do comments,
except directives (`//go:embed`, `//go:build`, ...), the output comment of an example
and every comment of a file that uses cgo.

```text
$ smates deps 'internal/api#TestPlaceOrder'
go.mod	static,recorded
internal/api/#methods:Order	static,recorded
internal/api/#methods:Server	static,recorded
internal/api/#shape	static,recorded
internal/api/helpers_test.go#decl:newServer	static,recorded
internal/api/order.go#decl:PlaceOrder	recorded
internal/api/order.go#sig:MaxItems	recorded
internal/api/order.go#sig:Order	static,recorded
internal/api/order_test.go#decl:TestPlaceOrder	static,recorded
internal/api/server.go#sig:Server	static,recorded
internal/store/store.go#decls	static,recorded
smates.yml	static,recorded
```

A function's dependencies, in the test files as elsewhere, are what it can reach, not
what it needs: a helper every test calls (a shared server or fixture constructor) is a
dependency of every test, and an edit inside it selects them all.

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

A package without tests has no check unless the suite's `tests` globs reach its files:
see [Go packages without tests](runners.md#go-packages-without-tests).

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

Listing the functions does not run the test binary: smates reads them from the test
files `go list` says the configuration compiles, so a `TestMain` that does work for the
whole suite does not run for it. (Only when that fails does smates ask the binary,
`-test.list`; a non-zero exit there falls back too, with the binary's last line of
output in the message.)

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

`TestMain` runs in every function's process, and with `-test.run` set: when selected
functions run, it runs once for them. A `TestMain` that checks something about the
whole suite after `m.Run()` (that every route was exercised, that no golden file went
unused) should skip that check when `-test.run` or `-test.skip` restricts the run:

```go
func TestMain(m *testing.M) {
	code := m.Run()
	if f := flag.Lookup("test.run"); code == 0 && f != nil && f.Value.String() == "" {
		code = auditWholeSuite()
	}
	os.Exit(code)
}
```

## Skipped tests

A test that skips itself did not fail, but it did not prove anything either: a test
that skips when a database is unreachable would otherwise count as passed on a laptop
without one, and not run again until its code changes. So while recording, smates runs
each function's process with `-test.v` (unless the command already sets it) and reads
the function's result line:

- `--- PASS` records a pass;
- `--- SKIP` at the top level, in any iteration of `-count`, records `skip`;
- no `=== RUN` line for the function records `skip` too: a `TestMain` that exits 0
  before `m.Run()` ran nothing;
- output that says neither records `unknown`.

A skipped function is selected again on the next run (reason `unproven`, "skipped last
run: not proven to pass"), as an unknown one is (reason `failed`), until it passes. A subtest that skips under a
passing function is a pass. The run itself passes, and its outcome line counts them:

```text
smates: PASSED 12 of 12 selected test unit(s) (240 in total) in 3.1s; 2 skipped (not proven: they run again next time)
```

A test that always skips (a `-short` gate, an integration test whose service is never
up) therefore runs every time; it is cheap, and `smates doctor` lists the units that
skipped in their last run (`state-skipped`). A skip only counts like this per
function. A package recorded as a whole under `granularity: func` records each
function's skip too when go test prints it (`-v` or `-json` in the suite's command),
and the package's result otherwise; a `granularity: package` suite always records the
package's result (see [Limitations](../reference/limitations.md)).

Tests whose outcome depends on something smates does not track, such as the date,
can be listed in `volatile` (they run every time), and `max_age` makes every recorded
run expire after a while:

```yaml
  - name: api
    runner: go
    granularity: func
    volatile: ["internal/clock#TestToday"]   # unit, package or test file globs
    max_age: 168h
```

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

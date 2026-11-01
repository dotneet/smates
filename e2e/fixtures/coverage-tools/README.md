# coverage-tools fixture

The same small code base tested with the coverage tools people run next to their test
runners, one suite each (see `smates.yml`): vitest with `--coverage` (v8 provider), c8
around `node --test`, `node --test --experimental-test-coverage` with a user-set
`NODE_V8_COVERAGE`, pytest-cov on two pytest-xdist workers, pytest-cov with
`COVERAGE_CORE=sysmon`, and `go test -cover -coverpkg=./... -coverprofile`.

`src/load.js` and `py/src/calc/plugins.py` load a module by a computed name, so only the
code smates traces at run time links the plugin tests to it. `e2e/coverage_test.go`
checks that every tool still writes its report while smates records.

# flaky fixture

`test/coin.test.js` fails while the file named by `$SMATES_E2E_COIN` (outside the
repository, so invisible to tracing) starts with `fail`; `fail-once` makes it fail a
single time. `e2e/flaky_test.go` uses it to flip the outcome without changing any input.

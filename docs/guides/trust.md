# Shadow mode, flaky tests and stats

Before you rely on the selection, you want evidence that it works in your repository.
This page covers three tools for that: shadow mode checks the selection against full runs,
flaky detection finds tests that flip without cause, and `smates stats` sums up what
smates saved.

## Shadow mode

```text
$ smates run --shadow
smates: selected 7 of 412 test unit(s) across 3 suite(s) (98% skipped)
...
smates: shadow: every failing test was selected (7/412 would have run)
```

`smates run --shadow` runs **every** test, and evaluates the selection on the side. It then
reports any failing test that the selection would have skipped (an *escape*):

```text
smates: SHADOW: 1 failing test(s) would NOT have been selected:
  - web: src/report.test.ts
```

An escape means a dependency smates cannot see, such as a server's code, a network
service, or an environment variable it ignores. Declare it (`depends_on`, `rules`,
`inputs`; see [Tests that talk to a server](servers.md)) and the next selection includes
the test.

### Cheaper: `--shadow --no-record`

`smates run --shadow --no-record` skips tracing, which is much cheaper, and leaves the
state as it was. Escapes are still found for runners that report per-file status without
tracing: vitest, jest, pytest, `go test`, and any runner with a `junit:` report. With
other runners, the unselected tests of a failing run are reported as *possible* escapes.

### Recommended rollout

1. In a new repository, replace the full test run in CI with `smates run --shadow`. It
   still runs every test, so you lose nothing.
2. Watch the log and `smates stats` for misses. Fix each one with a declaration.
3. Once misses stay at zero, switch pull requests to plain `smates run`.
4. Keep a nightly `--shadow` job permanently, to catch new blind spots.

??? note "Details: shadow mode and other flags"
    Shadow runs never reuse cached `go test` results (`-count=1`), because go does not
    recheck files outside the module before reusing a result. `--shadow` cannot be
    combined with `--fail-fast`.

## Flaky tests

smates detects flaky tests by proof, not by heuristics. It knows the content hash of
every input of every test. When a test's outcome flips between pass and fail while none
of those inputs changed, the test is flaky, or it depends on something smates cannot see
(time, network, environment variables it ignores, files outside the repository).

Typical evidence:

- a failing test passes when re-run;
- a full run on the default branch fails a test that nothing touched.

```text
$ smates flaky
SUITE  UNIT                FLIPS  LAST FLIP   LAST SEEN             COMMIT   RECENT (oldest first)       ENV READ
web    src/clock.test.ts   3      fail->pass  2026-10-30T09:12:44Z  3f2a1c9  pass fail pass pass fail pass  TZ
```

- `smates run` reports flaky tests in its log, in `--report` (`flaky`), in the job
  summary, and in the action's `flaky` / `flaky-tests` outputs.
- The state keeps the evidence: number of flips, last commit, and recent outcomes.
  `smates flaky` lists it, together with the environment variables each test reads (they
  were the same across its flips).
- `smates flaky --format json` gives the same as JSON.
- `smates flaky --clear [test...]` forgets the evidence of the given tests, or of all
  tests when none is given. Use it after fixing a flaky test.

**Detection never hides a failure.** A flaky failure still fails the run, and the test is
still selected next time.

### Retry known flaky tests

```yaml
flaky:
  retry: 1   # default 0: never hide a failure
```

When every failure of a suite run is a test *already known* to be flaky, smates repeats
the suite run, up to `retry` times (at most 5), with the same tests. If it then passes,
the run does not fail, and the flip is reported.

The retry covers the same tests as the failed run, not only the failing ones. A run can
also fail for reasons no single test reports (an unhandled error, a teardown, a crash),
and re-running only the flaky tests would hide that.

## Stats { #stats }

`smates stats` sums up the runs recorded in the state:

```text
$ smates stats --since 30d
smates: 214 runs since 2026-10-02 00:00, 2026-10-02 to 2026-11-01
  state: .smates/state.json.gz

Runs
  total:         214, 9 failed (exit code ≠ 0)
...
Selection
  test units:    ran 1840 of 88168 (98% skipped)
  time saved:    about 41h of recorded test time skipped
...
```

What it counts:

| section | contents |
|---|---|
| Runs | runs by mode (selective, full, shadow) and source (CI, local, watch) |
| Selection | test units run out of the total (the skip rate); the recorded time of the skipped units (the time saved), also extrapolated to 30 days |
| Shadow mode | shadow runs, misses and the miss rate |
| Orphans | orphans by how they were handled (reference scan, untested code, agent, full suite run, skipped by caller); agent calls with tokens, and their cost when the backend reports one (e.g. `claude -p`) |
| Flaky tests | flips detected, and tests currently marked flaky |
| Recent runs | the last few runs, one line each |

| flag | effect |
|---|---|
| `--since 30d` | only runs since then: an age (`30d`, `2w`, `12h`), a date (`2026-01-02`) or an RFC 3339 time |
| `--branch NAME` | only runs on that branch |
| `--ci` | only runs on CI |
| `--recent N` | list the last N runs (default 5) |
| `--format json` | machine-readable output |

### Where the numbers come from

- Every `smates run` adds a short record, except with `--no-record` or `--dry-run`.
- The state keeps the last 1000 records within 180 days.
- `smates state merge` unions records, so the stats of a state restored on CI cover all
  the runs that state went through.
- A sharded run leaves one record per shard. Its orphans are counted by shard 1 only.

The GitHub Actions job summary shows the cumulative time saved in one line.

## See also

- [GitHub Actions](../ci/github-actions.md): nightly shadow jobs, the `flaky` outputs
- [Failure hints](failure-hints.md): why a test failed
- [Commands](../reference/cli.md#smates-flaky): every `flaky` and `stats` flag
- [Limitations](../reference/limitations.md): what smates cannot see

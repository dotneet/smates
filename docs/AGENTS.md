# Using smates as a coding agent

smates runs only the tests your changes can affect. `smates guide` prints this file.

## After every change

```sh
smates run --output failures      # or set SMATES_OUTPUT=agent once
```

It prints the output of failing tests only, then a short summary. Read the summary:

```
smates: FAILED 1 of 7 test unit(s) in 3.2s:
  web: src/cart.test.ts
    changed inputs: src/price.ts (modified)
smates: rerun them: smates run --output failures --only web:src/cart.test.ts
```

A passing summary ending in `; N skipped (not proven: they run again next time)` means
N selected tests skipped themselves (a service that is not running, an environment
gate) and did not check your change: say so when you report, or start what they need.

Fix the code, rerun the printed `--only` command until it passes, then finish with a
plain `smates run --output failures` (it reruns what failed plus anything newly
affected). Do not run the whole test suite yourself: smates already picked what can break.

## Exit codes

| code | meaning | what to do |
|---|---|---|
| 0 | passed (or nothing to run) | done |
| 1 | tests failed | fix the code (or the test) |
| 2 | usage error (also an `--add` that names no test) | fix the command line |
| 3 | configuration error | fix `smates.yml` (`smates config ...`), or the `--suite`/`--only` name (`smates list`) |
| 4 | environment problem | see `smates doctor --format json`: each failed check has a `fix` |

Exit 4 also covers a runner that could not start (not installed, or exit 126/127 with no
test result in a recorded run of any runner but `custom`; the other suites still run and
are recorded, its tests stay selected) and a state that could not be saved (e.g. the
shared state's lock is held); the summary is printed first. When tests also failed, the
exit code is 1. `exit_code` in `--report` is 4 in these cases too (when tests failed, it
is the runner's own exit code). So is a suite whose `requires` (a database, a browser)
are not met here: the log names the check; its tests stay selected for a run where it is.

## Changes no test is known to depend on (orphans)

By default such files make whole suites run (or ask an LLM). You know what you changed,
so you can decide instead:

```sh
smates select --format json       # read "orphan_candidates": path, referenced_by, suites, handling
smates run --output failures --orphans=skip --add api:tests/test_users.py
```

`--add SUITE:UNIT` (repeatable; the unit may be a test file path) adds tests;
`--orphans=agent|full|skip` overrides the `orphans:` policy for one run. You can only add
tests, never remove selected ones. Use `--orphans=skip` only when you are sure the
orphans cannot break a test. Neither combines with `--only`. Files that should never
need tests belong in `ignore:`: `smates config add-ignore 'deploy/**'` (`--dry-run` first).

## Understanding a selection

- `smates select` (or `smates run --explain`): what would run and why.
- `smates why <file>`: the tests that depend on a file.
- `smates deps <test file or Go package dir>`: the files a test depends on.
- `smates list`: suites and test units (the names `--only` and `--suite` take); its
  `tests=` column says where the test patterns come from (e.g. `jest.config.js testMatch`).
  `smates list -v` prints the patterns and every unit: a new test file missing there is
  not seen as a test (fix the runner config or `tests:`; `smates doctor` warns too).

`why`, `deps`, `list`, `doctor` and `init` take `--format json`.

## Long runs

- In failures mode smates prints `smates: still running <suite> (...)` after every 30s
  without output, so a tool timeout does not mistake a long run for a hang
  (`SMATES_HEARTBEAT=10s` changes the interval, `0` turns it off).
- When the recorded durations of the selected tests add up to more than `--budget`
  (default 90s), smates prints a hint before starting. Then either run one suite at a
  time (`--suite <name>`), or start the run in the background and read its report
  afterwards: `smates run --output failures --report smates-report.json` (per-test
  results and `exit_code`).
- `--max-time 5m` starts no test batch that would end after 5 minutes; the tests left
  out stay selected and run in a later run, in the usual order (not necessarily first;
  CI runs them all).

## Do not

- edit `.smates/` (the recorded state) or pass runner filters after `--` to skip tests;
- treat exit code 3 or 4 as a test failure: fix the setup, then run again.

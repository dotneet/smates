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

Fix the code, rerun the printed `--only` command until it passes, then finish with a
plain `smates run --output failures` (it reruns what failed plus anything newly
affected). Do not run the whole test suite yourself: smates already picked what can break.

## Exit codes

| code | meaning | what to do |
|---|---|---|
| 0 | passed (or nothing to run) | done |
| 1 | tests failed | fix the code (or the test) |
| 2 | usage error | fix the command line |
| 3 | configuration error | fix `smates.yml`, or the `--suite`/`--only` name (`smates list`) |
| 4 | environment problem | install the runner or `node_modules`; see `smates doctor` |

## Changes no test is known to depend on (orphans)

By default such files make whole suites run (or ask an LLM). You know what you changed,
so you can decide instead:

```sh
smates select --format json       # read "orphan_candidates": path, referenced_by, suites
smates run --output failures --orphans=skip --add api:tests/test_users.py
```

`--add SUITE:UNIT` (repeatable) adds tests; `--orphans=agent|full|skip` overrides the
`orphans:` policy for one run. You can only add tests, never remove selected ones. Use
`--orphans=skip` only when you are sure the orphans cannot break a test.

## Understanding a selection

- `smates select` (or `smates run --explain`): what would run and why.
- `smates why <file>`: the tests that depend on a file.
- `smates deps <test file or Go package dir>`: the files a test depends on.
- `smates list`: suites and test units (the names `--only` and `--suite` take).

## Long runs

- In failures mode smates prints `smates: still running <suite> (...)` after every 30s
  without output, so a tool timeout does not mistake a long run for a hang
  (`SMATES_HEARTBEAT=10s` changes the interval, `0` turns it off).
- When the recorded durations of the selected tests add up to more than `--budget`
  (default 90s), smates prints a hint before starting. Then either run one suite at a
  time (`--suite <name>`), or start the run in the background and read its report
  afterwards: `smates run --output failures --report smates-report.json` (per-test
  results and `exit_code`).

## Do not

- edit `.smates/` (the recorded state) or pass runner filters after `--` to skip tests;
- treat exit code 3 or 4 as a test failure: the tests did not run, so fix the setup.

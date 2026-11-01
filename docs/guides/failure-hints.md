# Failure hints

When a test fails, smates already knows every file it used and which of them changed. So
for each failing test, it lists **the changed files among its inputs**. That is usually a
handful of files, often just one.

```text
smates: failure hints for 1 failing test unit(s):
  web: src/cart.test.ts — 2 of its 37 known inputs changed since its last recorded run (3f2a1c9, passed)
      src/price.ts  (source, direct import, modified)
      fixtures/prices.json  (data, traced, modified)
      $TZ  (environment variable, read in its last run, changed)
    → agent (high confidence): src/price.ts:12 — roundCents now truncates instead of rounding, so the total is off by one cent
```

Look at the first candidate first. The last line is the optional
[agent explanation](#agent-explanations).

## How candidates are found

"Changed" is measured against the content recorded the last time *that test* ran. A test
with no record is compared with the files this run's selection found changed.

Candidates are ranked by kind:

1. code the test imports directly;
2. other code it uses;
3. the test file itself, and environment variables it read;
4. data;
5. configuration (suite inputs, manifests, lockfiles);
6. directory listings.

Within a kind, the most recently modified file comes first.

## When none of its inputs changed

The hint says so. This points at a flaky test or the environment (time, network,
ordering, or something tracing cannot see). If the test did not pass last time either,
the cause predates that run.

Changed files no test is known to depend on (orphans, see
[Changes no test explains](orphans.md)) are listed instead when there are any: they are
exactly the changes smates cannot attribute. To check for flakiness, see
[Flaky tests](trust.md#flaky-tests).

## Where hints appear

- the run log;
- the step summary (`--summary`, or `$GITHUB_STEP_SUMMARY` on GitHub Actions);
- the JSON report (`--report`), as `failure_hints`. For vitest, jest, pytest, `go test`
  and JUnit reports, this includes the runner's failure output;
- the short summary of `smates run --output failures`, as `changed inputs:` (see
  [Use smates from a coding agent](coding-agents.md)).

Runs with `--no-record` have hints too, for the runners that report per-file status
without tracing (vitest, jest, pytest, `go test`, JUnit). Other runners give no per-test
results without recording.

## Agent explanations { #agent-explanations }

With the [LLM agent](agent.md) available, smates adds a one-line probable cause (file,
line, reason, confidence) to the first few failures:

```text
    → agent (high confidence): src/price.ts:12 — roundCents now truncates instead of rounding, so the total is off by one cent
```

This happens when an API key is set, the run is not `--no-agent`, and
`agent.explain_failures` is not `false`.

```yaml
agent:
  explain_failures: true   # default
  explain_max: 3           # failing tests explained per run (default 3)
```

- **One request per failure, without tools.** It contains the candidate list, the failure
  output and the diffs of the top candidates, all bounded in size. Git-ignored files are
  never sent.
- **In the background.** Requests start as soon as their suite finishes, while later
  suites run. Each is capped at 30 s.
- **Cached.** Answers are cached by content in the state, so the same failure is not
  explained twice.
- **Harmless when it fails.** An error prints one note and never changes the exit code.
- **Built-in backend only.** An external `agent.command` is not used for explanations.

## See also

- [Shadow mode, flaky tests and stats](trust.md)
- [The LLM agent](agent.md)
- [Design: failure hints](../DESIGN.md#failure-hints)

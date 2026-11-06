# Use smates from a coding agent

Coding agents (Claude Code, Codex, Cursor, ...) test their own changes many times per
task. This page shows how they can run only the affected tests, read small output, and
make their own call on changes no test explains.

## Point the agent at smates

```bash
smates init --agents-md   # append a short smates section to AGENTS.md (or CLAUDE.md), once
smates guide              # print the one-page guide for coding agents
```

`smates guide` prints [docs/AGENTS.md](../AGENTS.md), a page written for an agent working
in a repository that uses smates. When the repository has an `AGENTS.md` or `CLAUDE.md`,
`smates init` suggests the short section that points the agent at it, and `--agents-md`
appends that section.

## The loop

After every change, the agent runs:

```bash
smates run --output failures
```

When something fails, the output ends with a short summary:

```text
smates: FAILED 1 of 7 test unit(s) in 3.2s:
  web: src/cart.test.ts
    changed inputs: src/price.ts (modified), fixtures/prices.json (modified)
smates: rerun them: smates run --output failures --only web:src/cart.test.ts
```

1. Fix the code.
2. Rerun the printed `--only` command until it passes.
3. Finish with a plain `smates run --output failures`. It reruns what failed, plus
   anything newly affected.

The agent should not run the whole test suite itself: smates has already picked what can
break.

### Small output: `--output failures`

An agent reads everything a command prints. `--output failures` keeps that small:

- Each runner batch's output is held back and printed only when the batch fails. For
  vitest, jest, pytest, `go test` and JUnit reports, only each failing test's own output
  is printed. For other runners, smates prints the failing batch's output, capped at 200
  lines (the first 30 and the last 170).
- smates' own progress lines are left out. The selection line stays.
- The run ends with one line when it passed, or else with the summary above: the
  failing tests, the changed inputs of each (from the [failure hints](failure-hints.md)),
  and a rerun command.

Set `SMATES_OUTPUT=agent` once in the agent's environment instead of passing the flag
every time. The flag wins when both are set.

### Run exactly some tests: `--only`

`--only suite:unit` (repeatable) runs exactly those test units (a test file, or a
package directory for Go) and records them as usual. Other tests and changes wait for
the next run. `smates list` shows the unit names.

### JSON for a program: `--report -`

`smates run --report -` writes the JSON report (selection, per-test results,
`exit_code`) to stdout, and everything else to stderr.

## Long runs

Agent tools often have a timeout, and a silent run looks like a hang.

- **Heartbeat.** In failures mode, smates prints a line after every 30 s without output:
  `smates: still running web (3 of 5 test unit(s) started, 1m 30s)`.
  `SMATES_HEARTBEAT=10s` changes the interval, and `0` turns it off.
- **Budget.** When the recorded durations of the selected tests add up to more than
  `--budget` (default `90s`; `0` turns it off), `smates run` first prints how to split
  the run: one suite at a time (`--suite <name>`), or in the background with
  `--report smates-report.json` and the report read afterwards.
- **Time limit.** `--max-time 5m` starts no test batch that would end after five
  minutes, by recorded durations. The tests it leaves out stay selected and run first
  next time. See [Time limit](../reference/cli.md#time-limit).

## Tests the agent's environment cannot run

An agent often works in a worktree or a sandbox without the database, the browser or
the services some tests need. There, those tests fail for reasons that have nothing to
do with the change. Declare what they need with
[`requires`](../reference/configuration.md#requires):

```yaml
suites:
  - name: api
    runner: go
    requires:
      - tcp: 127.0.0.1:5432
        tests: ["internal/store/**"]   # only these packages need the database
```

Where nothing listens on that port, `smates run` does not start those tests. The other
tests of the suite run as usual. The tests left out are not recorded and stay selected,
so a run where the database is up (or CI) runs them. The log names the check that
failed, and the run exits 4 when no other test failed.

## Stop hooks

`smates hook install --claude` and `smates hook install --codex` add a Stop hook that
runs the affected tests whenever the agent ends its turn, and keeps it going while some
fail. Tests left out by `requires` or by the hook's time limit (8 minutes by default)
do not block. See [Git hooks and Claude Code](git-hooks.md#claude-code-stop-hook).

## Exit codes

The exit code tells "fix the code" apart from "fix the setup":

| code | meaning | what to do |
|---|---|---|
| 0 | passed (or nothing to run) | done |
| 1 | tests failed | fix the code (or the test) |
| 2 | usage error, including an `--add` that names no test | fix the command line |
| 3 | configuration error | fix `smates.yml` (`smates config ...`), or the `--suite`/`--only` name (`smates list`) |
| 4 | environment problem: a runner that cannot start, git missing, a state that cannot be saved, a suite's `requires` not met | run `smates doctor --format json`; each failed check has a `fix`. For `requires`, the tests stay selected for a run where they are met |

Do not treat 3 or 4 as a test failure. Fix the setup, then run again. When tests also
failed, the exit code is 1. See [Exit codes](../reference/exit-codes.md) for every case.

## Letting the calling agent decide { #letting-the-calling-agent-decide }

Some changes are *orphans*: files no test is known to depend on, such as a Dockerfile or
a SQL file (see [Changes no test explains](orphans.md)). By default, smates asks its own
[LLM agent](agent.md) about them, or runs whole suites. A coding agent already knows what
it changed and why, so it can decide instead:

```bash
smates select --format json        # read orphan_candidates
smates run --output failures --orphans=skip --add api:tests/test_users.py
```

### 1. Read the candidates

`smates select --format json` lists one `orphan_candidates` entry per orphan. The other
fields of the JSON are unchanged.

```json
{
  "orphan_candidates": [
    {
      "path": "db/seed.sql",
      "status": "modified",
      "referenced_by": [
        { "suite": "api", "test": "tests/test_db.py", "file": "tests/conftest.py", "selected": true }
      ],
      "suites": ["api"],
      "handling": "reference"
    }
  ]
}
```

| field | meaning |
|---|---|
| `path` | the changed file |
| `status` | `added`, `modified` or `deleted` |
| `referenced_by` | tests that name the file: the test, the `file` that names it, and whether the test is already `selected` |
| `suites` | the suites the orphan policy would run in full for it |
| `handling` | how this selection handled it: `reference`, `untested`, `agent`, `no-impact`, `policy`, `ignored`, `skipped` (empty when not handled) |

### 2. Add tests

`--add <suite>:<unit>` adds a test unit (repeatable). Its reason in the log is `caller`.

- The unit may also be a test file: `--add api:tests/test_users.py`, or
  `--add pkg/foo_test.go` for a Go package unit.
- A path can be repository-relative, relative to the current directory, or (after
  `suite:`) relative to the suite's root.
- The suite may be left out when only one suite has the unit.
- A name that matches no test of the run is an error (exit 2).

### 3. Choose the policy for this run

`--orphans=agent|full|skip` overrides `orphans:` in `smates.yml` for one run:

| value | effect |
|---|---|
| `agent` | ask the built-in agent |
| `full` | run the affected suites in full (`orphans: all`) |
| `skip` | apply no orphan policy to what the name-reference scan did not explain |

Both flags work with `smates select` too (to preview the result), and as run flags of the
git hook (`smates hook install -- --orphans=full`). Neither combines with `--only`.

### The safety principle

The caller can only **add** tests. It can never remove a test that the deterministic
selection or the name-reference scan picked.

`--orphans=skip` is the one explicit opt-out, for a caller that judged the changes
itself. Use it only when you are sure the orphans cannot break a test. The log, the job
summary and the run record (`smates stats`) show those orphans as *skipped by caller*.
Like any handled orphan, they are not reported again on the same branch.

When orphans make whole suites run and the caller decided nothing (for example, without
an API key), the log says how to decide instead.

## Stop hooks

With Claude Code, a Stop hook can run the affected tests each time Claude ends its turn,
and send failures back to it: `smates hook install --claude`. See
[Git hooks and Claude Code](git-hooks.md#claude-code-stop-hook).

## See also

- [Agent guide](../AGENTS.md): the page `smates guide` prints
- [Commands](../reference/cli.md): every `smates run` flag
- [Exit codes](../reference/exit-codes.md)

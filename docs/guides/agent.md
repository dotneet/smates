# The LLM agent

smates can call a small LLM, called the *agent*, for two jobs that deterministic analysis
cannot do. This page covers when smates calls it, what it costs, and the limits that keep
it safe.

## Turn it on

Set one API key. That is the whole setup:

```bash
export ANTHROPIC_API_KEY=sk-ant-...
smates run
```

In GitHub Actions, pass the key to the action
(`anthropic-api-key: ${{ secrets.ANTHROPIC_API_KEY }}`); see
[GitHub Actions](../ci/github-actions.md). Without a key, smates works exactly the same
and falls back to running the affected suites in full.

## When smates calls it

- **Orphans.** These are changed files that no test is known to depend on (see
  [Changes no test explains](orphans.md)). The agent decides which tests, if any, they can
  affect.
- **Failure explanations.** After a failing run, it gives a one-line probable cause for a
  few failures (see [Failure hints](failure-hints.md#agent-explanations)).

Most pull requests have neither, so they make no LLM call at all.

## Providers

smates has built-in clients (no SDK) for three providers. With `provider: auto` (the
default), smates uses the first provider in this table whose key is set:

| provider | API key (base URL override) | API | default model |
|---|---|---|---|
| `anthropic` | `ANTHROPIC_API_KEY` or `ANTHROPIC_AUTH_TOKEN` (`ANTHROPIC_BASE_URL`) | Messages | `claude-haiku-5-5` |
| `openai` | `OPENAI_API_KEY` (`OPENAI_BASE_URL`) | Responses | `gpt-6-luna` |
| `openrouter` | `OPENROUTER_API_KEY` (`OPENROUTER_BASE_URL`) | Chat Completions | `anthropic/claude-haiku-5.5` |

```yaml
agent:
  provider: openai          # auto | anthropic | openai | openrouter
  model: gpt-6-luna         # overrides the provider's default model
  max_turns: 12
  timeout: 120s
```

`agent.enabled: never` turns the agent off for every run, failure explanations included.
For a single run, use `smates run --no-agent`.

## Cost and latency

Requests use low reasoning effort and prompt caching by default. smates drops the effort
setting for models that do not reason. A typical call costs well under $0.01 and takes a
few seconds.

`smates stats` counts agent calls, tokens and the cost, when the backend reports a cost
(see [Shadow mode, flaky tests and stats](trust.md#stats)).

## What the agent can do

For orphans, the agent explores the repository with **read-only tools**: `list_files`,
`grep`, `read_file`, `git_diff`, `tests_for_file`, `test_deps` and `search_tests`. They
are confined to the repository. Files git ignores, such as `.env`, are never readable.

It answers with one of these decisions:

- specific tests to add;
- a full run of the affected suites;
- no impact.

The agent **never removes tests**. Its answer can only add tests to what the
deterministic selection picked. A wrong or manipulated answer can only fail to add a
test, never hide one.

## Bounds and fallbacks

- Each call is bounded by `max_turns` (default 12), a wall-clock `timeout` (default
  `120s`) and an input-token budget.
- smates checks the answer against the real test inventory. It drops unknown test names,
  and the built-in backend asks once for a correction. An answer that names tests, none of
  which exist, counts as a failure, never as an empty selection.
- Any failure (no key, network error, timeout, invalid answer) falls back to running
  the affected suites in full, and the log says why.

## Caching

Answers are cached in the state, keyed by the orphans (path, status and content), the
model or command, `agent.instructions` and the test inventory. The same change on the same
inventory never asks twice. The log then says
`agent: reusing cached decision for N orphan file(s)`.

Failure explanations are cached the same way, keyed by the test, its changed inputs and
its failure output.

## Sharded runs

Sharded runs (`--shard I/N`) do not ask the agent. Each shard would ask on its own, and a
test picked only by one shard's answer could belong to another shard, so it would run
nowhere.

A shard reuses a decision cached in the state it restored, from an unsharded
`smates select` or `smates run` on the same change. Without one, it falls back to the
orphan policy and says so.

## Project-specific instructions

`agent.instructions` is appended to the system prompt. Use it for facts the agent cannot
find in the code:

```yaml
agent:
  instructions: "Files under deploy/ configure production only. db/seed.sql is loaded by tests/db."
```

## An external agent: `agent.command`

Any agent CLI can replace the built-in client:

```yaml
agent:
  command: "fx ask --json --no-save"
```

- smates runs the command with `sh -c` in the repository root, writes the prompt to its
  stdin, and reads the answer from its stdout. The prompt ends with the JSON schema of the
  answer (`decision`: `none`, `tests` or `run_all`, plus `tests`, `no_impact` and
  `summary`).
- The same timeout, validation, caching and fallback apply.
- The command is used for orphans only. Failure explanations need the built-in backend.

Process handling depends on whether smates has a terminal:

| where smates runs | process group | on the timeout | on Ctrl-C, SIGTERM, SIGHUP |
|---|---|---|---|
| no controlling terminal (CI) | its own | the whole group is killed | the whole group is killed |
| a terminal | smates' own, so it can prompt there (ssh passphrase, pinentry) | only the command's own process is killed | it stops together with smates |

## Fork pull requests

GitHub gives pull requests from forks no secrets, so no API key is available. smates
simply falls back to the orphan policy without the agent. Nothing fails.

## Failure explanations

With an API key, and without `--no-agent`, smates explains up to `agent.explain_max`
(default 3) failing tests per run; `agent.explain_failures: false` turns this off. Each
explanation is one request without tools, capped at 30 s and made in the background
while later suites run. See [Failure hints](failure-hints.md#agent-explanations) for
what is sent and what you get back.

## Letting the calling agent decide

When a coding agent (Claude Code, Codex, ...) calls smates, it already knows what it
changed. It can decide about orphans itself, with no second LLM call and no full suites:
see [Use smates from a coding agent](coding-agents.md#letting-the-calling-agent-decide).

## See also

- [Changes no test explains](orphans.md)
- [smates.yml reference](../reference/configuration.md): every `agent.*` key
- [Variables smates reads](../reference/environment-variables.md)

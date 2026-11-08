# Variables smates reads

The environment variables that change what smates itself does, and the ones it sets
for the test processes it starts. Which variables *your tests* read, and which of those
smates tracks, is covered in [Environment variables](../guides/environment.md).

## smates settings

| Variable | Values | Effect |
|---|---|---|
| `SMATES_OUTPUT` | `full`, `failures`, `agent` (= `failures`) | Default output mode of `smates run` when `--output` is not given. An invalid value is ignored with a warning. See [Coding agents](../guides/coding-agents.md). |
| `SMATES_HEARTBEAT` | a duration (`10s`, `1m`) or a number of seconds; `0` | How long `--output failures` stays silent before it prints a `still running` line. Default `30s`; `0` turns it off. |
| `SMATES_SKIP_HOOK` | any value but `0` or `false` | The git hook and the Claude Code Stop hook do nothing. See [Git hooks and Claude Code](../guides/git-hooks.md). |
| `SMATES_NO_STATE_SEED` | any value but `0` or `false` | A linked worktree without a state starts cold instead of being seeded (like `state_seed: false`). See [Parallel worktrees](../guides/worktrees.md). |
| `SMATES_BRANCH` | branch name | The branch smates records runs under (`smates stats --branch`) and whose ref `smates state push`/`pull` use (`refs/smates/state/<branch>`). Takes precedence over the CI's branch and the checked-out branch. Useful on a detached HEAD. |
| `SMATES_KEEP_TRACE` | any non-empty value | Keep the temporary trace directory of `smates run` and print its path (for debugging the shims). |

Boolean-like variables count as set for any value except the empty string, `0` and
`false`.

## Agent credentials

The [agent](../guides/agent.md) uses the first provider whose key is set (with
`agent.provider: auto`), or the provider `agent.provider` names.

| Variable | Provider | Meaning |
|---|---|---|
| `ANTHROPIC_API_KEY` | `anthropic` | API key. |
| `ANTHROPIC_AUTH_TOKEN` | `anthropic` | Bearer token, used when `ANTHROPIC_API_KEY` is not set. |
| `ANTHROPIC_BASE_URL` | `anthropic` | Base URL (default `https://api.anthropic.com`). |
| `OPENAI_API_KEY` | `openai` | API key. |
| `OPENAI_BASE_URL` | `openai` | Base URL (default `https://api.openai.com/v1`). |
| `OPENROUTER_API_KEY` | `openrouter` | API key. |
| `OPENROUTER_BASE_URL` | `openrouter` | Base URL (default `https://openrouter.ai/api/v1`). |

The four credential variables are removed from the environment of every test
process, whichever provider is used. A suite that needs one can set it in its `env:`.

## CI detection

smates detects the CI it runs on to tell pull requests from other builds, to find the
branch and the default branch, and to choose a base revision. On CI, a build that is not
a pull request (a push, scheduled or manually started build) of a branch in
[`full_run_branches`](configuration.md#top-level-keys) (default: the default branch) runs
every test; locally, tests without a recording are compared against
the base branch instead of all running as new.

| CI | Detected by | Variables read |
|---|---|---|
| GitHub Actions | `GITHUB_ACTIONS=true` | `GITHUB_REF_NAME`, `GITHUB_HEAD_REF`, `GITHUB_BASE_REF`, `GITHUB_EVENT_NAME` (`pull_request*` and `merge_group` are pull requests), `GITHUB_EVENT_PATH` (the default branch) |
| GitLab CI | `GITLAB_CI=true` | `CI_MERGE_REQUEST_SOURCE_BRANCH_NAME`, `CI_COMMIT_BRANCH`, `CI_DEFAULT_BRANCH`, `CI_MERGE_REQUEST_IID`, `CI_MERGE_REQUEST_TARGET_BRANCH_NAME` |
| CircleCI | `CIRCLECI=true` | `CIRCLE_BRANCH`, `CIRCLE_PULL_REQUEST` |
| Buildkite | `BUILDKITE=true` | `BUILDKITE_BRANCH`, `BUILDKITE_PIPELINE_DEFAULT_BRANCH`, `BUILDKITE_PULL_REQUEST`, `BUILDKITE_PULL_REQUEST_BASE_BRANCH` |
| Jenkins | `JENKINS_URL` set | `CHANGE_BRANCH`, `BRANCH_NAME`, `CHANGE_ID`, `CHANGE_TARGET` |
| Any other CI | `CI=true` or `CI=1` | none (branch from git; default branch from `origin/HEAD`) |

When the CI does not expose the default branch, smates uses `origin/HEAD`. Pass `--all`
or `--select` when neither is available (see [Other CI systems](../ci/other-ci.md)).

## GitHub Actions files

| Variable | Used by | Effect |
|---|---|---|
| `GITHUB_STEP_SUMMARY` | `smates run`, `smates select --format github` | The Markdown summary is appended to this file (the default of `--summary`). |
| `GITHUB_OUTPUT` | `smates run`, `smates select --format github` | Step outputs are appended: `selected`, `total`, `run-all`, `skip`, `tests`; `run` adds `exit-code`, `flaky` and `flaky-tests`. Without it, `select --format github` prints them to stdout. |
| `GITHUB_ACTIONS` | vitest suites | `true` also adds vitest's `github-actions` reporter next to smates' own. |

## Toolchain variables smates reads

smates reads these to understand your build. It does not change their meaning.

| Variable | Why |
|---|---|
| `GOFLAGS` | Build and test flags (`-tags`, `-race`, `-p`, `-timeout`, `-coverpkg`, `-exec`, ...) that affect what `go test` builds and runs, from the suite's `env:` or the environment. |
| `GOWORK` | Which Go modules belong to the workspace. |
| `GOOS`, `GOARCH`, `CGO_ENABLED` | Which Go files are built. |
| `NODE_OPTIONS`, `BUN_OPTIONS`, `PYTHONPATH`, `PYTEST_PLUGINS` | Kept: smates adds its shim to your value instead of replacing it. |
| `NODE_V8_COVERAGE` | Your coverage directory: replaced while recording, then the raw coverage files are copied into it. |

## install.sh

The install script reads these (see [Install](../getting-started/install.md)).

| Variable | Values | Effect |
|---|---|---|
| `SMATES_VERSION` | `v1.2.3`, `1.2.3`, `latest` | Release to install when no version argument is given. Default `latest`. |
| `SMATES_INSTALL_DIR` | directory | Install directory when `-b` is not given. Default `/usr/local/bin` when writable, else `~/.local/bin`. |
| `SMATES_VERIFY` | `checksum`, `attestation` | `checksum` (default) checks SHA-256 against `checksums.txt`. `attestation` also verifies the build provenance with `gh attestation verify`. |
| `GH_TOKEN` | token | Used by the `gh` CLI for `SMATES_VERIFY=attestation` when it is not logged in. |

## Variables smates sets for test processes

smates runs your suite command with your environment, the suite's `env:` on top, and
the following changes. They are internal: do not set them yourself. `SMATES_*`,
`NODE_OPTIONS`, `BUN_OPTIONS`, `NODE_V8_COVERAGE`, `PYTHONPATH`, `PYTEST_*` and
`GOFLAGS` are on the built-in ignore list, so a test that reads them is not selected
again when they differ.

| Variable | When | Value |
|---|---|---|
| `ANTHROPIC_API_KEY`, `ANTHROPIC_AUTH_TOKEN`, `OPENAI_API_KEY`, `OPENROUTER_API_KEY` | always | Removed (unless the suite's `env:` sets them). |
| `SMATES_TRACE_DIR` | recording; also runs without recording whose reporters write per-file results | The temporary trace directory. |
| `SMATES_REPO_ROOT` | recording | The repository root. |
| `NODE_OPTIONS` | recording (Node, and Node children of any suite) | `--require=<shim>` prepended to your value. |
| `BUN_OPTIONS` | recording (Bun) | `--preload=<shim>` appended to your value. |
| `NODE_V8_COVERAGE` | recording (Node, Bun) | A directory inside the trace directory. |
| `PYTHONPATH` | recording (Python, and Python children of any suite); `unittest` with a top-level directory | The shim directory (and the unittest top-level directory) prepended to your value. |
| `PYTEST_PLUGINS` | recording (Python) | `smates_pytest` appended to your value. |
| `SMATES_TRACE_PARENT`, `SMATES_TRACE_CTX` | recording | Cleared for the runner; set by the shims for the child processes they trace. |
| `GOFLAGS` | `go` suites | Recording: your flags without `-exec`, plus `-exec=<smates> -count=1`. Shadow runs: `-count=1` appended. Compile checks: their own build flags. |
| `SMATES_GOEXEC`, `SMATES_GOFLAGS_ORIG`, `SMATES_GOEXEC_USER` | recording `go` suites | Let smates' `-exec` wrapper run the test binary (behind your own `-exec` program, if any). |
| `GOCOVERDIR`, `SMATES_GOCOVERDIR` | recording with `granularity: func` | The per-function coverage directory. |
| `SMATES_GOCHECK` | compile checks (`granularity: func`) | Makes the `-exec` wrapper exit without running the test binary. |

See [Languages and runners](../guides/runners.md) for how each runner is traced.

## See also

- [Environment variables](../guides/environment.md): which variables tests read and smates tracks
- [Commands](cli.md)
- [GitHub Actions](../ci/github-actions.md)

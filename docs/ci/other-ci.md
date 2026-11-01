# smates on other CI systems

Outside GitHub Actions you need two things: the binary, and a way to carry
`.smates/state.json.gz` from one pipeline to the next. Everything else is
`smates run`.

## Install

```sh
curl -fsSL https://raw.githubusercontent.com/dotneet/smates/main/install.sh | sh -s -- -b "$HOME/.local/bin" v1.2.3
export PATH="$HOME/.local/bin:$PATH"
```

Pin a version in CI; without one, the script installs the latest release. It verifies
the archive against the release's `checksums.txt`. Without `-b` it installs to
`/usr/local/bin` when writable (typical in containers running as root), else
`~/.local/bin`. Every option, including `SMATES_VERIFY=attestation`, is on
[Install](../getting-started/install.md#install-script-linux-macos);
`go install github.com/dotneet/smates/cmd/smates@v1.2.3` works too.

## Carrying the state

The state is one file. Any storage works, as long as you follow three rules:

1. **Restore before, save after**, and save even when tests failed (failed tests are
   then re-selected). Keep the tests' exit code as the job's exit code.
2. **Restore the branch's own state first, else the default branch's.** Any state is
   safe to start from, because an entry only lets a test be skipped when all of its
   inputs are byte-identical to the run that recorded it; the fallback order only
   maximizes reuse.
3. **Only trusted pipelines write state that other branches read.** State decides what
   is skipped, so a pipeline running untrusted code (e.g. fork merge requests) must not
   write the default branch's state.

Two ways to do that:

- **The CI's own cache**, keyed by branch with a default-branch fallback (GitLab,
  CircleCI). No credentials needed.
- **A git ref** with `smates state pull` / `smates state push` (any CI, also local
  machines). `push` stores the file under `refs/smates/state/<branch>` in the
  repository; `pull` fetches the current branch's ref and falls back to the default
  branch's. Pushing needs credentials with write access to the repository, so by
  default only the default branch pushes; every pipeline pulls.

The branch is `$SMATES_BRANCH`, else the branch the CI reports, else the checked-out
branch. On a detached HEAD without any of these, `smates state push` needs `--ref`.


Default branch detection: `smates run` runs everything on the default branch, detected
from the CI environment or `origin/HEAD`. If your CI checks out without that
information, pass `--all` or `--select` explicitly (see the Jenkins example).

## GitLab CI

```yaml
variables:
  SMATES_VERSION: v1.2.3

test:
  image: node:22
  before_script:
    - curl -fsSL https://raw.githubusercontent.com/dotneet/smates/main/install.sh | sh -s -- "$SMATES_VERSION"
    - npm ci
  script:
    - smates run
  cache:
    - key: smates-$CI_COMMIT_REF_SLUG
      fallback_keys:
        - smates-$CI_DEFAULT_BRANCH
      paths:
        - .smates/
      when: always
```

`fallback_keys` needs GitLab 16.0 or later. By default GitLab keeps separate caches
for protected and unprotected branches, so merge request pipelines on unprotected
branches cannot read the cache of a protected `main`. Either turn off **Settings >
CI/CD > General pipelines > Use separate caches for protected branches** (only if all
branches are trusted), or use the git-ref approach below for the fallback.

### Merge request comment

`smates run --markdown <file>` (or `smates select --format markdown`) writes a
Markdown summary for reviewers: what the change affects, why each test was selected,
how unattributed changes were handled and how much test time was skipped (see
[the GitHub Actions comment](github-actions.md#pull-request-comment)). Write it outside
the checkout, where it cannot count as a change.

Posting it is up to the CI. On GitLab, with a project access token (`api` scope) in
`SMATES_GITLAB_TOKEN`, this keeps one note per merge request up to date:

```yaml
  script:
    - summary=$(mktemp)
    - status=0; smates run --markdown "$summary" || status=$?
    - |
      if [ -n "${CI_MERGE_REQUEST_IID:-}" ] && [ -s "$summary" ]; then
        marker='<!-- smates-summary -->'
        api="$CI_API_V4_URL/projects/$CI_PROJECT_ID/merge_requests/$CI_MERGE_REQUEST_IID/notes"
        auth="PRIVATE-TOKEN: $SMATES_GITLAB_TOKEN"
        body=$({ echo "$marker"; cat "$summary"; } | jq -Rs '{body: .}')
        id=$(curl -fsS -H "$auth" "$api?per_page=100" |
          jq -r --arg m "$marker" '[.[] | select(.body | startswith($m))][0].id // empty')
        if [ -n "$id" ]; then method=PUT url="$api/$id"; else method=POST url="$api"; fi
        curl -fsS -X "$method" -H "$auth" -H 'Content-Type: application/json' -d "$body" "$url" >/dev/null ||
          echo "could not comment on the merge request"
      fi
    - exit "$status"
```

The same works on any CI with an API for pull request comments; run it only for
merge requests from the same project (forks should not get the token).

## CircleCI

```yaml
version: 2.1

jobs:
  test:
    docker:
      - image: cimg/node:22.11
    steps:
      - checkout
      - run:
          name: Install smates
          command: |
            curl -fsSL https://raw.githubusercontent.com/dotneet/smates/main/install.sh | sh -s -- -b "$HOME/.local/bin" v1.2.3
            echo 'export PATH="$HOME/.local/bin:$PATH"' >> "$BASH_ENV"
      - run: npm ci
      - restore_cache:
          keys:
            - smates-v1-{{ .Branch }}-
            - smates-v1-main-
      - run: smates run
      - save_cache:
          key: smates-v1-{{ .Branch }}-{{ .BuildNum }}
          paths:
            - .smates
          when: always

workflows:
  test:
    jobs:
      - test
```

CircleCI caches are immutable, hence the build number in the key; `restore_cache`
picks the newest entry matching each prefix in order. Bump `v1` to discard all state.

## Buildkite

Buildkite has no built-in cache, so this uses the git ref. The agent's git
credentials must be allowed to push for the default-branch push.

```yaml
steps:
  - label: ":test_tube: tests"
    env:
      SMATES_VERSION: v1.2.3
    command: |
      curl -fsSL https://raw.githubusercontent.com/dotneet/smates/main/install.sh | sh -s -- -b "$HOME/.local/bin" "$SMATES_VERSION"
      export PATH="$HOME/.local/bin:$PATH"

      smates state pull || echo "no saved state yet"
      summary=$(mktemp) # outside the checkout: untracked files there count as changes
      status=0
      smates run --summary "$summary" || status=$?
      if [ -s "$summary" ]; then
        buildkite-agent annotate --style info --context smates < "$summary"
      fi
      if [ "$BUILDKITE_BRANCH" = "$BUILDKITE_PIPELINE_DEFAULT_BRANCH" ]; then
        smates state push
      fi
      exit "$status"
```

## Jenkins

A declarative `Jenkinsfile` for a multibranch pipeline. `git-push` is a username/token
credential with push access, used only on `main` through the Git plugin's
`gitUsernamePassword` binding.

```groovy
pipeline {
  agent any
  environment {
    SMATES_VERSION = 'v1.2.3'
  }
  stages {
    stage('Test') {
      steps {
        withEnv(["PATH+SMATES=${env.HOME}/.local/bin"]) {
          sh 'curl -fsSL https://raw.githubusercontent.com/dotneet/smates/main/install.sh | sh -s -- -b "$HOME/.local/bin" "$SMATES_VERSION"'
          sh 'smates state pull || echo "no saved state yet"'
          // Jenkins checkouts usually lack origin/HEAD, so choose the mode explicitly.
          sh 'if [ "$BRANCH_NAME" = main ]; then smates run --all; else smates run --select; fi'
        }
      }
    }
  }
  post {
    always {
      script {
        if (env.BRANCH_NAME == 'main') {
          withEnv(["PATH+SMATES=${env.HOME}/.local/bin"]) {
            withCredentials([gitUsernamePassword(credentialsId: 'git-push', gitToolName: 'Default')]) {
              sh 'smates state push'
            }
          }
        }
      }
    }
  }
}
```

Workspaces on persistent agents keep `.smates/` between builds of the same branch,
which already gives per-branch reuse; `state pull` adds the default branch's state.

## Per-branch state in a git ref (optional)

If pull requests are trusted (no fork contributions) and have push credentials, let
every branch push its own state, not only the default branch:

```sh
smates state pull || true   # refs/smates/state/<branch>, else the default branch's
status=0
smates run || status=$?
smates state push           # refs/smates/state/<branch>
exit "$status"
```

No extra logic is needed for the fallback: `smates state pull` tries the branch's ref
first and the default branch's ref next. Delete a branch's ref when the branch is
merged: `git push origin --delete refs/smates/state/<branch>`.

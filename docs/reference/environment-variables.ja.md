# smates が読む環境変数 { #variables-smates-reads }

smates 自身の動作を変える環境変数と、smates が起動するテストプロセスに設定する環境変数をまとめたページです。*テストが*読む変数と、そのうち smates が追跡するものは [環境変数](../guides/environment.md) で説明しています。

## smates の設定 { #smates-settings }

| 変数 | 値 | 効果 |
|---|---|---|
| `SMATES_OUTPUT` | `full`、`failures`、`agent`（= `failures`） | `--output` を指定しないときの `smates run` の出力モード。不正な値は警告を出して無視します。[コーディングエージェントから使う](../guides/coding-agents.md) を参照してください。 |
| `SMATES_HEARTBEAT` | 時間（`10s`、`1m`）または秒数、`0` | `--output failures` が `still running` の行を出すまでに黙っている時間。デフォルトは `30s` で、`0` で無効になります。 |
| `SMATES_SKIP_HOOK` | `0` と `false` 以外の任意の値 | git フックと Claude Code の Stop フックが何もしなくなります。[git フックと Claude Code](../guides/git-hooks.md) を参照してください。 |
| `SMATES_NO_STATE_SEED` | `0` と `false` 以外の任意の値 | ステートのないリンクされた worktree を、シードせず空から始めます（`state_seed: false` と同じ）。[並行する worktree](../guides/worktrees.md) を参照してください。 |
| `SMATES_BRANCH` | ブランチ名 | smates が実行を記録するブランチ（`smates stats --branch`）と、`smates state push` / `pull` が使う ref（`refs/smates/state/<branch>`）のブランチ。CI のブランチやチェックアウト中のブランチより優先されます。detached HEAD で便利です。 |
| `SMATES_KEEP_TRACE` | 空でない任意の値 | `smates run` の一時的なトレースディレクトリを残し、そのパスを表示します（シムのデバッグ用）。 |

真偽値のような変数は、空文字列、`0`、`false` 以外の値なら設定されているとみなします。

## エージェントの認証情報 { #agent-credentials }

[エージェント](../guides/agent.md) は、キーが設定されている最初のプロバイダー（`agent.provider: auto` のとき）か、`agent.provider` で指定したプロバイダーを使います。

| 変数 | プロバイダー | 意味 |
|---|---|---|
| `ANTHROPIC_API_KEY` | `anthropic` | API キー。 |
| `ANTHROPIC_AUTH_TOKEN` | `anthropic` | Bearer トークン。`ANTHROPIC_API_KEY` がないときに使います。 |
| `ANTHROPIC_BASE_URL` | `anthropic` | ベース URL（デフォルト `https://api.anthropic.com`）。 |
| `OPENAI_API_KEY` | `openai` | API キー。 |
| `OPENAI_BASE_URL` | `openai` | ベース URL（デフォルト `https://api.openai.com/v1`）。 |
| `OPENROUTER_API_KEY` | `openrouter` | API キー。 |
| `OPENROUTER_BASE_URL` | `openrouter` | ベース URL（デフォルト `https://openrouter.ai/api/v1`）。 |

4 つの認証情報の変数は、どのプロバイダーを使う場合でも、すべてのテストプロセスの環境から取り除かれます。必要なスイートは `env:` で設定できます。

## CI の検出 { #ci-detection }

smates は実行中の CI を検出し、プルリクエストとそれ以外のビルドを区別し、ブランチとデフォルトブランチを特定し、比較の基準となるリビジョンを選びます。CI 上で [`full_run_branches`](configuration.md#top-level-keys) のブランチ（指定がなければデフォルトブランチ）を、プルリクエスト以外のビルド（push、スケジュール実行、手動で開始したビルドなど）でビルドするときはすべてのテストを実行します。ローカルでは、記録のないテストを新規としてすべて実行するのではなく、ベースブランチと比較します。

| CI | 検出条件 | 読む変数 |
|---|---|---|
| GitHub Actions | `GITHUB_ACTIONS=true` | `GITHUB_REF_NAME`、`GITHUB_HEAD_REF`、`GITHUB_BASE_REF`、`GITHUB_EVENT_NAME`（`pull_request*` と `merge_group` はプルリクエスト）、`GITHUB_EVENT_PATH`（デフォルトブランチ） |
| GitLab CI | `GITLAB_CI=true` | `CI_MERGE_REQUEST_SOURCE_BRANCH_NAME`、`CI_COMMIT_BRANCH`、`CI_DEFAULT_BRANCH`、`CI_MERGE_REQUEST_IID`、`CI_MERGE_REQUEST_TARGET_BRANCH_NAME` |
| CircleCI | `CIRCLECI=true` | `CIRCLE_BRANCH`、`CIRCLE_PULL_REQUEST` |
| Buildkite | `BUILDKITE=true` | `BUILDKITE_BRANCH`、`BUILDKITE_PIPELINE_DEFAULT_BRANCH`、`BUILDKITE_PULL_REQUEST`、`BUILDKITE_PULL_REQUEST_BASE_BRANCH` |
| Jenkins | `JENKINS_URL` が設定されている | `CHANGE_BRANCH`、`BRANCH_NAME`、`CHANGE_ID`、`CHANGE_TARGET` |
| その他の CI | `CI=true` または `CI=1` | なし（ブランチは git から、デフォルトブランチは `origin/HEAD` から） |

CI がデフォルトブランチを公開していなければ、smates は `origin/HEAD` を使います。どちらもない場合は `--all` か `--select` を指定してください（[その他の CI](../ci/other-ci.md)）。

## GitHub Actions のファイル { #github-actions-files }

| 変数 | 使うコマンド | 効果 |
|---|---|---|
| `GITHUB_STEP_SUMMARY` | `smates run`、`smates select --format github` | Markdown のまとめをこのファイルに追記します（`--summary` のデフォルト）。 |
| `GITHUB_OUTPUT` | `smates run`、`smates select --format github` | ステップの出力 `selected`、`total`、`run-all`、`skip`、`tests` を追記します。`run` はさらに `exit-code`、`flaky`、`flaky-tests` を追加します。この変数がなければ、`select --format github` は stdout に出力します。 |
| `GITHUB_ACTIONS` | vitest のスイート | `true` なら、smates 自身のレポーターに加えて vitest の `github-actions` レポーターも追加します。 |

## smates が読むツールチェーンの変数 { #toolchain-variables-smates-reads }

smates はビルドの内容を把握するためにこれらを読みます。意味を変えることはありません。

| 変数 | 理由 |
|---|---|
| `GOFLAGS` | `go test` がビルド・実行する内容に影響するフラグ（`-tags`、`-race`、`-p`、`-timeout`、`-coverpkg`、`-exec` など）。スイートの `env:` または環境から読みます。 |
| `GOWORK` | どの Go モジュールがワークスペースに含まれるか。 |
| `GOOS`、`GOARCH`、`CGO_ENABLED` | どの Go ファイルがビルドされるか。 |
| `NODE_OPTIONS`、`BUN_OPTIONS`、`PYTHONPATH`、`PYTEST_PLUGINS` | 置き換えずに保持します。smates はあなたの値にシムを追加します。 |
| `NODE_V8_COVERAGE` | あなたのカバレッジ用ディレクトリ。記録中は置き換え、実行後に生のカバレッジファイルをそこへコピーします。 |

## install.sh { #installsh }

インストールスクリプトは次の変数を読みます（[インストール](../getting-started/install.md)）。

| 変数 | 値 | 効果 |
|---|---|---|
| `SMATES_VERSION` | `v1.2.3`、`1.2.3`、`latest` | バージョンの引数がないときにインストールするリリース。デフォルトは `latest`。 |
| `SMATES_INSTALL_DIR` | ディレクトリ | `-b` がないときのインストール先。デフォルトは書き込めれば `/usr/local/bin`、そうでなければ `~/.local/bin`。 |
| `SMATES_VERIFY` | `checksum`、`attestation` | `checksum`（デフォルト）は `checksums.txt` と SHA-256 を照合します。`attestation` は `gh attestation verify` でビルドの来歴も検証します。 |
| `GH_TOKEN` | トークン | `SMATES_VERIFY=attestation` で、`gh` CLI がログインしていないときに使われます。 |

## smates がテストプロセスに設定する変数 { #variables-smates-sets-for-test-processes }

smates はスイートのコマンドを、あなたの環境にスイートの `env:` を重ね、さらに次の変更を加えて実行します。これらは内部用なので、自分で設定しないでください。`SMATES_*`、`NODE_OPTIONS`、`BUN_OPTIONS`、`NODE_V8_COVERAGE`、`PYTHONPATH`、`PYTEST_*`、`GOFLAGS` は組み込みの無視リストに入っているので、テストがこれらを読んでも、値の違いで再び選択されることはありません。

| 変数 | 設定する場面 | 値 |
|---|---|---|
| `ANTHROPIC_API_KEY`、`ANTHROPIC_AUTH_TOKEN`、`OPENAI_API_KEY`、`OPENROUTER_API_KEY` | 常に | 取り除きます（スイートの `env:` で設定されていなければ）。 |
| `SMATES_TRACE_DIR` | 記録中。記録しない実行でも、レポーターがファイルごとの結果を書く場合 | 一時的なトレースディレクトリ。 |
| `SMATES_REPO_ROOT` | 記録中 | リポジトリのルート。 |
| `NODE_OPTIONS` | 記録中（Node、および任意のスイートの Node の子プロセス） | あなたの値の先頭に `--require=<shim>` を追加。 |
| `BUN_OPTIONS` | 記録中（Bun） | あなたの値の末尾に `--preload=<shim>` を追加。 |
| `NODE_V8_COVERAGE` | 記録中（Node、Bun） | トレースディレクトリ内のディレクトリ。 |
| `PYTHONPATH` | 記録中（Python、および任意のスイートの Python の子プロセス）、トップレベルディレクトリのある `unittest` | あなたの値の先頭にシムのディレクトリ（と unittest のトップレベルディレクトリ）を追加。 |
| `PYTEST_PLUGINS` | 記録中（Python） | あなたの値の末尾に `smates_pytest` を追加。 |
| `SMATES_TRACE_PARENT`、`SMATES_TRACE_CTX` | 記録中 | ランナーに対しては空にし、シムがトレースする子プロセスのために設定します。 |
| `GOFLAGS` | `go` のスイート | 記録中: あなたのフラグから `-exec` を除き、`-exec=<smates> -count=1` を追加。シャドーモード: `-count=1` を追加。コンパイルチェック: 専用のビルドフラグ。 |
| `SMATES_GOEXEC`、`SMATES_GOFLAGS_ORIG`、`SMATES_GOEXEC_USER` | 記録中の `go` のスイート | smates の `-exec` ラッパーがテストバイナリを実行できるようにします（あなた自身の `-exec` プログラムがあればその背後で）。 |
| `GOCOVERDIR`、`SMATES_GOCOVERDIR` | `granularity: func` の記録中 | テスト関数ごとのカバレッジディレクトリ。 |
| `SMATES_GOCHECK` | コンパイルチェック（`granularity: func`） | `-exec` ラッパーがテストバイナリを実行せずに終了するようにします。 |

各ランナーのトレース方法は [言語とランナー](../guides/runners.md) を参照してください。

## 関連項目 { #see-also }

- [環境変数](../guides/environment.md): テストが読む変数と smates が追跡する変数
- [コマンド](cli.md)
- [GitHub Actions](../ci/github-actions.md)

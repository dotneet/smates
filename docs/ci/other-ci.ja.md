# その他の CI で smates を使う { #smates-on-other-ci-systems }

GitHub Actions 以外で必要なのは、バイナリと、`.smates/state.json.gz` をパイプラインからパイプラインへ引き継ぐ方法の 2 つです。あとは `smates run` だけです。

## インストール { #install }

```sh
curl -fsSL https://raw.githubusercontent.com/dotneet/smates/main/install.sh | sh -s -- -b "$HOME/.local/bin" v1.2.3
export PATH="$HOME/.local/bin:$PATH"
```

CI ではバージョンを固定してください。指定しなければ、スクリプトは最新のリリースをインストールします。アーカイブはリリースの `checksums.txt` で検証されます。`-b` を指定しなければ、書き込める場合は `/usr/local/bin`（root で動くコンテナでよくあるケース）、そうでなければ `~/.local/bin` にインストールします。`SMATES_VERIFY=attestation` を含むすべてのオプションは [インストール](../getting-started/install.md#install-script-linux-macos) にあります。`go install github.com/dotneet/smates/cmd/smates@v1.2.3` でもインストールできます。

## ステートの引き継ぎ { #carrying-the-state }

ステートは 1 つのファイルです。次の 3 つの規則を守れば、どんなストレージでもかまいません。

1. **前に復元し、後で保存する。** テストが失敗したときも保存します（失敗したテストは次回また選択されます）。ジョブの終了コードはテストの終了コードのままにしてください。
2. **まずそのブランチのステート、なければデフォルトブランチのステートを復元する。** エントリがテストのスキップを許すのは、その入力がすべて記録時とバイト単位で同一のときだけなので、どのステートから始めても安全です。フォールバックの順序は再利用を最大にするためにすぎません。
3. **ほかのブランチが読むステートを書くのは、信頼できるパイプラインだけにする。** 何をスキップするかはステートが決めるので、信頼できないコードを実行するパイプライン（フォークからのマージリクエストなど）にデフォルトブランチのステートを書かせてはいけません。

方法は 2 つあります。

- **CI 自身のキャッシュ**を、デフォルトブランチへのフォールバック付きでブランチごとのキーで使う（GitLab、CircleCI）。認証情報は不要です。
- **git の ref** を `smates state pull` / `smates state push` で使う（どの CI でも、ローカルのマシンでも使えます）。`push` はリポジトリの `refs/smates/state/<branch>` にファイルを保存し、`pull` は現在のブランチの ref を取得して、なければデフォルトブランチの ref にフォールバックします。push にはリポジトリへの書き込み権限を持つ認証情報が必要なので、デフォルトでは push するのはデフォルトブランチだけにし、どのパイプラインも pull します。

ブランチは `$SMATES_BRANCH`、なければ CI が報告するブランチ、それもなければチェックアウト中のブランチです。いずれもない detached HEAD では、`smates state push` に `--ref` が必要です。


デフォルトブランチの検出: `smates run` はデフォルトブランチではすべてを実行します。デフォルトブランチは CI の環境変数か `origin/HEAD` から検出します。CI がその情報なしでチェックアウトする場合は、`--all` か `--select` を明示してください（Jenkins の例を参照）。

## GitLab CI { #gitlab-ci }

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

`fallback_keys` には GitLab 16.0 以降が必要です。GitLab はデフォルトで保護ブランチと非保護ブランチのキャッシュを分けるので、非保護ブランチのマージリクエストのパイプラインは、保護された `main` のキャッシュを読めません。**Settings > CI/CD > General pipelines > Use separate caches for protected branches** をオフにする（すべてのブランチが信頼できる場合だけ）か、フォールバックには下の git の ref を使う方法を使ってください。

### マージリクエストへのコメント { #merge-request-comment }

`smates run --markdown <file>`（または `smates select --format markdown`）は、レビュアー向けの Markdown のまとめを書き出します。変更が何に影響するか、各テストが選択された理由、紐づかない変更の扱い、スキップしたテスト時間などです（[GitHub Actions のコメント](github-actions.md#pull-request-comment) を参照）。変更として数えられないよう、チェックアウトの外に書き出してください。

投稿は CI 側の仕事です。GitLab では、`api` スコープのプロジェクトアクセストークンを `SMATES_GITLAB_TOKEN` に入れておけば、次のスクリプトでマージリクエストごとに 1 つのノートを最新に保てます。

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

プルリクエストにコメントする API を持つ CI なら、同じ方法が使えます。実行するのは同じプロジェクトからのマージリクエストだけにしてください（フォークにトークンを渡すべきではありません）。

## CircleCI { #circleci }

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

CircleCI のキャッシュは変更できないので、キーにビルド番号を入れています。`restore_cache` は、各プレフィックスに一致する最新のエントリを順に探します。すべてのステートを捨てるには `v1` を上げてください。

## Buildkite { #buildkite }

Buildkite には組み込みのキャッシュがないので、git の ref を使います。デフォルトブランチで push するために、エージェントの git の認証情報に push の権限が必要です。

```yaml
steps:
  - label: ":test_tube: tests"
    env:
      SMATES_VERSION: v1.2.3
    command: |
      curl -fsSL https://raw.githubusercontent.com/dotneet/smates/main/install.sh | sh -s -- -b "$HOME/.local/bin" "$SMATES_VERSION"
      export PATH="$HOME/.local/bin:$PATH"

      smates state pull || echo "no saved state yet"
      summary=$(mktemp) # チェックアウトの外に: そこにある git 管理外のファイルは変更として数えられる
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

## Jenkins { #jenkins }

マルチブランチパイプライン用の宣言的な `Jenkinsfile` です。`git-push` は push の権限を持つユーザー名とトークンの認証情報で、Git プラグインの `gitUsernamePassword` バインディングを通じて `main` でだけ使います。

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
          // Jenkins のチェックアウトには通常 origin/HEAD がないので、モードを明示する。
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

常駐するエージェントのワークスペースは、同じブランチのビルド間で `.smates/` を保持するので、それだけでブランチごとの再利用になります。`state pull` はそこにデフォルトブランチのステートを加えます。

## git の ref でブランチごとのステートを持つ（任意） { #per-branch-state-in-a-git-ref-optional }

プルリクエストが信頼でき（フォークからの貢献がない）、push の認証情報を持っているなら、デフォルトブランチだけでなく各ブランチが自分のステートを push できます。

```sh
smates state pull || true   # refs/smates/state/<branch>、なければデフォルトブランチの ref
status=0
smates run || status=$?
smates state push           # refs/smates/state/<branch>
exit "$status"
```

フォールバックのための処理は要りません。`smates state pull` はブランチの ref を先に、次にデフォルトブランチの ref を試します。ブランチをマージしたら、その ref を削除してください: `git push origin --delete refs/smates/state/<branch>`。

# GitHub Actions で smates を使う { #smates-on-github-actions }

`dotneet/smates` アクションは、smates をインストールし、ステートを復元し、影響を受けるテストを実行して、更新したステートを保存します。テストのコマンドを 1 ステップで置き換えられます。

```yaml
on:
  pull_request:
  push:
    branches: [main]

jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v5
      - uses: actions/setup-node@v4   # ほかにもテストに必要なツールチェーンを
        with:
          node-version: 22
      - run: npm ci
      - uses: dotneet/smates@vX.Y.Z
        with:
          anthropic-api-key: ${{ secrets.ANTHROPIC_API_KEY }}   # 任意
```

`vX.Y.Z` はリリース（`v1.2.3`）に置き換えるか、`@v1` として v1.x の最新リリースに追従させます（[バージョンを固定する](#pin-the-version) を参照）。

コメント付きの完全なワークフローは [`.github/workflows/smates.yml`](https://github.com/dotneet/smates/blob/main/.github/workflows/smates.yml) にあります。`smates init --github` を実行すると、プロジェクトに合わせたワークフローを生成します。既存のワークフローがすでにテストを実行している場合は、2 つ目のワークフローを追加する代わりに、そのステップをアクションに置き換えるパッチを表示します（`--patch FILE` ならファイルに書き出します）。

## アクションがすること { #what-the-action-does }

1. **インストール。** リリースから `smates_<os>_<arch>` をダウンロードし、リリースの `checksums.txt` とビルドの来歴のアテステーション（[`verify`](#verification) を参照）で検証して `PATH` に置きます。アーカイブはバージョン、OS、アーキテクチャごとにキャッシュされ、キャッシュがヒットしたときも毎回検証し直します（そのため `checksums.txt` の取得にネットワークが必要です）。`version: source` の場合、またはダウンロードに失敗して `PATH` に Go がある場合は、アクション自身のチェックアウトからビルドします。検証に失敗したときは必ずジョブを失敗させ、ほかの方法にフォールバックすることはありません。
2. **ステートの復元**（`.smates/`）を Actions のキャッシュから行います。このブランチの最新のステート、なければデフォルトブランチの最新のステート、それもなければ何もなし（コールドスタート）です。
3. **実行。** `working-directory` で `smates <command> <args>` を実行します。ステップの終了コードはテストの終了コードです。
4. **コメント**（`comment: true` のとき）。選択のまとめをプルリクエストにコメントします。
5. **ステートの保存。** テストが失敗したときも保存し（失敗したテストは次回また選択されます）、ステートファイルが実際に変わった場合だけ保存します。

どの実行でも、まとめはジョブのページ（`$GITHUB_STEP_SUMMARY`）にも書き込まれます。

デフォルトブランチでは、`smates run` はすべてのテストを実行して完全なステートを書きます。新しいプルリクエストはどれもそこから始まります。ほかのブランチでは選択します。`args: --all` や `args: --select` で変えられます。

### ブランチのフォールバックが安全な理由 { #why-branch-fallback-is-safe }

ステートのエントリが表すのは「入力がこれらのコンテンツハッシュだったとき、テスト T は成功した」ということです。別のブランチのエントリを再利用しても、T がスキップされるのは T のすべての入力がその実行時とバイト単位で同一のときだけです。したがって、どのステートから始めても安全で、ブランチごとに分けるのはヒット率を上げるためにすぎません。

重要なのは、ほかの人が読むステートを*書ける*のは誰か、です。GitHub のキャッシュのスコープでは、プルリクエストはデフォルトブランチのキャッシュを読めますが、プルリクエストが保存したものはそのプルリクエストの外からは読めません。フォークからのプルリクエストがデフォルトブランチのステートに影響することはありません。

## 入力 { #inputs }

| 入力 | デフォルト | 説明 |
|---|---|---|
| `version` | アクションの ref | インストールするリリース（`v1.2.3`）、`latest`、または `source`。既定では `uses:` の ref に従います。リリースタグ（またはそのコミット）ならそのリリース、`v1` や `v1.2` のような移動するタグならその系列の最新リリース、それ以外の ref なら `latest` です。 |
| `verify` | `auto` | ダウンロードの検証方法: `auto`、`attestation`、`checksum`（[後述](#verification)）。 |
| `command` | `run` | サブコマンド。`select` のデフォルトは `--format github` です。 |
| `args` | | 追加の引数。シェルと同じ規則で分割されます: `--shard 1/4 -- --reporter=dot`。信頼できない値（PR のタイトル、ブランチ名）を埋め込まないでください。 |
| `working-directory` | `.` | smates を実行する場所。 |
| `anthropic-api-key` | | オーファン（どのテストにも紐づかない変更）を判断するエージェント用のキー。smates のステップにだけ渡されます。指定がなければジョブの環境の `ANTHROPIC_API_KEY` を使います。 |
| `openai-api-key` | | 同様に OpenAI 用（`OPENAI_API_KEY`）。 |
| `openrouter-api-key` | | 同様に OpenRouter 用（`OPENROUTER_API_KEY`）。複数のキーがある場合は `smates.yml` の `agent.provider` で選びます（デフォルト: Anthropic、OpenAI、OpenRouter の順）。 |
| `cache` | `true` | Actions のキャッシュでステートを復元・保存します。自分で保存するなら `false` にします。 |
| `cache-key` | | 同じ OS で異なるテストを実行するジョブ（マトリクスの各エントリ、シャード）のための、キャッシュキーの追加要素。 |
| `state-path` | `.smates` | ステートのディレクトリ（`working-directory` からの相対）。`smates.yml` の `state` と一致させる必要があります。 |
| `comment` | `false` | `true` にすると、まとめをプルリクエストにコメントし、その場で更新します（[後述](#pull-request-comment)）。 |
| `comment-key` | | 1 つのプルリクエストにある複数の smates ジョブのコメントを分けます。 |
| `github-token` | `github.token` | コメントとアテステーションの照会に使うトークン。 |

キャッシュキーは `smates-state-<runner.os>-[<cache-key>-]<branch>-<run_id>-<run_attempt>` で、`...-<branch>-`、次に `...-<default branch>-` のプレフィックスで復元します。

## 出力 { #outputs }

| 出力 | 説明 |
|---|---|
| `selected` | 選択されたテストユニットの数。 |
| `total` | 既知のテストユニットの数。 |
| `run-all` | すべてが選択されたとき `true`。 |
| `skip` | 何も選択されなかったとき `true`。 |
| `tests` | JSON オブジェクト: スイートごとの、選択されたテストユニットの ID。 |
| `suites` | JSON オブジェクト。スイートごとに 1 エントリ: `{"selected": 2, "total": 40, "all": false, "skip": false}`。`skip` が `true` になるのは、そのスイートで何も選択されなかったときだけです（[これでゲートします](#gate-jobs-per-suite)）。 |
| `exit-code` | smates コマンドの終了コード。 |
| `flaky` | `run` が flaky と証明したテストユニットの数（入力が変わらないのに結果が反転したもの）。 |
| `flaky-tests` | それらのユニットの JSON 配列（`"suite: unit"`）。`smates flaky` も参照してください。 |
| `version` | インストールされた smates のバージョン（ソースからビルドした場合は `source`）。 |
| `verified` | 検証方法: `attestation`、`checksum`、`none`（ソースからのビルド）。 |
| `comment-url` | コメントを投稿・更新したときの、プルリクエストのコメントの URL。 |

## パターン { #patterns }

### バージョンを固定する { #pin-the-version }

`uses: dotneet/smates@v1.2.3` は v1.2.3 をそのままインストールし、`@v1` は v1.x の最新リリースをインストールします。固定したバージョンの更新は Dependabot（`package-ecosystem: github-actions`）に任せましょう。

多くのセキュリティポリシーが求めるようにコミットで固定するなら、リリースタグのコミットを使います。Dependabot と Renovate はコメントも追従させます。

```yaml
      - uses: dotneet/smates@0123456789abcdef0123456789abcdef01234567 # v1.2.3
        with:
          verify: attestation   # 任意: 正しい来歴のないものを拒否する
```

アクションは、そのコミットをちょうど指すリリースタグを（自身のチェックアウトのタグから、なければ API トークンなしの `git ls-remote` で）探し、そのリリースのバイナリをインストールします。アテステーションがある場合は、そのコミットを指していなければなりません。リリースではないコミット（またはリリースタグが複数付いたコミット）は、通知を出したうえでソースからビルドするため、ランナーに Go（`actions/setup-go`）が必要です。

### 影響がないときは重いセットアップを省く { #skip-expensive-setup-when-nothing-is-affected }

同じジョブで先に `select` を実行します。

```yaml
      - id: select
        uses: dotneet/smates@vX.Y.Z
        with:
          command: select
      - if: steps.select.outputs.skip != 'true'
        run: docker compose up -d && npx playwright install --with-deps
      - if: steps.select.outputs.skip != 'true'
        uses: dotneet/smates@vX.Y.Z
```

### 選択結果でジョブ全体をゲートする { #gate-a-whole-job-on-the-selection }

選択だけを行う軽いジョブを別に用意します。

```yaml
jobs:
  select:
    runs-on: ubuntu-latest
    outputs:
      skip: ${{ steps.select.outputs.skip }}
    steps:
      - uses: actions/checkout@v5
      - id: select
        uses: dotneet/smates@vX.Y.Z
        with:
          command: select
  test:
    needs: select
    # select ジョブが成功し、何も選択されなかったと言ったときだけスキップする。
    if: ${{ !cancelled() && (needs.select.result != 'success' || needs.select.outputs.skip != 'true') }}
    runs-on: ubuntu-latest
    steps:
      # ... チェックアウト、ツールチェーン、そして smates のステップ
```

条件はこの形で書いてください。`!cancelled()` がないと、select ジョブが失敗したときに `needs:` のジョブはスキップされます。GitHub はスキップされたジョブを成功として扱うので、そのジョブを必須チェックにしていると、テストを 1 本も実行しないまま緑になります。`!cancelled()` があれば、select ジョブが失敗してもテストジョブはゲートがない場合と同じように実行されます（そのときは自分で選択します）。必須のステータスチェックでジョブがスキップされても問題ない場合だけ使ってください。

### スイートごとにジョブをゲートする { #gate-jobs-per-suite }

`suites` 出力は、スイートごとに何か選択されたかを示します。スイートを 1 つずつ実行するジョブ（`args: --suite <name>`）を、1 つずつスキップできます。

```yaml
jobs:
  plan:
    runs-on: ubuntu-latest
    outputs:
      suites: ${{ steps.plan.outputs.suites }}
    steps:
      - uses: actions/checkout@v5
      - id: plan
        uses: dotneet/smates@vX.Y.Z
        with:
          command: select
  api:
    needs: plan
    if: >-
      ${{ !cancelled() && (needs.plan.result != 'success' ||
      fromJSON(needs.plan.outputs.suites || '{}')['api'].skip != true) }}
    runs-on: ubuntu-latest
    steps:
      # ... そして args: --suite api と cache-key: api を付けた smates のステップ
```

ジョブをスキップするのは、出力がそのスイートで何も選択されなかったと明示しているときだけにします。

- **`.skip != true` でゲートし、`.selected > 0` や `.selected != 0` は使わないでください。** 出力にないスイート（名前の誤り、`--suite` で外したスイート、空の出力）は `null` になり、ワークフローの式は `null` を `0` として扱います。`selected` の条件ではジョブがスキップされますが、`null != true` なら実行されます。
- **plan ジョブが成功しなかったときは実行します**（`needs.plan.result != 'success'`。一緒にスキップされないよう `!cancelled()` も付けます）。smates は自身のエラー（設定、環境）で 2 から 4 で終了し、そのとき出力は何も言いません。
- `|| '{}'` は、出力が空のときに `fromJSON` が失敗しないようにします。

smates はスイートをまたいで選択します。共有パッケージへの変更は、それに依存するすべてのスイートのテストを選択します。手書きの path filter はこうした依存先を見落とすので、通常はそれより多く実行されます。path filter を残すなら、smates が選択したスイートを決してスキップしない粗い外側のゲートとしてだけ使ってください。

**必須のステータスチェック。** スキップされたジョブは成功として数えられます。集約ジョブ 1 つを必須チェックにするなら、plan がそう言ったときだけスキップを受け入れるようにします。

```yaml
  tests-passed:
    needs: [plan, api, web]
    if: ${{ always() }}
    runs-on: ubuntu-latest
    steps:
      - env:
          PLAN: ${{ needs.plan.result }}
          SUITES: ${{ needs.plan.outputs.suites }}
          API: ${{ needs.api.result }}
          WEB: ${{ needs.web.result }}
        run: |
          ok() { # result suite
            [ "$1" = success ] && return 0
            [ "$1" = skipped ] && [ "$PLAN" = success ] &&
              [ "$(jq -r --arg s "$2" '.[$s].skip' <<<"$SUITES")" = true ]
          }
          ok "$API" api && ok "$WEB" web
```

### シャードとマトリクス { #shards-and-matrices }

`--shard I/N` は、スイートと ID の安定したハッシュで各テストをシャードに割り当てます。そのため、あるテストは常に同じシャードに入り、各シャードがどのステートを復元していても割り当ては一致します。ジョブ同士がステートを上書きし合わないよう、シャードごとに別の `cache-key` を付けてください。

```yaml
    strategy:
      matrix:
        shard: [1, 2, 3, 4]
    steps:
      # ...
      - uses: dotneet/smates@vX.Y.Z
        with:
          args: --shard ${{ matrix.shard }}/4
          cache-key: shard-${{ matrix.shard }}
```

Node のバージョンなどのマトリクスでも同じです（`cache-key: node-${{ matrix.node }}`）。ランナーの OS はすでにキーに含まれています。

シャード実行ではエージェントを呼びません（[理由](../guides/agent.md#sharded-runs)）。各シャードは担当分にオーファンのポリシーを適用します。シャードでエージェントを使うには、シャードより前に（シャードなしで）`smates select` を 1 回実行し、そのステートを全シャードに復元させます。各シャードはキャッシュされた判断を再利用します。

### モノレポ { #monorepos }

smates のステップ 1 つで、リポジトリのすべてのスイートをカバーできます（リポジトリルートの `smates.yml` に列挙します）。スイートを複数のジョブに分けるなら、ジョブごとに `args: --suite <name>` と別の `cache-key` を使います。各ジョブは、ステップ 1 つの場合にそのスイートについて選択したはずのものを選択します。

- 別のスイートのテストが依存する変更は、そのジョブではオーファンになりません。
- ジョブが対象外にしたスイートのルート配下の変更も同様です（そのスイートを実行するジョブが扱います）。
- デフォルトブランチでの `--suite <name> --all` は全件実行と同じくスナップショットを記録するので、プルリクエストはそれ以降の変更だけを見ます。

### シャドーモード { #shadow-mode }

`args: --shadow` はすべてのテストを実行し、選択していたらどうなったかを報告します。選択を信頼する前に確信を得るために使います。導入の手順は [シャドーモード](../guides/trust.md#shadow-mode) を参照してください。

### プルリクエストのコメント { #pull-request-comment }

`comment: true` にすると、変更が何に影響し、それはなぜかをレビュアーに伝えるコメントを 1 つ投稿し、push のたびに更新します。

```yaml
jobs:
  test:
    permissions:
      contents: read
      pull-requests: write   # コメント用
    steps:
      # ...
      - uses: dotneet/smates@vX.Y.Z
        with:
          comment: true
```

コメントには次の内容が入ります。

- どのスイートのいくつのテストユニットが影響を受けるか、スキップしたテストに記録上どれだけ時間がかかっていたか。
- 変更された各ファイルについて、どのテストを選択し、それがどう依存しているか（静的な import、前回の実行でトレースした読み取り、スイートの入力、ディレクトリの一覧など）。
- ほかの理由で選択されたテスト（前回失敗、記録なし、エージェントが選んだ）。
- どのテストにも紐づかない各変更がどう扱われたか（テストが名前で参照、エージェント、`orphans` ポリシー）。
- `run` では結果と、シャドーモードなら見逃し。

長い一覧は件数付きで省略されます。詳細:

- `command: run` と `command: select` で使えます。コメントは隠しマーカー（`<!-- smates-summary -->`）で見つけ直すので、プルリクエストごとに 1 つです。
- 1 つのプルリクエストに複数の smates ジョブ（マトリクスのエントリ、ジョブに分けたスイート）がある場合、それぞれが `comment-key` を設定しない限り、互いのコメントを上書きします。
- **シャード**はそれぞれ自分のコメントを投稿し（キーに `shard-I-of-N` が付きます）、そのシャードが担当するテストを扱います。シャードごとに `cache-key` を分けると各シャードのステートは自分のテストしか知らないので、あるシャードがほかのシャードの分まで語ることはできません。シャードごとの詳細より 1 つのコメントが大事なら、`comment: true` 付きのシャードなしの `command: select` ジョブを別に追加してください（そのジョブには記録されたステートがなく、ベースブランチに対する静的解析で選択するので、`fetch-depth: 0` を付けます）。
- コメントがジョブを失敗させることはありません。フォークからのプルリクエスト（トークンが読み取り専用。まとめはジョブのページにあります）、トークンに `pull-requests: write` がない場合、`pull_request` / `pull_request_target` 以外のイベント、ランナーに `gh` CLI がない場合（GitHub ホストのランナーにはあります）は、通知を出してスキップします。
- アクションの外では、`smates run --markdown file` と `smates select --format markdown` が同じテキストを出力します。

### 自前のストレージ { #your-own-storage }

`cache: false` にして `.smates/` を自分で管理します。たとえば `smates state pull` / `smates state push` を使います（[その他の CI](other-ci.md) を参照）。

## 検証 { #verification }

リリースのアーカイブと `checksums.txt` には、smates のリリースワークフローが署名した [ビルドの来歴のアテステーション](https://docs.github.com/actions/security-for-github-actions/using-artifact-attestations) が付いています。アーカイブと同じリリースからダウンロードする `checksums.txt` とは違い、リリースを編集できるだけの人には作れません。`dotneet/smates` の `.github/workflows/release.yml` を、そのリリースのタグに対して GitHub ホストのランナーで実行したときの短命な証明書で署名されているからです。

アクションは `github-token` を使い、`gh attestation verify` で検証します。

```bash
gh attestation verify <archive> --repo dotneet/smates \
  --signer-workflow dotneet/smates/.github/workflows/release.yml \
  --source-ref refs/tags/<version> --deny-self-hosted-runners
  # アクションをコミットで固定した場合はさらに --source-digest <commit>
```

| `verify` | 動作 |
|---|---|
| `auto`（デフォルト） | チェックサムに加え、`gh` で検証でき、アーカイブにアテステーションがあればそれも検証します。アテステーション導入前に公開されたリリースにはないので、通知を出してチェックサムのみになります。リリースがアクション自身のもの（`uses: dotneet/smates@v1.2.3` かそのコミットで、`version` の入力なし）なら、アテステーションがあると分かっているので、ない場合はジョブを失敗させます。使える `gh` がない場合（一部のセルフホストランナー）は、通知を出してチェックサムのみになります。 |
| `attestation` | チェックサムとアテステーションを検証します。それ以外（`gh` がない、アテステーションがない、古いリリース）はすべてジョブを失敗させます。 |
| `checksum` | `checksums.txt` のみ（アテステーション導入前の動作）。 |

アテステーションが存在するのに検証できない場合は、チェックサムの不一致と同じく必ずジョブを失敗させます。検証に失敗した後で別の方法やソースからのビルドにフォールバックすることはありません。`version: source` は何もダウンロードしないので、検証するものもありません。

**キャッシュからのインストールも検証し、ネットワークを使います。** キャッシュがヒットした場合も含め、毎回リリースから `checksums.txt` をダウンロードします（`verify` が `checksum` でなければアテステーションも照会します）。そのためキャッシュがヒットしてもオフラインでは動きません。コストは実行ごとに小さなリクエスト 1 つです。

??? note "詳細: バイナリのキャッシュの検証方法"
    キャッシュのエントリ（セルフホストランナーではツールキャッシュも）が持つのはリリースのアーカイブだけです。それは `PATH` に入れないステージング用のディレクトリに復元され、アクションはそこからアーカイブだけを取り出して、新たにダウンロードした場合と同じ検証にかけます。その後、`smates` のバイナリだけを入れた新しいディレクトリに展開して、それを `PATH` に置きます。そのため `PATH` には検証済みのバイナリしかなく、本物のアーカイブを含まないキャッシュエントリは使われずに検証で失敗します。

    ただし、これでキャッシュを信頼できない入力として扱えるわけではありません。ほかの `actions/cache` の復元と同様、細工されたエントリは自分のパスの外にファイルを書けるので（復元は絶対パスで展開します）、たとえばアクション自身のスクリプトを上書きできます。ステートと同様に、ジョブが復元するキャッシュのスコープに書き込める人は信頼されている必要があります（[ステートは入力](#security-model) を参照）。

## 補足 { #notes }

- **チェックアウトの深さ。** ステートがあれば浅いクローンで十分です。コールドスタート（ステートなし）のとき、smates は履歴があればベースブランチとの差分を取ります（`fetch-depth: 0`）。履歴がなければすべてを実行します。
- **フォーク。** フォークからのプルリクエストにはシークレットが渡らないので、エージェントは使われず `orphans` ポリシーが適用されます。選択自体は機能します。
- **Windows。** アクションと CLI は `windows-latest` で動きます（ステップは Git Bash で実行されます）が、トレースを検証しているのは Linux と macOS だけです。
- **ランナーへの引数。** `--` より後ろの引数はテストランナーに渡りますが、テストを絞り込むことがあるため（`-t`、`-k`、`-run`）、こうした実行ではテストを成功として記録しません。常に使うランナーのフラグは、`smates.yml` のスイートの `command` に書いてください。
- **ストレージ。** 新しいキャッシュエントリは、ステートが変わったときだけ保存されます。7 日間使われなかったエントリは GitHub が削除します。ステートは小さな gzip ファイルです。
- **デバッグ。** `args: --explain` は各テストが選択された理由を表示します。プルリクエストで毎回すべてが実行されるなら、「Restore smates state」ステップのログでキャッシュがヒットしているか、`state-path` が `smates.yml` と一致しているかを確認してください。

## セキュリティモデル { #security-model }

- **エージェントはテストを追加することしかできません。** 差分やファイルの内容はエージェントにとって信頼できない入力であり（エージェントにもそう伝えています）、操作された回答でも、最悪の場合オーファンのテストを追加しそこなうだけです。無視されたファイル（`.env` など）は、シンボリックリンク経由も含めて、エージェントのツールからは読めません。git 管理外でも無視されていないファイルは、差分の一部としてモデルに送られることがあります。
- **認証情報は smates の手元に留まります。** `ANTHROPIC_API_KEY` / `ANTHROPIC_AUTH_TOKEN`、`OPENAI_API_KEY`、`OPENROUTER_API_KEY` は、エージェントがどのプロバイダーを使うかにかかわらず、テストプロセスの環境から取り除かれます。
- **ステートは入力です。** 何をスキップするかはステートが決めるので、ジョブが復元するキャッシュに書き込める人はテストをスキップさせられます。プルリクエストでは、それはプルリクエスト自身であり、その作成者はワークフローやテストも同じように変えられます。デフォルトブランチの保証は、すべてを実行するデフォルトブランチ自身の実行から来ています。保護されたブランチを守るジョブで、信頼できないジョブが書いたステートを復元しないでください。
- `smates.yml` の **`agent.command`** は、テストのコマンドと同じく、リポジトリが決めたコマンドラインを実行します。

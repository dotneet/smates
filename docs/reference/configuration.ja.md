# smates.yml { #smatesyml }

`smates.yml` のすべてのキーについて、型・デフォルト・意味をまとめたページです。スイートの設定手順は [スイートの設定](../guides/configuration.md) を読んでください。

smates はリポジトリルートにある `smates.yml`、`smates.yaml`、`.smates.yml`、`.smates.yaml` のうち最初に見つかったもの（または `--config` で指定したファイル）を読みます。どれもなければスイートを自動検出します（`smates list` で確認でき、`smates init` で書き出せます）。

## 全体の例 { #full-example }

```yaml
version: 1
suites:
  - name: web
    runner: vitest
    root: packages/web
    tests: ["src/**/*.test.ts"]        # デフォルト: ランナーの設定から読む
    exclude: ["src/legacy/**"]
    command: "pnpm exec vitest run"     # 選択したファイルを末尾に追加（{tests} があればそこに置換）
    inputs: ["/proto/**"]              # "/" = リポジトリルート。それ以外は root からの相対
    rules:                             # トレースでは見えない依存
      - tests: ["src/db/**"]
        deps: ["migrations/**"]
    env: { TZ: UTC }
    isolate: true                      # 記録中はテストファイルごとに 1 プロセス
    junit: reports/junit.xml           # 自前で出力する JUnit レポートからファイルごとの結果を得る
  - name: api
    runner: go
    root: services/api
    granularity: func                  # テスト関数ごとに 1 ユニット（デフォルト: package）
    record_jobs: 4                     # func のとき同時に記録する関数の数（デフォルト 1）
  - name: e2e
    runner: custom
    root: e2e
    command: "npx playwright test"
    depends_on:                        # テストが通信するサーバーのコード
      - api                            # = {suite: api}
      - paths: ["docker-compose.yml"]  # リポジトリルートからのグロブ
        tests: ["admin/**"]            # これらのテストだけ（root からの相対）
    serves: ["services/worker/**"]     # depends_on: [{paths: [...]}] の省略形
  - name: tools
    runner: pytest
    root: tools
    python_roots: ["src"]              # 静的解析用の追加の import ルート
  - name: legacy
    runner: unittest
    root: legacy
    command: "python3 -m unittest discover -s tests -p '*_check.py'"

orphans: agent                         # agent | all | ignore
untested: skip                         # skip | orphan
ignore: ["design/**"]                  # 組み込みのデフォルトに追加
inputs: [".github/workflows/test.yml"] # すべてのスイートのすべてのテストが依存する
env_ignore: ["BUILD_*", DEPLOY_ID]     # "*" で環境変数の追跡を無効化
env_track: [LANG]                      # デフォルトで無視される変数を追跡する
full_run_branches: [main, release]     # デフォルト: デフォルトブランチ
state: .smates/state.json.gz
state_seed: true
state_shared: false
flaky:
  retry: 0                             # 0..5
agent:
  enabled: auto                        # auto | always | never
  provider: auto                       # auto | anthropic | openai | openrouter
  # model: claude-haiku-5-5            # デフォルトはプロバイダーごとに異なる
  effort: low
  max_turns: 12
  timeout: 120s
  instructions: "Files under deploy/ configure production only."
  explain_failures: true
  explain_max: 3
  # command: "my-agent --json"         # 組み込みの代わりに外部のエージェントを使う
```

未知のキーはエラーです。値が不正な場合も同様で、smates はキー名を示して終了コード 3 で終了します（[終了コード](exit-codes.md)）。`smates config set` と `smates config add-ignore` はファイルを編集し、書き込む前に検証します（[コマンド](cli.md#smates-config)）。

!!! note "編集で再実行されるもの"
    スイートのエントリを編集すると、そのスイートのテストが再実行されます。各テストは、書かれたとおりのスイートの設定（とトップレベルの `env_track`）に依存しているためです。それ以外のトップレベルのキー（`ignore`、`orphans`、`agent` など）や別のスイートを編集しても、何も再実行されません。`smates.yml` 自体を読むテストは、通常どおりその内容に依存します。

## トップレベルのキー { #top-level-keys }

| キー | 型 | デフォルト | 意味 |
|---|---|---|---|
| `version` | int | `1` | 設定フォーマットのバージョン。`1` のみ対応しています。 |
| `suites` | [スイート](#suite-keys) のリスト | 設定ファイルがなければ自動検出 | テストスイート。スイートのない設定ファイルは、スイートが必要なコマンドでエラーになります。 |
| `ignore` | グロブのリスト（リポジトリルートから） | `[]` | オーファン（どのテストにも紐づかない変更）として扱わない変更。[組み込みのデフォルト](#built-in-ignore-globs) に追加されます。そのファイルを実際に読むテストは、変更されれば選択されます。[どのテストにも紐づかない変更](../guides/orphans.md) を参照してください。 |
| `inputs` | グロブのリスト（リポジトリルートから） | `[]` | すべてのスイートのすべてのテストが依存するファイル: CI のワークフロー、ツールチェーンのバージョン指定、共有のフィクスチャなど。 |
| `orphans` | `agent`、`all`、`ignore` | `agent` | どのテストにも紐づかない変更の扱い。[エージェント](../guides/agent.md) に尋ねる（使えないか失敗したら `all` にフォールバック）、影響しうるスイートを丸ごと実行する、何もしない、のいずれかです。`smates run --orphans` で 1 回の実行に限り上書きできます。 |
| `untested` | `skip`、`orphan` | `skip` | どのテストも import も実行もしていないソースファイルの変更の扱い。`skip` はテストされていないコードとして報告し、何も実行しません。`orphan` はほかのオーファンと同じく扱います。 |
| `env_ignore` | 名前またはグロブのリスト | `[]` | 変更されても何も選択しない環境変数。[組み込みのリスト](../guides/environment.md#variables-ignored-by-default) に追加されます。`"*"` で環境変数の追跡を無効にします。パターンは `path.Match` の構文で、大文字小文字を区別します。 |
| `env_track` | 名前またはグロブのリスト | `[]` | 組み込みの無視リストに含まれていても追跡する変数（例: `LANG`）。 |
| `full_run_branches` | ブランチ名のリスト | デフォルトブランチ | CI 上の `smates run`（プルリクエストではなく push のビルド）がすべてのテストを実行するブランチ。 |
| `state` | パス（リポジトリルートから） | `.smates/state.json.gz` | ステートファイルの場所。名前が `.gz` なら gzip で圧縮されます。 |
| `state_seed` | bool | `true` | ステートのないリンクされた git worktree で、空から始める代わりにメイン worktree のステート（またはリモートのステート）から始めます。`SMATES_NO_STATE_SEED=1` でも無効にできます。[並行する worktree](../guides/worktrees.md) を参照してください。 |
| `state_shared` | bool | `false` | テストごとのエントリを git の共通ディレクトリ配下の 1 ファイルに置き、すべての worktree で共有します。スナップショットは worktree ごとに持ちます。 |
| `flaky` | [マップ](#flaky) | | flaky と分かっているテストの扱い。 |
| `agent` | [マップ](#agent) | | オーファンと失敗ヒントのために smates が呼ぶ LLM エージェント。 |

## スイートのキー { #suite-keys }

`suites:` の 1 エントリです。

| キー | 型 | デフォルト | 意味 |
|---|---|---|---|
| `name` | string | ランナー名。`root` が `.` でなければ `root` | `--suite`、`--only`、`depends_on`、出力で使う名前。一意である必要があります。 |
| `runner` | `vitest`、`jest`、`node`、`bun`、`pytest`、`unittest`、`go`、`custom` | **必須** | スイートの実行方法とトレース方法。[言語とランナー](../guides/runners.md) を参照してください。 |
| `root` | パス（リポジトリルートから） | `.` | コマンドを実行するディレクトリ。リポジトリ内である必要があります。 |
| `command` | string | ランナーから推定 | ベースとなるコマンドライン。選択したテストファイルを末尾に追加するか、コマンドに `{tests}` があればそこに置き換えます。`custom` では必須です。 |
| `tests` | グロブのリスト（`root` から） | ランナーの設定から読む。読めなければランナーのデフォルト | どのファイルがテストファイルか。[どのファイルがテストか](../guides/configuration.md#which-files-are-tests) を参照してください。 |
| `exclude` | グロブのリスト（`root` から） | `[]` | テストファイルとして扱わないファイル。ランナーの設定で除外されるものに追加されます。 |
| `inputs` | グロブのリスト | `[]` | スイートのすべてのテストが依存するファイル。`root` からの相対で、先頭に `/` を付けるとリポジトリルートからになります。 |
| `rules` | [ルール](#rules) のリスト | `[]` | 一部のテストについて、トレースでは見えない依存。 |
| `depends_on` | [依存](#depends_on) のリスト | `[]` | テストがプロセス境界を越えて使うコード（docker compose で起動するサーバー、HTTP API など）。[サーバーを叩くテスト](../guides/servers.md) を参照してください。 |
| `serves` | グロブのリスト（リポジトリルートから） | `[]` | `depends_on: [{paths: [...]}]` の省略形。 |
| `env` | 文字列から文字列へのマップ | `{}` | スイートのテストに渡す環境変数。シェルの値より優先され、値を変えるとスイートが再実行されます。 |
| `isolate` | bool | `false` | 記録中、テストファイルごとに別プロセスで実行します。`vitest`、`jest`、`node`（`--test-isolation=none` のとき）、`unittest` が対応しています。`bun` は常にファイルごとに 1 プロセスで記録します。 |
| `record_jobs` | int（0 以上） | 意味の欄を参照 | ファイルごとに 1 プロセスで記録するとき、同時に動かすプロセス数。`bun` と、ファイルを順に実行するコマンド（`--runInBand`、`--no-file-parallelism`、`--test-isolation=none`、`--test-concurrency=1`）では `1`、それ以外は CPU 数です。`granularity: func` では、1 つのパッケージのテスト関数を同時にいくつ記録するか（デフォルト `1`）。`0` はデフォルトの意味です。 |
| `granularity` | `package`、`func` | `package` | Go のみ。`func` にするとトップレベルのテスト関数がそれぞれ 1 ユニットになります（`function` も別名として使えます）。[Go のテスト関数単位の選択](../guides/go-test-functions.md) を参照してください。 |
| `static` | bool | `true` | トレースに加えて静的な import グラフを使います。 |
| `junit` | パス（`root` から） | | コマンドが書き出す JUnit XML のレポート。ランナーがファイルごとの成否を報告できない場合に、smates はここから読み取ります。 |
| `python_roots` | パスのリスト（`root` から） | `[]` | Python の静的解析で使う追加の import ルート（src レイアウトの `src` など）。 |
| `env_ignore` | 名前またはグロブのリスト | `[]` | トップレベルのキーと同じで、このスイートだけに追加されます。 |
| `env_track` | 名前またはグロブのリスト | `[]` | トップレベルのキーと同じで、このスイートだけに追加されます。 |
| `volatile` | グロブのリスト（`root` から） | `[]` | 何が変わっても毎回実行するテストユニット（理由 `volatile`）。日付など smates が追跡できないものに依存するテスト向けです。ルールの `tests` と同じく照合します。ユニット ID に加え、Go のテスト関数（`pkg#TestName`）ではパッケージディレクトリとテストファイルにも一致します。コンパイルチェックには適用されません。 |
| `max_age` | 期間（`168h`、`30m`） | `0`（期限なし） | 前回記録した実行がこれより古いテストユニットは、何が変わっていなくても再実行します（理由 `expired`）。 |

### rules { #rules }

| キー | 型 | 意味 |
|---|---|---|
| `tests` | グロブのリスト（スイートの `root` から） | ルールを適用するテスト。 |
| `reaches` | グロブ（リポジトリルートから）または `$NAME` パターンのリスト | 静的 import の閉包がグロブに一致するファイルに届くテストだけにルールを適用します。`$NAME` の項目（`$DB_*` のような変数名のグロブ）は、前回記録した実行でその環境変数を読んだテストに一致します。`tests` と併用すると、両方に一致するテストだけが対象です。 |
| `deps` | グロブのリスト（リポジトリルートから） | それらのテストが依存するファイル。**必須**です。 |

ルールには `tests` か `reaches` が必要です。グロブ先頭の `./` は取り除かれます。

```yaml
rules:
  - tests: ["src/db/**"]
    deps: ["migrations/**"]
  # データベースのテストヘルパーを import するテストすべて（置き場所を問わない）
  - reaches: ["pkg/shared/dbtest/**", "$APP_DB_*"]
    deps: ["database/migrations/**"]
```

`reaches` はファイル単位の閉包をたどります。ほかのヘルパーや、Go パッケージのほかのテストファイルが import するものも含みます。`granularity: func` ではパッケージ単位で照合します。smates が判断できないときはルールを適用します。静的解析が無効（`static: false`）なとき、テストのファイルを読むアナライザがないとき、そして `$NAME` の項目でテストにまだ記録された実行がないときです。ルールが挙げていない経路でデータベースに届くテスト（自前の `sql.Open`、リストにないヘルパー、子プロセス）は deps を得ないので、入口をすべて挙げ、データベース接続が読む変数も加えてください。`smates doctor` はこうしたルールごとに適用されるテストユニットの数を表示し、1 つにも適用されないルールを警告します。[データベースを使うテスト](../guides/configuration.md#tests-backed-by-a-database) を参照してください。

### depends_on { #depends_on }

各エントリは `suite` と `paths` のどちらか一方だけを持ちます。文字列だけを書くとスイート名になります。

| キー | 型 | 意味 |
|---|---|---|
| `suite` | スイート名 | テストがそのスイートのコードに依存します。そのスイートのルート配下のファイルから、（どのスイートのものでも）テストファイル、無視・ベンダリングされたファイル、配下にルートを持つスイートのファイルを除いたものです。自分自身には依存できません。 |
| `paths` | グロブのリスト（リポジトリルートから） | テストがこれらのファイルに依存します。先頭の `/` や `..` は使えません。 |
| `tests` | グロブのリスト（スイートの `root` から） | 依存を持たせるテストを絞り込みます（デフォルト: すべて）。 |

グロブはどこでも区切りに `/` を使います（`**` は任意の深さに一致します）。`depends_on` と `serves` のグロブ先頭の `./` は取り除かれます。

## agent { #agent }

オーファンについて、また失敗した実行の後にいくつかの失敗を説明するために、smates が呼ぶ [LLM エージェント](../guides/agent.md) です。

| キー | 型 | デフォルト | 意味 |
|---|---|---|---|
| `enabled` | `auto`、`always`、`never` | `auto` | `never` でエージェントを無効にします。`auto` は認証情報（または `command`）があれば使います。`always` は現状 `auto` と同じ動作です。 |
| `provider` | `auto`、`anthropic`、`openai`、`openrouter` | `auto` | 組み込みのバックエンド。`auto` は Anthropic、OpenAI、OpenRouter のうち API キーが設定されている最初のものを使います（[smates が読む環境変数](environment-variables.md#agent-credentials)）。 |
| `model` | string | プロバイダーごと: `claude-haiku-5-5`、`gpt-6-luna`、`anthropic/claude-haiku-5.5` | モデル名。 |
| `effort` | `low`、`medium`、`high` | `low` | 推論の effort。受け付けないモデルでは送りません。 |
| `max_turns` | int | `12` | 1 回の呼び出しでのツール利用のターン数。`0` はデフォルトの意味です。 |
| `timeout` | duration（`120s`、`2m`） | `120s` | 1 回の呼び出しの実時間の上限。 |
| `instructions` | string | | エージェントのシステムプロンプトに追加する、プロジェクト固有のヒント。 |
| `command` | string | | 組み込みの代わりに外部のエージェントを実行します。プロンプトを stdin で受け取り、`decision` フィールドを持つ JSON オブジェクトを stdout に出力します。 |
| `explain_failures` | bool | `true` | 失敗したテストのありそうな原因を `smates run` がエージェントに尋ねます（[失敗ヒント](../guides/failure-hints.md)）。組み込みのバックエンドのみ。 |
| `explain_max` | int（0 以上） | `3` | 1 回の実行で説明する失敗テストの数。`0` はデフォルトの意味なので、説明を止めるには `explain_failures: false` を使います。 |

## flaky { #flaky }

| キー | 型 | デフォルト | 意味 |
|---|---|---|---|
| `retry` | int、`0` から `5` | `0` | あるスイートの実行の失敗がすべて flaky と分かっているテストだった場合、同じテストでその実行を最大この回数まで繰り返します。それで成功すれば、反転を報告したうえで実行は失敗扱いになりません。`0` は失敗を決して隠しません。[シャドーモード・flaky・統計](../guides/trust.md) を参照してください。 |

## 組み込みの ignore グロブ { #built-in-ignore-globs }

次の変更は、`ignore:` に加えて、オーファンとして扱われません（それを読むテストは、変更されれば選択されます）。一覧は [internal/config/config.go](https://github.com/dotneet/smates/blob/main/internal/config/config.go) の `DefaultIgnore` です。

| 種類 | グロブ |
|---|---|
| ドキュメント | `**/*.md`、`**/*.mdx`、`**/*.rst`、`docs/**`、`doc/**` |
| ライセンスと所有者 | `**/*.txt.license`、`**/LICENSE*`、`**/CHANGELOG*`、`**/CODEOWNERS` |
| GitHub のメタデータ | `.github/ISSUE_TEMPLATE/**`、`.github/PULL_REQUEST_TEMPLATE*`、`.github/dependabot.yml` |
| エディターと git | `.vscode/**`、`.idea/**`、`.editorconfig`、`.devcontainer/**`、`**/.gitignore`、`**/.gitattributes` |
| フォーマッターとリンター | `**/.prettierrc*`、`**/.prettierignore`、`**/.eslintrc*`、`**/eslint.config.*`、`**/.markdownlint*`、`**/biome.json`、`**/biome.jsonc`、`**/.golangci.y*ml`、`**/ruff.toml`、`**/.pre-commit-config.yaml` |

smates が追跡しない環境変数の組み込みリスト（`DefaultEnvIgnore`）は [環境変数](../guides/environment.md#variables-ignored-by-default) で説明しています。

## 関連項目 { #see-also }

- [スイートの設定](../guides/configuration.md)
- [コマンド](cli.md)
- [どのテストにも紐づかない変更](../guides/orphans.md)

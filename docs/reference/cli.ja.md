# コマンド { #commands }

`smates` のすべてのコマンドとフラグを用途別にまとめたページです。オプションを調べるときに使ってください。いつ、なぜ使うのかはガイドで説明しています。

| コマンド | 内容 |
|---|---|
| [`smates run`](#smates-run) | 影響を受けるテストを選択し、トレースしながら実行して、ステートを更新する |
| [`smates select`](#smates-select) | 何も実行せずに選択結果だけを表示する |
| [`smates watch`](#smates-watch) | 変更のたびに影響を受けるテストを再実行する |
| [`smates why`](#smates-why) | ファイルや環境変数に依存するテストを一覧する |
| [`smates deps`](#smates-deps) | テストが依存するファイルを一覧する |
| [`smates list`](#smates-list) | スイートとテストユニットを一覧する |
| [`smates doctor`](#smates-doctor) | セットアップを診断する |
| [`smates flaky`](#smates-flaky) | flaky と証明されたテストを一覧する |
| [`smates stats`](#smates-stats) | 節約できた時間、シャドーモードでの見逃し、オーファン、エージェントの利用状況を集計する |
| [`smates orphans`](#smates-orphans) | 最近のコミットを再生し、`ignore:` に追加するグロブを提案する |
| [`smates init`](#smates-init) | `smates.yml`（必要なら GitHub のワークフローも）を書き出す |
| [`smates config`](#smates-config) | `smates.yml` を安全に編集する |
| [`smates guide`](#smates-guide) | コーディングエージェント向けのガイドを表示する |
| [`smates state`](#smates-state) | 記録されたステートの確認・移動・削除 |
| [`smates hook`](#smates-hook) | git の pre-push / pre-commit フック、または Claude Code や Codex の Stop フックを設定する |
| [`smates version`](#smates-version-and-help) | バージョンを表示する |

各コマンドのフラグは `smates <command> -h` で確認できます（終了コード 0）。`config`、`state`、`hook` ではサブコマンドの一覧も表示されます。フラグはダッシュ 1 つでも 2 つでもかまいません（`-all` と `--all` は同じです）。フラグは位置引数の前にも後ろにも置けます（`smates why src/a.ts --format json`）。`--` 以降はフラグとして扱いません。`run`、`select`、`watch` では、`--` より後ろの引数はテストランナーに渡されます。

## 共通フラグ { #common-flags }

プロジェクトを読み込むコマンド（`run`、`select`、`watch`、`why`、`deps`、`list`、`doctor`、`flaky`、`orphans`）は次のフラグを共有します。コマンド名より前に置くグローバルフラグはありません。

| フラグ | 値 | デフォルト | 意味 |
|---|---|---|---|
| `-C` | dir | カレントディレクトリ | `dir` で起動したものとして動作します。 |
| `--config` | file | リポジトリルートの `smates.yml` | 読み込む設定ファイル。smates は `smates.yml`、`smates.yaml`、`.smates.yml`、`.smates.yaml` の順に探します。 |
| `--state` | file | `.smates/state.json.gz`（または `smates.yml` の `state:`） | 読み書きするステートファイル。 |
| `--suite` | name | すべてのスイート | 対象をそのスイートに絞ります。複数指定できます。存在しない名前は終了コード 3 になります。 |
| `-v` | | オフ | 詳細な出力（`run` / `select` では、`--explain` と同様に選択されたテストと理由を表示）。 |

`--format` を持つコマンドはどれも `text`（デフォルト）か `json` を受け付けます。例外は形式の多い `smates select` です。JSON のレポートは stdout に、ログは stderr に出力されます。

終了コードはすべてのコマンドで共通です。[終了コード](exit-codes.md) を参照してください。

## テストを実行する { #running-tests }

### smates run { #smates-run }

```bash
smates run [flags] [-- runner args]
```

変更が影響しうるテストを選択し、トレースしながら実行して、各テストが使ったものを記録します。CI でデフォルトブランチ（または [`full_run_branches`](configuration.md#top-level-keys) に挙げたブランチ）への push をビルドするときは、代わりにすべてのテストを実行します。[選択の仕組み](../getting-started/how-it-works.md) も参照してください。

| フラグ | 値 | デフォルト | 意味 |
|---|---|---|---|
| `--all` | | オフ | すべてのテストを実行し、記録されたステートを書き直します。 |
| `--select` | | オフ | デフォルトでは全件実行になる場面（CI のデフォルトブランチ）でもテストを選択します。 |
| `--base` | rev | 自動 | まだ記録のないテストを比較する git リビジョン。デフォルトはプルリクエストのマージ先またはデフォルトブランチとのマージベースです。 |
| `--explain` | | オフ | 各テストが選択された理由を表示します（`[changed: src/price.ts]`）。 |
| `--only` | `SUITE:UNIT` | | このテストユニットだけを実行し、通常どおり記録します。複数指定できます。ほかのテストや変更は次回の実行に持ち越されます。 |
| `--add` | `SUITE:UNIT` | | このテストも実行します。ユニットにはテストファイルのパスも使えます。そのユニットを持つスイートが 1 つだけなら `SUITE:` は省略できます。複数指定できます。テストを外すことはありません。どのテストにも一致しない名前は終了コード 2 になります。 |
| `--orphans` | `agent`、`full`、`skip` | `smates.yml` の `orphans:` | どのテストにも紐づかない変更の扱いを、この実行に限って指定します。エージェントに尋ねる、影響しうるスイートを丸ごと実行する、ポリシーを適用しない（明示的なオプトアウト）のいずれかです。[どのテストにも紐づかない変更](../guides/orphans.md) を参照してください。 |
| `--no-agent` | | オフ | エージェントを呼びません。紐づかない変更には影響しうるスイートを丸ごと実行し、失敗の説明も行いません。 |
| `--no-record` | | オフ | トレースせず、ステートも更新せずに実行します。 |
| `--shadow` | | オフ | すべてのテストを実行し、選択していたら見逃していた失敗テストを報告します。`--shard` と一緒なら、そのシャードが担当するテストをすべて実行します。[シャドーモード](../guides/trust.md#shadow-mode) を参照してください。 |
| `--shard` | `I/N` | | 選択されたテストのうち、`N` 分割した `I` 番目のシャードを実行します。テストはスイートとテスト ID の安定したハッシュでシャードに割り当てられます。割り当ては `N` によって変わるので、1 回の実行の全シャードは同じ `N` を使います。シャード実行ではエージェントを呼びません。 |
| `--fail-fast` | | オフ | あるバッチが失敗したら、それ以降のバッチを開始しません。テストは失敗しそうなものから順に実行されます。 |
| `--output` | `full`、`failures` | `$SMATES_OUTPUT`、なければ `full` | `full` はランナーの出力をそのまま流します。`failures` は失敗したテストの出力だけを表示し、最後にそれらを再実行するコマンド付きの短いまとめを出します。`agent` は `failures` の別名として受け付けます。 |
| `--report` | file | | JSON のレポート（選択結果、テストごとの結果、シャドーモードでの見逃し、`exit_code`）を `file` に書き出します。`-` を指定すると stdout に書き、ほかの出力はすべて stderr に回します。 |
| `--markdown` | file | | まとめを Markdown で `file` に書き出します（上書き）。プルリクエストのコメント用です。 |
| `--summary` | file | GitHub Actions では `$GITHUB_STEP_SUMMARY` | Markdown のまとめを `file` に追記します。 |
| `--budget` | duration | `90s` | 選択されたテストの記録済み所要時間の合計がこれを超えると、実行前に分割方法を表示します。`0` で無効になります。 |
| `--max-time` | duration | `0`（上限なし） | 記録済み所要時間から見て、これより後に終わるバッチは開始しません。外れたテストは選択されたまま次の実行に残ります。[時間の上限](#time-limit) を参照してください。 |
| `--dry-run` | | オフ | ランナーのコマンドを実行せずに表示します。 |

加えて [共通フラグ](#common-flags) も使えます。

次の組み合わせはエラー（終了コード 2）になります。

- `--only` と `--all`、`--shadow`、`--add`、`--orphans`
- `--shadow` と `--fail-fast`、`--max-time`
- `--dry-run` と `--report`、`--markdown`、`--summary`（代わりに `smates select --format json|markdown` を使います）

`--` より後ろの引数はすべてのスイートのランナーに渡されます（`smates run -- -t name`）。テストを絞り込む可能性があるため、こうした引数付きの実行ではテストを成功として記録しません。常に使うフラグはスイートの `command` に書いてください。

??? note "詳細: `--output failures` の出力内容"
    ランナーのバッチごとに出力をバッファし、バッチが失敗したときだけ表示します。失敗したテストごとの出力を報告できるランナー（vitest、jest、pytest、`go test`、JUnit）ではその部分だけを、それ以外ではバッチの出力を最大 200 行（先頭 30 行と末尾 170 行）表示します。smates 自身の進捗行は省略されます。出力のないまま 30 秒経つと `smates: still running <suite> (...)` を表示します。間隔は `SMATES_HEARTBEAT` で変えられます。[コーディングエージェントから使う](../guides/coding-agents.md) を参照してください。

#### 時間の上限 { #time-limit }

`--max-time 8m` を付けると、実行をおよそ 8 分以内に収めます。

- 各スイートのテストを、記録済みの所要時間をもとに、上限のおよそ 4 分の 1 ずつのバッチに分けます。所要時間が記録されていないテストは、そのスイートの平均として数えます。所要時間がまったく記録されていないスイートは 1 つのバッチのままで、使った時間が上限の半分未満のときだけ開始します。
- バッチはいつもどおり失敗しそうなものから順に開始します。各バッチの前に、上限より後に終わるかを確かめ、終わるならそれ以降は何も開始しません。
- 最初のバッチは必ず開始するので、どの実行でも少しは進みます。実行中のバッチを止めることはありません。
- 外れたテストは実行も記録もされず、選択されたまま残り、次の実行で先に走ります。ログ、サマリー（`Time limit`）、レポート（`max_time_reached`、`not_run`）に表示されます。

実行したものがすべて成功すれば、終了コードは 0 です。CI でマージの可否を決めるのには `--max-time` を使わないでください。外れたテストは実行されていません。Claude Code と Codex の Stop フックは、デフォルトで `--max-time 8m` を使います（[git フック](../guides/git-hooks.md#claude-code-stop-hook) を参照）。

#### 前提条件を満たさないスイート { #unmet-requires }

[`requires`](configuration.md#requires) を満たさないスイートのユニットは開始しません。選択されたまま残り、記録もされません。ログには失敗したチェックがそれぞれ表示されます。テストが失敗していなければ終了コードは 4、失敗していれば 1 です。レポートでは、チェックが `unmet_requires` に、ユニットが `not_run` に入ります。

### smates select { #smates-select }

```bash
smates select [flags]
```

何も実行せずに選択結果を表示します。エージェントを呼ぶことはあり、その場合は回答をステートに保存するので、同じ変更に対する次の `smates run` はそれを再利用します。

| フラグ | 値 | デフォルト | 意味 |
|---|---|---|---|
| `--format` | `text`、`files`、`json`、`github`、`markdown` | `text` | 出力形式（下表）。 |
| `--all` | | オフ | すべてを選択します。 |
| `--base` | rev | 自動 | `run` と同じです。 |
| `--explain` | | オフ | 各テストが選択された理由を（stderr に）表示します。 |
| `--add` | `SUITE:UNIT` | | `run` と同じです。テストを追加した結果を事前に確認できます。 |
| `--orphans` | `agent`、`full`、`skip` | `smates.yml` の `orphans:` | `run` と同じです。 |
| `--no-agent` | | オフ | エージェントを呼びません。 |
| `--shard` | `I/N` | | 選択結果のうち `I/N` のシャード分だけを対象にします。 |
| `--max-shards` | `N`（1〜256） | | 選択に合わせたシャード数も求めます。何も選択されなければ `0`、それ以外は選択されたテストの記録済み実行時間を `--shard-time` で割って切り上げ、`1` から `N` に収めた値です。[全件実行するブランチ](configuration.md#top-level-keys)への push のビルドでは、`smates run` がそこで全テストを実行するので、全テストを前提に求めます。[シャード数を選択に合わせる](../ci/github-actions.md#size-the-shards-to-the-selection) を参照してください。`--shard` とは併用できません。 |
| `--shard-time` | 期間 | `10m` | `--max-shards` のとき: 1 シャードあたりの記録済みテスト時間の目安。実行時間が記録されていない選択済みテストはスイートの中央値として数えます。選択されたテストがあり、実行時間が 1 つも記録されていないスイートがあれば `N` になります。 |
| `--markdown` | file | | Markdown のまとめを `file` にも書き出します。 |

加えて [共通フラグ](#common-flags) も使えます。

| `--format` | stdout の内容 |
|---|---|
| `text` | 選択されたテスト ID を 1 行に 1 つ。選択のまとめは stderr に出ます。 |
| `files` | 選択されたテスト ID を 1 行に 1 つ。stderr には何も出ません。 |
| `json` | 選択結果のすべて: `selected`、`total`、`selected_tests`（スイートごと）、テストごとの理由、`orphan_candidates`、エージェントの判断、`--max-shards` のときは `shards`。 |
| `github` | `selected`、`total`、`run-all`、`skip`、`tests`、`suites`（`--max-shards` のときは `shards` と `shard-matrix` も）をステップの出力として `$GITHUB_OUTPUT` に書き込みます（未設定なら stdout）。まとめは `$GITHUB_STEP_SUMMARY` に追記されます。[出力](../ci/github-actions.md#outputs) を参照してください。 |
| `markdown` | プルリクエストのコメントに使う Markdown のまとめ。 |

### smates watch { #smates-watch }

```bash
smates watch [flags] [-- runner args]
```

作業ツリーをポーリングし、変更のたびに影響を受けるテストを再実行します。stdin が端末のときはキー操作ができます: `a` で全件実行、`f` で失敗したテストを再実行、Enter で再実行、`q` で終了。[ウォッチモード](../guides/watch.md) を参照してください。

| フラグ | 値 | デフォルト | 意味 |
|---|---|---|---|
| `--interval` | duration | `500ms` | 作業ツリーをポーリングする間隔。正の値が必要です。 |
| `--debounce` | duration | `300ms` | この時間ファイルの変更が止まるまで待ってから実行します。 |
| `--agent` | | オフ | 紐づかない変更についてエージェントに尋ねます。指定しなければエージェントなしでオーファンのポリシーを適用します。 |
| `--clear` | | オフ | 実行のたびに画面をクリアします。 |
| `--fail-fast` | | オフ | 各回の実行で、あるバッチが失敗したら以降のバッチを開始しません。 |

加えて [共通フラグ](#common-flags) も使えます。

## 調べる { #inspecting }

### smates why { #smates-why }

```bash
smates why [flags] <file | $VAR>
```

ファイルに依存するテストと、その依存の種類（`static`、`recorded`、`depends_on` の宣言など）を一覧します。`'$TZ'` のように指定すると（シェルに展開されないようクォートします）、環境変数 `TZ` を読むテストを一覧します。

フラグ: `--format text|json` と [共通フラグ](#common-flags)。

### smates deps { #smates-deps }

```bash
smates deps [flags] <test unit>
```

1 つのテストユニットの依存を一覧します。テストユニットはテストファイル、Go のパッケージディレクトリ、`granularity: func` のスイートでは `pkg#TestName` です。各行には、その依存が `static`（静的解析、`inputs`、`rules`、`depends_on`）か `recorded`（テスト実行中に観測）か、またはその両方かが付きます。テストユニットでない名前は終了コード 3 になります。

フラグ: `--format text|json` と [共通フラグ](#common-flags)。

### smates list { #smates-list }

```bash
smates list [flags]        # 別名: smates ls
```

スイートごとに 1 行、名前、ランナー、ルート、ユニット数、コマンド、テストのパターンの出どころ（`tests=jest.config.js testMatch`）を表示します。`-v` を付けるとパターンとすべてのユニットも表示します。smates が読めなかった設定は、検出の警告として stderr に出ます。JSON のレポートには `tests_source`、`test_patterns`、`units`、`discovery_warning` が含まれます。

フラグ: `--format text|json` と [共通フラグ](#common-flags)。

### smates doctor { #smates-doctor }

```bash
smates doctor [flags]
```

セットアップを診断し、チェックごとに 1 行表示します。失敗したチェックがあると終了コード 1 になります（警告だけなら 0 です）。JSON では各チェックに安定した `id`、`status`（`ok`、`warn`、`fail`）、`message`、`fix`（`command`、`config` のキーと値、または `doc`）が付きます。

| チェックの `id` | 確認する内容 |
|---|---|
| `git-installed`、`repository`、`project-load`、`config` | git が `PATH` にあり、リポジトリと `smates.yml` を読み込めること |
| `suites-detected`、`suite-units` | スイートがあり、それぞれにテストファイルが一致すること |
| `suite-tests-source`、`suite-test-discovery` | テストのパターンの出どころと、smates が読めなかったランナー設定 |
| `suite-launcher`、`suite-dependencies-installed` | スイートのコマンドを起動できること、`node_modules` がインストールされていること |
| `toolchain-go`、`toolchain-bun`、`toolchain-python`、`suite-engines-node` | ツールチェーンと、`engines.node` に対する Node のバージョン |
| `service-reachable`、`local-server-reachable`、`env-var-set` | コードが使うサービスと `localhost` の URL、コードが読むのに設定されていない環境変数 |
| `suite-server-dependency` | エンドツーエンドテストに見えるのに `depends_on`、`serves`、`rules` のどれもないスイート |
| `suite-rule-reaches` | `reaches` を持つルールごとに適用されるテストユニットの数。1 つにも適用されないルールは警告 |
| `state-recorded` | 記録されたステートがあるか、どのコミットのものか |
| `state-skipped` | 前回の実行で自分をスキップしたテストユニット（未証明なので、成功するまで毎回実行される） |
| `agent`、`ci` | エージェントが使えるか、検出した CI と、デフォルトで全件実行になるか |
| `workflow-test-step` | smates を使わずにテストを実行している GitHub ワークフローのステップ |

フラグ: `--format text|json` と [共通フラグ](#common-flags)。

### smates flaky { #smates-flaky }

```bash
smates flaky [flags]
smates flaky --clear [unit...]
```

入力が何も変わっていないのに結果が反転したテストを、その証拠（反転回数、最後のコミット、直近の結果、読んだ環境変数）とともに一覧します。[シャドーモード・flaky・統計](../guides/trust.md) を参照してください。

| フラグ | 値 | デフォルト | 意味 |
|---|---|---|---|
| `--clear` | | オフ | 指定したユニット（指定がなければすべて）の flaky の証拠を消去します。ユニットの指定は `--clear` のときだけ受け付けます。 |
| `--format` | `text`、`json` | `text` | 出力形式。 |

加えて [共通フラグ](#common-flags) も使えます。

### smates stats { #smates-stats }

```bash
smates stats [flags]
```

ステートに記録された実行を集計します: スキップ率、節約できた記録済みテスト時間（30 日換算も）、シャドーモードでの見逃し、扱い別のオーファン、エージェントの呼び出しとトークン・コスト、flaky の反転。

| フラグ | 値 | デフォルト | 意味 |
|---|---|---|---|
| `--since` | when | すべて | 指定した日付（`2026-01-02`）、RFC 3339 の時刻、または経過時間（`30d`、`2w`、`12h`）以降の実行だけを対象にします。 |
| `--branch` | branch | すべて | そのブランチでの実行だけを対象にします。 |
| `--ci` | | オフ | CI での実行だけを対象にします。 |
| `--recent` | n | `5` | テキスト出力で直近 `n` 件の実行を一覧します。 |
| `--format` | `text`、`json` | `text` | 出力形式。 |
| `-C` | dir | カレントディレクトリ | `dir` で起動したものとして動作します。 |
| `--state` | file | `.smates/state.json.gz` | ステートファイル。 |

`smates stats` には `--config`、`--suite`、`-v` はありません。

### smates orphans { #smates-orphans }

```bash
smates orphans [flags]
smates orphans --apply [--dry-run]
```

最近の first-parent のコミットを現在の依存マップに照らして再生し、オーファン（どのテストにも紐づかない変更）になっていたはずのファイルを順位付けして、そのまま貼り付けられる `ignore:` ブロックと、`inputs:` や `rules:` の候補を表示します。`smates run --all` の後に実行してください。[どのテストにも紐づかない変更](../guides/orphans.md) を参照してください。

| フラグ | 値 | デフォルト | 意味 |
|---|---|---|---|
| `--history` | n | `100` | 直近 `n` 件の first-parent コミットを再生します。`0` は無制限です。 |
| `--since` | rev | | `rev` より後のコミットを再生します。`--history` を明示しなければ件数の上限はありません。 |
| `--apply` | | オフ | 提案された `ignore:` のグロブを `smates.yml` に追加します（それ以外は変更しません）。コメントとレイアウトは保たれます。 |
| `--dry-run` | | オフ | `--apply` と併用し、書き込まずに差分を表示します。 |
| `--format` | `text`、`json` | `text` | 出力形式。 |

加えて [共通フラグ](#common-flags) も使えます。

## セットアップ { #setup }

### smates init { #smates-init }

```bash
smates init [flags]
```

リポジトリのスイートを検出して `smates.yml` を書き出し、`.gitignore` に `.smates/` を追加します。既存の設定は `--force` なしでは上書きしません。既存の設定がある状態で `--github` や `--agents-md` を指定すると、その部分だけを実行します。[クイックスタート](../getting-started/quickstart.md) を参照してください。

| フラグ | 値 | デフォルト | 意味 |
|---|---|---|---|
| `--github` | | オフ | `.github/workflows/smates.yml` も書き出します。既存のワークフローがすでにテストを実行している場合は、代わりにそのテストステップを smates に置き換えるパッチを表示します。 |
| `--patch` | file | 表示する | `--github` と併用し、そのパッチを `file` に書き出します（`git apply` 用）。 |
| `--agents-md` | | オフ | コーディングエージェント向けの短い smates の節を `AGENTS.md`（または `CLAUDE.md`）に追加します。 |
| `--force` | | オフ | 既存の `smates.yml` を上書きします。 |
| `--stdout` | | オフ | 書き出さずに設定を表示します。 |
| `--format` | `text`、`json` | `text` | `json` はスイート、書き出したファイル、`needs_review` の項目（根拠と推奨される変更付き）を報告します。 |
| `-C` | dir | カレントディレクトリ | `dir` で起動したものとして動作します。 |

### smates config { #smates-config }

```bash
smates config add-ignore <glob>... [flags]
smates config add-input <glob>... [flags]
smates config set <key> <value> [flags]
```

YAML を手で編集せずに `smates.yml` を変更します。コメントとレイアウトは保たれ、書き込む前に結果を検証します。フラグは引数の後ろにも置けます。

| サブコマンド | 効果 |
|---|---|
| `add-ignore <glob>...` | トップレベルの `ignore:` に追加します（重複は除きます）。 |
| `add-input <glob>...` | トップレベルの `inputs:` に追加します（重複は除きます）。 |
| `set <key> <value>` | スカラー値のキーを設定します: `version`、`orphans`、`untested`、`state`、`state_seed`、`state_shared`、`flaky.retry`、`agent.enabled`、`agent.provider`、`agent.model`、`agent.max_turns`、`agent.timeout`、`agent.command`、`agent.effort`、`agent.instructions`、`agent.explain_failures`、`agent.explain_max`。 |

| フラグ | 値 | デフォルト | 意味 |
|---|---|---|---|
| `--dry-run` | | オフ | 差分を表示し、何も書き込みません。 |
| `--format` | `text`、`json` | `text` | 出力形式。 |
| `--config` | file | リポジトリルートの `smates.yml` | 編集する設定ファイル。存在している必要があります（先に `smates init` を実行してください）。 |
| `-C` | dir | カレントディレクトリ | `dir` で起動したものとして動作します。 |

`-` で始まるグロブの前には `--` を置いてください。

### smates guide { #smates-guide }

```bash
smates guide
```

コーディングエージェント向けの 1 ページのガイド（[エージェント向けガイド](../AGENTS.md)）を表示します。フラグはありません。

### smates version と help { #smates-version-and-help }

```bash
smates version      # smates --version、smates -v でも可
smates help         # smates -h、smates --help でも可
```

コマンド名より前の `-v` はバージョン表示、コマンド名より後ろの `-v` は詳細出力の意味になります。

## ステート { #state }

### smates state { #smates-state }

```bash
smates state show [--format json]
smates state path
smates state pull [--remote origin] [--ref REF]
smates state push [--remote origin] [--ref REF]
smates state merge [-o file] <state file>...
smates state clear
```

| サブコマンド | 効果 |
|---|---|
| `show` | ステートの場所（共有か、どこからシードされたか）、記録したコミット、スナップショットの大きさ、スイートごとの件数を表示します。 |
| `path` | ステートファイルのパスを表示します。 |
| `pull` | git の ref からステートを取得して保存します。`--ref` がなければ `refs/smates/state/<branch>`、次にデフォルトブランチの ref を試します。`state_shared: true` では取得したエントリをマージします。 |
| `push` | ステートを専用のコミットとして、リモートの `refs/smates/state/<branch>`（または `--ref`）に push します（強制 push、フックは実行されません）。 |
| `merge` | 指定したステートファイルをステート（または `-o file`）にマージします。 |
| `clear` | ステートを削除します。リンクされた worktree では以後シードされません。`state_shared: true` ではすべての worktree のステートが消えます。 |

| フラグ | 値 | デフォルト | 意味 |
|---|---|---|---|
| `--remote` | name | `origin` | `pull` / `push`: git のリモート。 |
| `--ref` | ref | `refs/smates/state/<branch>` | `pull` / `push`: git の ref。 |
| `-o` | file | ステートファイル | `merge`: 出力先のファイル。 |
| `--format` | `text`、`json` | `text` | `show`: 出力形式。 |
| `--json` | | オフ | `show`: `--format json` の非推奨の別名。 |
| `--state` | file | `.smates/state.json.gz` | ステートファイル。 |
| `-C` | dir | カレントディレクトリ | `dir` で起動したものとして動作します。 |

ブランチは `$SMATES_BRANCH`、なければ CI がビルドしているブランチ、なければチェックアウト中のブランチです。いずれもない detached HEAD では、`push` に `--ref` が必要です。[その他の CI](../ci/other-ci.md) と [並行する worktree](../guides/worktrees.md) を参照してください。

## フック { #hooks }

### smates hook { #smates-hook }

```bash
smates hook install [--hook pre-push|pre-commit] [--append] [--agent] [-- run flags]
smates hook install --claude [--user] [--agent] [-- run flags]
smates hook install --codex [--agent] [-- run flags]
smates hook uninstall [--hook ...] | [--claude [--user]] | [--codex]
smates hook status
```

`install` は `smates run --no-agent`（と `--` 以降の run のフラグ）を実行する git フックを書き出し、選択されたテストが失敗したら push やコミットを止めます。`--claude` を付けると代わりに `.claude/settings.json` に、`--codex` を付けると `.codex/hooks.json` に Stop フックを追加します。`uninstall` は smates が書いた部分だけを取り除きます。`status` は git フックと、Claude Code と Codex のフックを表示します。[git フックと Claude Code](../guides/git-hooks.md) を参照してください。

| フラグ | 値 | デフォルト | 意味 |
|---|---|---|---|
| `--hook` | `pre-push`、`pre-commit` | `pre-push` | 対象の git フック。`--claude`、`--codex` とは併用できません。 |
| `--append` | | オフ | `install`: 既存のフックスクリプトの末尾に smates を追加します。`--claude`、`--codex` とは併用できません。 |
| `--agent` | | オフ | `install`: フックからエージェントを呼べるようにします（指定しなければ `--no-agent` を渡します）。 |
| `--claude` | | オフ | git フックの代わりに、`.claude/settings.json` の Claude Code Stop フックを対象にします。 |
| `--codex` | | オフ | git フックの代わりに、`.codex/hooks.json` の Codex Stop フックを対象にします。`--claude`、`--user` とは併用できません。 |
| `--user` | | オフ | `--claude` と併用し、`~/.claude/settings.json` を使います（`smates.yml` のあるすべてのプロジェクトで有効）。 |
| `-C` | dir | カレントディレクトリ | `dir` で起動したものとして動作します。 |

インストールされたフックは、次の内部サブコマンドを呼びます。通常、自分で実行することはありません。

- `smates hook run pre-push|pre-commit [-- run flags]`: git フックが実行するもの。
- `smates hook claude-stop|codex-stop [--require-config] [-- run flags]`: Claude Code と Codex の Stop フックが実行するもの（`--output failures --max-time 8m` が暗黙に付きます。run のフラグに `--max-time` があればそちらが優先されます）。テストが失敗すると `{"decision": "block", ...}` を表示して 0 で終了し、smates 自身のエラーでは 1 で終了します。時間の上限や満たされない [`requires`](configuration.md#requires) で外れたテストではブロックしません。それらについて stderr に 1 行表示し、0 で終了します。

いずれも、`SMATES_SKIP_HOOK` が `0` と `false` 以外の値に設定されていれば何もしません。

## 関連項目 { #see-also }

- [smates.yml](configuration.md): すべての設定キー
- [終了コード](exit-codes.md)
- [smates が読む環境変数](environment-variables.md)

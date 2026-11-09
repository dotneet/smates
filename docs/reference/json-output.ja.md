# JSON 出力 { #json-output }

smates がプログラムやコーディングエージェント向けに出力する JSON を、フィールドごとにまとめたページです。JSON は stdout に（`run --report` は `-` でなければそのファイルに）、ログは stderr に出力されます。

| コマンド | 出力 |
|---|---|
| [`smates select --format json`](#smates-select) | 選択結果 |
| [`smates run --report <file>`](#smates-run-report) | 選択結果と実行結果 |
| [`smates stats --format json`](#smates-stats) | 記録された実行の集計 |
| [`smates flaky --format json`](#smates-flaky) | flaky と証明されたテスト |
| [`smates init --format json`](#smates-init) | init が書き出したものと、判断が必要な項目 |
| [`smates why`、`deps`、`list --format json`](#smates-why-deps-list) | 依存、スイート、ユニット |
| [`smates doctor --format json`](#smates-doctor) | 診断結果 |
| [`smates state show --format json`](#smates-state-show) | 記録されたステート |
| [`smates orphans --format json`](#smates-orphans) | 最近のコミットのオーファン |
| [`smates config ... --format json`](#smates-config) | `smates.yml` の編集結果 |

表の読み方:

- 「省略可」のフィールドは、空（または `false`、`0`）のときは出力されません。
- スイート名やテスト ID がキーになるオブジェクトがあります: `{"web": {"src/a.test.ts": "pass"}}`。
- 時刻は RFC 3339 の文字列です。所要時間はミリ秒の整数です（`_ms`）。

## smates select { #smates-select }

`smates select --format json` は、何も実行せずに選択結果を出力します。

```json
{
  "mode": "state",
  "suites": [
    {
      "name": "go",
      "tests": [
        {
          "id": "cart",
          "selected": true,
          "reason": "changed",
          "changed": ["price/price.go"],
          "via": {"price/price.go": "import"},
          "duration_ms": 481
        }
      ],
      "all": true
    }
  ],
  "changed": ["notes.txt", "price/price.go"],
  "orphans": ["notes.txt"],
  "orphan_handling": {"notes.txt": "policy"},
  "selected": 1,
  "total": 1,
  "selected_tests": {"go": ["cart"]},
  "orphan_candidates": [
    {"path": "notes.txt", "status": "added", "referenced_by": [], "suites": ["go"], "handling": "policy"}
  ]
}
```

| フィールド | 型 | 意味 |
|---|---|---|
| `mode` | 文字列 | 選択のしかた。[モード](#modes) を参照してください。 |
| `suites` | 配列 | スイートごとに 1 つ。[スイートとテスト](#suites-and-tests) を参照してください。 |
| `changed` | 文字列の配列、または `null` | 選択で調べた変更ファイル。なければ `null` です。 |
| `orphans` | 文字列の配列、または `null` | どのテストも依存していない変更ファイル（[どのテストにも紐づかない変更](../guides/orphans.md) を参照）。なければ `null` です。 |
| `left_out` | 文字列の配列、省略可 | どのテストも依存していない変更のうち、`--suite` で外したスイートだけに属するもの（この実行のオーファンではありません）。 |
| `orphan_handling` | オブジェクト、省略可 | オーファン → その扱い。[オーファンの扱い](#orphan-handling) を参照してください。 |
| `notes` | 文字列の配列、省略可 | ログにも出る選択についての注記（記録されたステートがない、呼び出し側がオーファンをスキップした、など）。 |
| `selected` | 整数 | 選択されたテストユニットの数。 |
| `total` | 整数 | テストユニットの数。 |
| `selected_tests` | オブジェクト | スイート → 選択されたテストユニットの ID（なければ空の配列）。 |
| `orphan_candidates` | 配列 | オーファンごとに 1 つ。オーファンを自分で判断する呼び出し側（`--add`、`--orphans`）向けです。[オーファンの候補](#orphan-candidates) を参照してください。 |
| `agent` | オブジェクト、省略可 | エージェントに尋ねたとき、またはキャッシュした回答を使ったときの、エージェントの判断。[エージェントの判断](#agent-decision) を参照してください。 |
| `agent_note` | 文字列、省略可 | エージェントを使わなかった理由（使えない、失敗した、シャード実行）。 |
| `shards` | 整数、`--max-shards` のときのみ | 選択に合わせて求めたシャード数（`0` でも出力されます。何も選択されなかったときです）。 |

### モード { #modes }

| `mode` | 意味 |
|---|---|
| `state` | 記録されたステートに照らして選択しました。 |
| `diff` | 記録されたステートがないので、ベースリビジョンに対する静的解析で選択しました。 |
| `all` | すべてのテスト: `--all`、CI の全件実行のビルド、または記録されたステートもベースリビジョンもない場合。 |

### スイートとテスト { #suites-and-tests }

`suites` の各要素:

| フィールド | 型 | 意味 |
|---|---|---|
| `name` | 文字列 | スイート名。 |
| `tests` | 配列 | スイートのすべてのテストユニット（選択されたかどうかによらず）。 |
| `all` | 真偽値 | スイートのすべてのテストユニットが選択されていれば `true`。 |

`tests` の各要素:

| フィールド | 型 | 意味 |
|---|---|---|
| `id` | 文字列 | テストユニットの ID（テストファイル、Go のパッケージディレクトリ、または `pkg#TestName`）。 |
| `selected` | 真偽値 | 選択されたかどうか。 |
| `reason` | 文字列 | 理由。[選択の理由](#selection-reasons) を参照してください。選択されていないユニットでは `unaffected` です。 |
| `detail` | 文字列、省略可 | 理由の補足（`last run: fail`、`references <file>`、エージェントの理由）。 |
| `changed` | 文字列の配列、省略可 | 選択のきっかけになった変更された依存。ファイル、または環境変数の `$NAME` です。 |
| `via` | オブジェクト、省略可 | `changed` の要素 → それがテストに届いた経路。[経路](#via) を参照してください。 |
| `service` | オブジェクト、省略可 | `service` 経由で届いた `changed` の要素 → その宣言（`depends_on suite api`、`serves backend/**`）。 |
| `duration_ms` | 整数、省略可 | そのユニットについて記録された所要時間。 |
| `shard` | 整数、省略可 | `--shard` か `--max-shards` のとき: そのユニットを担当するシャード（1 から）。 |
| `priority` | 整数、省略可 | `smates run` で、そのユニットをどれだけ早く開始するか。`1` は前回成功しなかったもの、`2` はテストファイルかそれが直接 import するファイルが変わったもの、`3` は依存するほかのものが変わったもの（間接的な import、トレースしたデータ、設定）と、理由が `reference`、`agent`、`caller`、`orphan` のもの、`4` はそれ以外の理由（`new`、`volatile`、`expired`、`unproven`、`only`、`all`、`rerun`）のものです。 |

### 選択の理由 { #selection-reasons }

`reason` の値です。`--explain` での表示は [テストが選択された理由](../getting-started/how-it-works.md#why-a-test-was-selected) を参照してください。

| `reason` | 意味 |
|---|---|
| `all` | 全件実行。 |
| `new` | ステートにそのテストのエントリがありません。 |
| `changed` | テストが前回実行されてから入力が変わりました。 |
| `failed` | 前回成功しませんでした。 |
| `unproven` | 前回テストが自分をスキップしたので、成功が証明されていません。 |
| `volatile` | スイートの [`volatile`](configuration.md#suite-keys) に挙げられていて、毎回実行されます。 |
| `expired` | 最後に記録された実行が、スイートの [`max_age`](configuration.md#suite-keys) より古くなっています。 |
| `orphan` | オーファンのポリシーでスイートを丸ごと実行しました。 |
| `reference` | テストがオーファンのファイルの名前を含んでいます。 |
| `agent` | エージェントが追加しました。 |
| `diff` | diff モード: テストが静的に依存するファイルが、ベースリビジョンから変わりました。 |
| `rerun` | `smates watch` が前回の選択を再実行しました。 |
| `only` | `smates run --only` で指定されました。 |
| `caller` | `--add` で追加されました。 |
| `unaffected` | 選択されていません。 |

### 経路 { #via }

`via` の値です。

| `via` | 変更された依存 |
|---|---|
| `test` | テスト自身のファイル |
| `input` | スイートまたはトップレベルの入力、または `smates.yml` のルール |
| `service` | `depends_on` または `serves` の宣言に含まれるもの（どの宣言かは `service` に入ります） |
| `import` | 静的な import グラフ上のもの |
| `traced` | 以前の実行のトレースで記録されたもの（読んだファイル、ディレクトリの一覧、コマンド） |
| `reference` | テストが名前を含むオーファンのファイル |
| `orphan` | オーファンのポリシーによるオーファン |

### オーファンの扱い { #orphan-handling }

`orphan_handling`（と `orphan_candidates` の `handling`）の値です。

| 値 | 意味 |
|---|---|
| `reference` | そのファイルの名前を含むテストを選択しました。 |
| `agent` | エージェントが判断しました。 |
| `no-impact` | エージェントが、どのテストにも関係ないと判断しました。 |
| `policy` | オーファンのポリシーで、影響しうるスイートを丸ごと実行しました。 |
| `ignored` | `orphans: ignore`。 |
| `skipped` | 呼び出し側がオーファンのポリシーを適用しませんでした（`--orphans=skip`）。 |
| `untested` | どのテストからも届かないソースコード（`untested: skip`）。 |

### オーファンの候補 { #orphan-candidates }

`orphan_candidates` の各要素:

| フィールド | 型 | 意味 |
|---|---|---|
| `path` | 文字列 | オーファンのファイル。 |
| `status` | 文字列 | `added`、`modified`、`deleted` のいずれか。 |
| `referenced_by` | 配列 | そのファイルの名前を含むテスト: `suite`、`test`、`file`（名前を含むファイル）、`selected`。 |
| `suites` | 文字列の配列 | そのファイルのためにオーファンのポリシーが丸ごと実行するスイート。 |
| `handling` | 文字列 | この実行での扱い（[値](#orphan-handling)）。扱われていなければ `""` です。 |

### エージェントの判断 { #agent-decision }

`agent` オブジェクト:

| フィールド | 型 | 意味 |
|---|---|---|
| `run_all` | 真偽値 | エージェントが全スイートの全テストを求めました。 |
| `add` | 配列 | 追加したテスト（`Test` が `""` ならスイート全体）。ほかのキーと違い、各要素のキーは大文字で始まります: `Suite`、`Test`、`Reason`、`Detail`、`Files`（省略可。きっかけになったオーファン）。 |
| `no_impact` | 文字列の配列 | どのテストにも関係ないと判断したオーファン。 |
| `summary` | 文字列 | 短い説明。 |
| `usage` | オブジェクト | `input_tokens`、`output_tokens`、`cache_read_tokens`、`cache_write_tokens`、`turns`。バックエンドがコストを報告したときは `cost_usd` も。 |
| `model` | 文字列 | モデル。 |
| `dropped` | 文字列の配列、省略可 | エージェントが挙げたが存在しないテストやスイート。 |

## smates run --report { #smates-run-report }

`smates run --report <file>` はこのレポートを `file` に書き出します（`-` なら stdout に書き、ほかの出力はすべて stderr に回します）。[`smates select`](#smates-select) のフィールドをすべて含み（`priority` は選択されたすべてのユニットに付きます。`not_run` に入ったものも含みます）、さらに次のフィールドがあります。

| フィールド | 型 | 意味 |
|---|---|---|
| `results` | オブジェクト | スイート → ユニット → 結果（実行したユニットのみ）: `pass`、`fail`、`skip`（テストが自分をスキップした）、`unknown`（結果を判断できなかった、または `--` 以降のランナー引数で絞り込まれた可能性がある）。 |
| `exit_code` | 整数 | ランナーの終了コードのうち最も悪いもの。テストが失敗せず環境の問題だったときは `4` です。[終了コード](exit-codes.md#in-reports-and-ci-outputs) を参照してください。 |
| `shadow` | 真偽値 | `--shadow` で実行しました。 |
| `escapes` | 文字列の配列 | `--shadow` のとき: 選択していたら見逃していた失敗テスト（`suite: unit`）。 |
| `possible_escapes` | 文字列の配列 | `--shadow` のとき: 実行が失敗したというだけで失敗扱いになった、選択されていないテスト（ランナーがファイルごとの結果を報告しない場合）。 |
| `flaky` | 配列 | この実行で見つかった反転。下表を参照してください。 |
| `failure_hints` | 配列 | 失敗したテストごとのヒント。[失敗ヒント](#failure-hints) を参照してください。 |
| `fail_fast` | 真偽値 | `--fail-fast` で実行しました。 |
| `not_run` | オブジェクト | スイート → 実行されなかった選択済みユニット（`--fail-fast`、`--max-time`、中断、満たされない `requires`、起動できなかったランナー）。選択されたまま残ります。 |
| `interrupted` | 真偽値 | 実行が中断されました（Ctrl-C、SIGTERM、SIGHUP）。終わらなかったものは `not_run` に入ります。 |
| `max_time_reached` | 真偽値 | `--max-time` で `not_run` のユニットが外れました（`unmet_requires` の分を除く）。[時間の上限](cli.md#time-limit) を参照してください。 |
| `unmet_requires` | オブジェクト | スイート → 満たされなかった [`requires`](configuration.md#requires) のチェック。そのユニットは `not_run` に入ります。 |

`flaky` の各要素:

| フィールド | 型 | 意味 |
|---|---|---|
| `suite`、`unit` | 文字列 | テストユニット。 |
| `flip` | 文字列 | `pass->fail` または `fail->pass`。 |
| `count` | 整数 | そのユニットについて記録された反転の回数。 |
| `retried` | 真偽値、省略可 | 既知の flaky なユニットが失敗し、再実行で成功したので（[`flaky.retry`](configuration.md#flaky)）、失敗として数えませんでした。 |

### 失敗ヒント { #failure-hints }

`failure_hints` の各要素（[失敗ヒント](../guides/failure-hints.md) を参照）:

| フィールド | 型 | 意味 |
|---|---|---|
| `suite`、`test` | 文字列 | 失敗したテストユニット。 |
| `inferred` | 真偽値、省略可 | 複数のユニットをまとめた実行の終了コードから導いた失敗です。別のテストのものかもしれません。 |
| `verdict` | 文字列 | `changed-inputs`（入力の一部が変わった。それが候補）、`unattributed-changes`（入力は変わっていないが、どのテストにも紐づかない変更がある）、`no-changed-inputs`、`no-record`（比較するものがない）のいずれか。 |
| `basis` | 文字列 | 何に対する「変更」か: `last-run`、`diff`（この実行の選択での変更）、`none`。 |
| `since` | 文字列、省略可 | 比較した記録済み実行のコミット。 |
| `last_status` | 文字列、省略可 | その実行の結果。 |
| `inputs` | 整数 | テストの既知の入力の数。 |
| `changed` | 整数 | そのうち変わった数。 |
| `candidates` | 配列 | 原因の候補（可能性の高い順）: `path`、`kind`（`source`、`test`、`data`、`env`、`config`、`listing`）、`link`（テストからの届き方: `test file`、`direct import`、`static import`、`suite input`、`traced`、`unattributed`、`server code`）、`change`（`modified`、`added`、`deleted`）。 |
| `more` | 整数、省略可 | 省略した候補の数。 |
| `output` | 文字列、省略可 | ランナーがそのテストについて報告した失敗の出力。 |
| `explanation` | オブジェクト、省略可 | エージェントの見立て: `file`、`line`、`reason`、`confidence`（`low`、`medium`、`high`）、`model`、`cached`。[エージェントによる説明](../guides/failure-hints.md#agent-explanations) を参照してください。 |

## smates stats { #smates-stats }

`smates stats --format json` は、ステートに記録された実行を集計します（[統計](../guides/trust.md#stats) を参照）。シャード実行では、シャードごとに 1 件の記録になります。

| フィールド | 型 | 意味 |
|---|---|---|
| `state` | 文字列 | ステートファイル。 |
| `since` | 時刻、省略可 | `--since` の起点。 |
| `branch` | 文字列、省略可 | `--branch` の指定。 |
| `from`、`to` | 時刻、省略可 | 期間内の最初と最後の実行。 |
| `runs` | 整数 | 期間内の実行の記録数。 |
| `shard_jobs` | 整数 | シャード実行の記録数。 |
| `by_mode` | オブジェクト | モード別の記録数。下表を参照してください。 |
| `by_source` | オブジェクト | 実行元別の記録数: `ci`、`local`、`watch`。 |
| `failed_runs` | 整数 | 終了コードが 0 以外の記録数。 |
| `selected_units` | 整数 | シャドーモード以外の実行で選択されたテストユニットの数。 |
| `total_units` | 整数 | 同じ実行でのテストユニットの総数。 |
| `skip_rate` | 数値 | `(total_units - selected_units) / total_units`。0 から 1 です。 |
| `saved_ms` | 整数 | スキップしたユニットの記録済み所要時間。 |
| `skipped_untimed_units` | 整数 | 所要時間が記録されていない、スキップしたユニットの数（`saved_ms` には含まれません）。 |
| `elapsed_ms` | 整数 | シャドーモード以外の実行にかかった時間。 |
| `saved_per_30d_ms` | 整数、省略可 | `saved_ms` を 30 日に換算したもの（期間が 1 日未満なら出力されません）。 |
| `shadow` | オブジェクト | シャドーモードの実行。下表を参照してください。 |
| `orphans` | オブジェクト | `files`（実行全体で数えたオーファン）、`runs`（オーファンがあった実行）、`by_handling`（扱い → 件数、[値](#orphan-handling)）。 |
| `agent` | オブジェクト | エージェントの呼び出し: `calls`、`cached`、`turns`、`input_tokens`、`output_tokens`、`cache_read_tokens`、`cache_write_tokens`、`cost_usd`（報告されたコストの合計）、`cost_reported_calls`（バックエンドがコストを報告した、キャッシュでない呼び出しの数）。 |
| `flaky` | オブジェクト | `flips`（期間内の実行が検出した反転）、`units`（現在ステートが flaky としているユニット）。 |

`by_mode` のキー:

| キー | 実行 |
|---|---|
| `state` | 記録されたステートに照らして選択した |
| `diff` | 記録されたステートがなく、ベースリビジョンに対する静的解析で選択した |
| `all` | すべてのテストを実行した（`--all`、CI の全件実行のビルド、または記録もベースリビジョンもない場合） |
| `shadow` | `--shadow` の実行。本来のモードではなくここで数えます |

`shadow` オブジェクト:

| フィールド | 型 | 意味 |
|---|---|---|
| `runs` | 整数 | シャドーモードの実行数（シャード実行は 1 回と数えます）。 |
| `runs_with_escapes` | 整数 | 選択していたら失敗テストを見逃していたシャドーモードの実行数。 |
| `failed_units` | 整数 | シャドーモードの実行で失敗したユニットの数。 |
| `escapes` | 整数 | そのうち、選択していたらスキップしていたユニットの数。 |
| `possible_escapes` | 整数 | 実行が失敗したというだけで失敗扱いになったユニットの数。 |
| `miss_rate` | 数値 | `escapes / failed_units`。0 から 1 です。 |

## smates flaky { #smates-flaky }

`smates flaky --format json` は `{"flaky": [...]}` を出力します。ステートが flaky としているユニットごとに 1 つです（なければ空の配列）。

| フィールド | 型 | 意味 |
|---|---|---|
| `suite`、`unit` | 文字列 | テストユニット。 |
| `count` | 整数 | 記録された反転の回数。 |
| `first`、`last` | 時刻 | 最初と最新の反転。 |
| `commit` | 文字列、省略可 | 最新の反転のコミット。 |
| `flip` | 文字列、省略可 | 最新の反転（`pass->fail`、`fail->pass`）。 |
| `status` | 文字列 | 最後に記録されたユニットの結果。 |
| `recent` | 文字列の配列 | 直近の結果（古い順）。`?` が付いたものは、複数のユニットをまとめた実行の終了コードから推定した結果です。 |
| `env` | 文字列の配列 | ユニットが読んだ環境変数。 |

## smates init { #smates-init }

`smates init --format json` は、init が行ったことと判断できなかったことを報告します。

| フィールド | 型 | 意味 |
|---|---|---|
| `config` | 文字列 | 設定ファイル。 |
| `written` | 真偽値 | `--stdout` のとき、または設定がすでにあったときは `false`。 |
| `config_yaml` | 文字列、省略可 | `--stdout` のとき: 書き出すはずだった設定。 |
| `files_written` | 文字列の配列 | `smates.yml`、`.gitignore`（`.smates/` を追加したとき）、ワークフロー、`--patch` のファイル、`AGENTS.md` または `CLAUDE.md`（`--agents-md` で節を追加したとき）。 |
| `suites` | 配列 | スイート: `name`、`runner`、`root`、`exclude`。 |
| `workflow` | オブジェクト、省略可 | `--github` が行ったこと。下表を参照してください。`--github` がないとき、`--stdout` のときは出力されません。 |
| `agents_md` | オブジェクト、省略可 | コーディングエージェント向けの節。下表を参照してください。リポジトリに `AGENTS.md` も `CLAUDE.md` もなく、`--agents-md` も指定しないとき、`--stdout` のときは出力されません。 |
| `needs_review` | 配列 | 人（またはエージェント）の判断が必要な項目。下表を参照してください。 |
| `next_steps` | 文字列の配列 | 次に実行するコマンド。 |

`workflow` オブジェクト:

| フィールド | 型 | 意味 |
|---|---|---|
| `file` | 文字列 | ワークフローのファイル。 |
| `written` | 真偽値 | 書き出したかどうか。 |
| `skipped` | 文字列、省略可 | 書き出さなかった理由。 |
| `patch` | 文字列、省略可 | 既存のワークフローのテストステップを smates のアクションに置き換えるパッチ。 |
| `patch_file` | 文字列、省略可 | `--patch` のとき: パッチを書き出したファイル。 |

`agents_md` オブジェクト:

| フィールド | 型 | 意味 |
|---|---|---|
| `file` | 文字列 | `AGENTS.md` または `CLAUDE.md`。 |
| `status` | 文字列 | `added`（`--agents-md` で書き出した）、`present`（ファイルにすでに節がある）、`suggested`（書き出していない。`snippet` が追加すべき節）。 |
| `snippet` | 文字列、省略可 | その節。 |

`needs_review` の各要素:

| フィールド | 型 | 意味 |
|---|---|---|
| `id` | 文字列 | 同じ指摘なら変わりません: ケバブケースの `<kind>-<対象>`。対象がないときは `<kind>` だけです（`no-suites`、`orphans-policy`）。 |
| `kind` | 文字列 | 下表を参照してください。 |
| `reason` | 文字列 | 見つかったこと。 |
| `evidence` | 文字列の配列 | 見つかった場所。 |
| `action` | オブジェクト | 推奨される対応: `description` と、該当するものだけ `command`（実行するコマンド）、`config`（`suites.web.depends_on` のようなドット区切りの `key` と、任意の JSON 値の `value`。通常は文字列、`depends_on` では配列）、`snippet` と `snippet_file`（スニペットを貼り付けるファイル）。 |

`kind` の値:

| `kind` | 意味 |
|---|---|
| `no-suites` | テストスイートが見つかりませんでした。 |
| `makefile-command` | Makefile のターゲットが、smates 自身のコマンドが渡さないフラグや変数でスイートのランナーを実行しています。 |
| `server-dependency` | 別プロセスのサーバーのエンドツーエンドテストに見えるのに、`depends_on` も `serves` もないスイートです。 |
| `service` | テストがサービスを必要としているようです（compose ファイル、または localhost で接続するコードから）。 |
| `local-server` | スイートが実行するファイルが、ポート 1024 以上の `localhost` の URL を呼んでいて、そのサーバーを自分で起動も横取りもしていません。それを使うテストにはそのサーバーの起動が必要です。 |
| `env` | `.env.example`（`.env.sample`、`.env.template`、`.env.dist`）に挙げられ、どのスイートの `env` も設定していない環境変数を、コードが読んでいます。 |
| `workflow-test-step` | ワークフローのステップが smates を使わずにテストを実行しています。 |
| `orphans-policy` | `orphans: agent` ですが、エージェントが使えません。 |
| `go-granularity` | 1 つのパッケージが Go スイートのテスト関数の大半を持つため、パッケージ単位の選択ではほとんど省けません。`granularity: func` を提案します。 |

## smates why、deps、list { #smates-why-deps-list }

`smates why <file | $VAR> --format json`:

| フィールド | 型 | 意味 |
|---|---|---|
| `target` | 文字列 | 調べたファイル（リポジトリからの相対パス）または `$VAR`。 |
| `tests` | 配列 | それに依存するテストユニット: `suite`、`test`、`kind`（`content`、`declarations`、`listing`、`existence`、`tree`: 対象の何に依存するか）、`how`（文字列の配列。テキスト出力と同じ表記: `static/inputs`、`recorded`、`depends_on` や `serves` の宣言など）。 |

`smates deps <test unit> --format json`:

| フィールド | 型 | 意味 |
|---|---|---|
| `suite`、`test` | 文字列 | テストユニット。 |
| `deps` | 配列 | その依存: `dep`（リポジトリからの相対パス、または `$VAR` のようなキー）と `sources`（`static` と `recorded` のどちらか、または両方。どちらでもない依存では空）。 |

`smates list --format json`:

| フィールド | 型 | 意味 |
|---|---|---|
| `config` | 文字列 | スイートの出どころの設定ファイル（自動検出なら `""`）。 |
| `suites` | 配列 | 下表を参照してください。 |

`suites` の各要素:

| フィールド | 型 | 意味 |
|---|---|---|
| `name`、`runner`、`root`、`command` | 文字列 | スイート。 |
| `unit_count` | 整数 | テストユニットの数。 |
| `units` | 文字列の配列 | テストユニット。 |
| `tests_source` | 文字列 | テストのパターンの出どころ: `smates.yml tests`、`runner defaults`、または読み取ったランナー設定（`jest.config.js testMatch`）。 |
| `test_patterns` | 文字列の配列 | パターン（ルートからの相対グロブ。除外は `!`、jest の正規表現は `regex ...`）。 |
| `discovery_warning` | 文字列、省略可 | ランナー設定を読めなかった理由（そのときはデフォルトのパターンを使います）。 |

## smates doctor { #smates-doctor }

`smates doctor --format json`:

| フィールド | 型 | 意味 |
|---|---|---|
| `version` | 文字列 | smates のバージョン。 |
| `ok` | 真偽値 | 失敗したチェックがあれば `false`（終了コードは 1 になります）。 |
| `checks` | 配列 | 下表を参照してください。 |

各チェック:

| フィールド | 型 | 意味 |
|---|---|---|
| `id` | 文字列 | 安定したケバブケースの ID。[チェックの一覧](cli.md#smates-doctor) を参照してください。 |
| `status` | 文字列 | `ok`、`warn`、`fail` のいずれか。 |
| `subject` | 文字列、省略可 | スイート、サービス、変数、ワークフローのステップごとに繰り返すチェックで、何についてのものか。 |
| `message` | 文字列 | 見つかったこと。 |
| `fix` | オブジェクト、省略可 | 解決方法: `command`、`config`（ドット区切りの `key` と、任意の JSON 値の `value`）、`doc`（URL）のいずれか、または複数。 |

## smates state show { #smates-state-show }

`smates state show --format json`:

| フィールド | 型 | 意味 |
|---|---|---|
| `path` | 文字列 | ステートファイル。 |
| `commit` | 文字列 | 記録されたスナップショットのコミット。 |
| `created` | 時刻 | ステートが作られた時刻。 |
| `tool` | 文字列 | 書き出した smates のバージョン。 |
| `snapshot_files` | 整数 | スナップショット内のファイル数。 |
| `suites` | オブジェクト | スイート → `tests`（記録されたユニット数）、`status`（結果 → 件数）、`avg_deps`（ユニットあたりの平均依存数）。 |
| `agent_cache_entries` | 整数 | キャッシュしたエージェントの回答の数。 |
| `shared` | 真偽値 | `state_shared: true` を設定していれば `true`。 |
| `snapshot_path` | 文字列、省略可 | 共有ステートのとき: この worktree のスナップショットファイル。 |
| `seeded_from` | 文字列、省略可 | ステートのシード元。 |
| `seeded_at` | 時刻、省略可 | シードした時刻。 |

## smates orphans { #smates-orphans }

`smates orphans --format json`:

| フィールド | 型 | 意味 |
|---|---|---|
| `commits` | 整数 | 再生したコミット数。 |
| `since` | 文字列、省略可 | `--since` のリビジョン。 |
| `recorded_state` | 真偽値 | 実行が記録されていなければ `false`。そのときの依存は静的解析、`inputs`、`rules` だけによるもので、レポートは悲観的になります。 |
| `warnings` | 文字列の配列 | 警告。 |
| `orphan_policy` | 文字列 | `smates.yml` の `orphans:`。 |
| `commits_with_orphans` | 整数 | どのテストにも紐づかない変更ファイルがあったコミット数。 |
| `commits_with_orphans_after_ignore` | 整数 | そのうち、提案された `ignore` のグロブを加えてもまだ残るコミット数。 |
| `commits_with_orphans_after_inputs` | 整数 | そのうち、種類 `inputs` のファイルも `inputs` や `rules` に宣言したとしてもまだ残るコミット数。 |
| `files` | 配列 | オーファン（オーファンだったコミット数の多い順）: `path`、`commits`、`kind`（下表）、`why`（種類 `inputs`: 何に見えるか）、`glob`（種類 `ignore`: 提案するグロブ）。 |
| `ignore` | 配列 | 提案する `ignore` のグロブ: `glob`、`files`、`commits`。 |
| `inputs` | 配列 | 種類 `inputs` のファイル（`files` と同じ形）。 |
| `history` | 配列 | オーファンか、テストが名前を含むファイルがあったコミット: `commit`、`subject`、`orphans`、`referenced`（テストが名前を含むので説明がつくオーファン）。 |
| `existing_ignore` | 文字列の配列 | 現在の `smates.yml` の `ignore:`。 |
| `applied` | オブジェクト、省略可 | `--apply` のとき: 編集結果（[`smates config`](#smates-config) と同じ形）。 |

`kind` の値:

| `kind` | 意味 |
|---|---|
| `ignore` | 何も読んでいません。`ignore` のグロブを提案します。 |
| `inputs` | テストの実行のしかたを変えうるファイルです。`ignore` ではなく `inputs` や `rules` に宣言します。 |
| `reference` | テストが名前を含んでいるので、`ignore` なしで説明がつきます。 |
| `untested` | どのテストからも届かないソースコード（`untested: skip`）。 |

## smates config { #smates-config }

`smates config add-ignore|add-input|set ... --format json`（と `smates orphans --apply --format json` の `applied`）:

| フィールド | 型 | 意味 |
|---|---|---|
| `file` | 文字列 | 設定ファイル。 |
| `key` | 文字列 | 編集したキー（`ignore`、`inputs`、または `set` のキー）。 |
| `added` | 文字列の配列、省略可 | `add-ignore`、`add-input`: 追加したグロブ。 |
| `present` | 文字列の配列、省略可 | `add-ignore`、`add-input`: すでにあったグロブ。 |
| `value` | 文字列、省略可 | `set`: 新しい値。 |
| `previous` | 文字列、省略可 | `set`: 以前の値。 |
| `changed` | 真偽値 | ファイルが変わるかどうか。 |
| `written` | 真偽値 | 書き込んだかどうか（`--dry-run` では `false`）。 |
| `dry_run` | 真偽値 | `--dry-run` を指定しました。 |
| `diff` | 文字列、省略可 | unified diff 形式の変更。 |

## 関連項目 { #see-also }

- [コマンド](cli.md)
- [終了コード](exit-codes.md)
- [選択の仕組み](../getting-started/how-it-works.md)

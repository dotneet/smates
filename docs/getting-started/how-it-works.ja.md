# 選択の仕組み { #how-smates-decides }

smates がテストをスキップするのは、前回成功してから、そのテストが依存するものが何も
変わっていないときだけです。このページでは「依存する」とは何か、変更をどう検出するか、
そして各選択理由が何を意味するかを説明します。

## 例 { #an-example }

テストユニット `src/cart.test.ts` を考えます。前回成功した実行で、smates は次のものを
記録しました。

```text
src/cart.test.ts        テストファイル自身
src/pricing.ts          import され、テストが実行したコード
src/money.ts            src/pricing.ts が import するコード
fixtures/prices.json    テストが実行時に読んだファイル
$TZ                     テストが読んだ環境変数
package.json, vitest.config.ts, pnpm-lock.yaml   スイートの入力
```

ここでファイルを変更すると、次のようになります。

| 変更したもの | `src/cart.test.ts` の扱い |
|---|---|
| `src/money.ts` | 選択: `[changed: src/money.ts]` |
| `fixtures/prices.json` | 選択: `[changed: fixtures/prices.json]` |
| `TZ=UTC` を `TZ=Asia/Tokyo` に | 選択: `[env changed: TZ]` |
| `src/admin/users.ts`（入力に含まれない） | スキップ |
| 何も変更していないが、前回失敗した | 選択: `[failed: last run: fail]` |

*テストユニット* は、smates がまとめて選択・スキップする単位です。テストファイル、
`unittest` ならテストモジュール、Go ならパッケージ（[`granularity: func`](../guides/go-test-functions.md)
ならテスト関数）です。

## テストが依存するもの { #what-a-test-depends-on }

smates はテストユニット `T` ごとに依存（入力）の集合 `D(T)` を持ちます。これは次の 4 つの
和集合です。

| 部分 | 含まれるもの | 出どころ |
|---|---|---|
| traced | `T` の実行中に（子プロセスも含めて）実行・読み込みされたファイル、列挙したディレクトリ、存在を確認したファイル、コードが読んだ環境変数 | 前回の記録された実行 |
| static | `T` の import グラフの閉包 | 毎回計算し直すので、新しい import もすぐに反映 |
| inputs | lockfile、マニフェスト、ランナーの設定、`inputs:` や `rules:` の glob、`smates.yml` 内のそのスイート自身の設定 | スイートの設定 |
| depends_on | テストが別プロセスで叩くサーバーのコード | `smates.yml` で宣言（[サーバーを叩くテスト](../guides/servers.md)） |

トレースは静的解析では分からないもの（計算したパスで読み込む fixture、起動時に読む設定
ファイル、サブプロセスとして起動するスクリプト）を見つけます。静的なグラフは、まだ記録
されていないものを補います。どちらにも見えないものは宣言で補います。

## スキップの規則 { #the-skip-rule }

`T` が **スキップ** されるのは、次の両方を満たすときだけです。

1. 前回の実行で `T` が成功している。
2. `D(T)` のすべての要素のハッシュが、そのときと今とで同じ。

それ以外は `T` が **選択** されます。記録されていない依存（新しく追加した import、新しい
`conftest.py` など）は変更扱いです。

`smates.yml` の設定のうち、テストの結果を変えようがないもの（`ignore`、`agent`、
`orphans`、他のスイート）を編集しても、何も選択されません。

## テストが選択された理由 { #why-a-test-was-selected }

`smates run --explain` と `smates select` は、選択されたテストの横に理由を表示します。

```text
  web (vitest): 4/380
    + src/cart.test.ts  [changed: src/pricing.ts]
    + src/dates.test.ts  [env changed: TZ]
    + src/report.test.ts  [failed: last run: fail]
    + src/new.test.ts  [new: no recorded run]
```

| 理由 | 意味 |
|---|---|
| `changed: <files>` | 前回の実行以降に入力が変わった。 |
| `env changed: <vars>` | テストが読んだ環境変数の値が変わった。同じ角かっこ内で `changed:` の後に続くこともあります。 |
| `failed: last run: <status>` | 前回成功しなかった（`fail`、または結果を判定できなかった場合の `unknown`）。 |
| `new: no recorded run` | ステートにそのテストの記録がない。 |
| `diff: <files>` | 記録がなく、テストが静的に依存するファイルがベースブランチから変わっている（下記参照）。 |
| `reference: references <file>` | オーファン（どのテストにも紐づかない変更）のファイル名にテストが言及している。 |
| `orphan: unattributed change: <file>` | オーファンのポリシーにより、スイート全体を実行した。 |
| `agent: …` | オーファンについて LLM エージェントがテストを追加した。 |
| `caller: added by caller` | `smates run --add` で追加された。 |
| `only: --only` | `smates run --only` で指定された。 |
| `all` | 全件実行: `--all`、または CI でのデフォルトブランチ。 |

??? note "詳細: あまり見ない理由"
    | 理由 | 意味 |
    |---|---|
    | `unproven: skipped last run: not proven to pass` | Go のみ: 前回テスト関数が自分自身をスキップした（`granularity: func`）か、ビルドの構成によってパッケージのテストが除外されたので、成功が証明されていない。それ以外のスキップ（pytest、vitest、jest、すべてのテストがスキップした Go のパッケージ）は成功として記録されます。 |
    | `volatile: listed in volatile: runs every time` | スイートの [`volatile`](../reference/configuration.md#suite-keys) に含まれている。 |
    | `expired: last run <age> ago, older than max_age <max_age>` | 最後に記録した実行が、スイートの [`max_age`](../reference/configuration.md#suite-keys) より古い。 |
    | `new: no recorded run, and tests read <file> (changed since the base revision)` | 記録がなく、import するファイルもベースブランチから変わっていないが、同じスイートの記録済みのテストが読むファイルが変わっていて、このテストも読む可能性がある（下記参照）。 |
    | `all: runs with <test>, of a project whose test files smates cannot list` | 選択したテストが、テストファイルを推測でしか決められないプロジェクト（smates が読めない Storybook のストーリー）に属するので、スイート全体を実行した。 |
    | `rerun: rerun` | `smates watch` が前回の選択を再実行した。 |
    | `failed: rerun of failed tests` | `smates watch` が成功していないテストを再実行した（`f` キー）。 |

    `smates select --format json` の理由のコードは [JSON 出力](../reference/json-output.md#selection-reasons) にあります。

??? note "詳細: `new` と `diff` の違い"
    CI では、記録のないテストは `new` として選択されます。手元のマシンのステートは
    そのマシンで実行したテストしか知らないため、記録のないテストはベースブランチに対する
    静的解析で判定します。テストファイルやそれが import するファイルがベースブランチから
    変わっていれば選択されます（`diff`）。追加したばかりのテストファイルはそれ自体が
    変更なので、必ず実行されます。ステートがまったくない場合は、すべてのテストがこの
    方法で判定されます。CI でも `--base` を渡せば同じです（1 つのジョブでスイートを
    1 つずつ導入する場合）。また、同じスイートの記録済みのテストが読むデータファイルが
    ベースブランチから変わっている場合も、このテストがそれを読む可能性があるので選択されます
    （`new`）。

## git diff ではなく内容ハッシュ { #content-hashes-not-git-diff }

各入力は、テスト実行時点の内容ハッシュ（git の blob ハッシュ）とともに保存され、現在の
内容と比較されます。git の履歴から何かを計算するわけではないので、次のようになります。

- 履歴が不要です。シャロークローン（`fetch-depth: 1`）でも構いません。
- 未コミットのファイルや追跡されていないファイルも数えるので、作業ツリーが汚れていても
  動きます。
- rebase、マージ、amend したコミットに惑わされません。

ファイルのハッシュには、さらに 2 つの要素が含まれます。**実行ビット**: テストが実行する
スクリプトに `chmod -x` すると、そのテストが選択されます。**シンボリックリンク**: リポジトリ
内のリンク経由で読んだファイルはリンク自体にも依存するので、リンク先を変えると、そのリンク
経由で読むテストが選択されます。

??? note "詳細: 実行ビットが付いた場合"
    検出できるのは実行ビットが *外れた* 場合だけです。古い smates が書いたステートは
    実行ビットなしでファイルを記録しており、smates はそうしたステートを（CI が古い
    バージョンで書き続けている間も）引き続き受け入れるため、内容を変えずに実行ビットだけを
    付けても何も選択されません。
    [制限事項](../reference/limitations.md#files-and-file-systems) を参照してください。

## 安全側に倒す原則 { #safety-principles }

不確かなことはすべて、テストを *多めに* 実行する側に倒します。

- **失敗したテストは必ず再実行します。** 成功するまで続きます。結果が不明なテストも同様です。
- **新しいテストは必ず実行します。** 記録されていない依存は変更として扱います。
- **中が見えないコマンドは依存を広げます。** smates が中を追えないコマンド（`make`、
  コンパイル済みバイナリ）を実行するテストは、それを実行したディレクトリ以下のすべてに
  依存します。
- **エラー時は実行する側に倒します。** エージェントが失敗した、タイムアウトした、API キーが
  ないといった場合は、影響しうるスイートを丸ごと実行します。
- **誰もテストを取り除けません。** LLM エージェントも、`--add` を渡すコーディングエージェント
  も、決定的な選択にテストを追加することしかできません。

## どのテストにも紐づかない変更 { #changes-no-test-explains }

どのテストの `D(T)` にも含まれず、無視設定にも当たらない変更ファイル（Dockerfile、CI
スクリプト、サービス側で適用する SQL ファイルなど）を *オーファン* と呼びます。smates は
次の手順で扱います。

1. そのファイル名に言及しているテストを選択する
2. どのテストからも到達しないソースコード（*テストされていないコード*）への変更をスキップする
3. 残りは、API キーがあれば LLM エージェントに尋ねる
4. それもなければ、そのファイルが影響しうるスイートを丸ごと実行する

頻繁に現れるオーファンは `ignore:` に入れるべきもので、`smates orphans` が glob を提案します。詳しくは
[どのテストにも紐づかない変更](../guides/orphans.md) を参照してください。

## テストのトレース方法 { #how-tests-are-traced }

smates はいつものテストコマンドを実行し、その実行の間だけ、環境変数（`NODE_OPTIONS`、
`PYTHONPATH`、`GOFLAGS`）といくつかの引数（[smates がコマンドに追加するもの](../guides/runners.md#what-smates-adds-to-the-command)
を参照）でトレース用のフックを加えます。プロジェクトには何もインストールしません。
`.smates/` を除いてプロジェクト内に書き込むのは、vitest のレポーターの一時的なコピーだけで、
実行後に削除します。

| 言語 | 依存の記録方法 |
|---|---|
| JavaScript / TypeScript（vitest、jest、`node --test`） | ワーカーまたはプロセスごとの V8 カバレッジと、ファイルシステムのフック |
| Bun | ファイルごとの `bun test --coverage`、モジュールキャッシュ、`node:fs`・`Bun.file`・`Bun.Glob` の preload フック |
| Python（pytest、unittest） | `sys.monitoring`（3.12 以降）または `sys.setprofile` と audit フック。テスト・fixture・モジュールごと |
| Go | 静的な import の閉包（Go では正確）と、`go test -exec` 経由でテストログが記録するファイルと環境変数 |
| 子プロセス | Node、Bun、Python の子プロセスは自分自身をトレースします。シェルスクリプト、`grep`、`cat` などはコマンドラインから解析し、Go のテストの子プロセスは Go のテストログから追跡します（[詳細](../guides/runners.md#child-processes)） |

カバレッジツールやパッケージマネージャーを含むランナーごとの詳細は
[言語とランナー](../guides/runners.md) にあります。

## 失敗しそうなテストから実行する { #likeliest-failures-first }

`smates run` は選択したテストを次の順に開始します。

1. 前回成功しなかったテスト
2. テストファイル自身、またはそれが直接 import するファイルが変わったテスト
3. 間接的な依存（推移的な import、トレースしたデータ、設定、ファイル名での言及、
   エージェント）経由で選ばれたテスト
4. 新しいテストとその他

同じグループ内では、記録された実行時間が短いものから先に実行します。スイートはそれぞれの
最も優先度の高いテストの順に実行し、各ランナーにはこの順でファイルを渡します。pytest と
`go test` はその順に開始しますが、vitest と jest は独自のシーケンサーを使います（こちらも
前回失敗したファイルを先に実行します）。

`--fail-fast` を付けると、各スイートをグループごとのランナー呼び出しに分け、スイートを
またいでグループ順に実行します。ある呼び出しが失敗すると、それ以降の呼び出しは開始しません
（実行中のものは最後まで走ります）。開始しなかったテストは記録されないので、次の実行でも
選択されたままです。これらはログ、サマリー、`--report`（`not_run`）に列挙されます。ランナーの起動回数が増えるため、
既定では分割しません。`--fail-fast` は `--shadow` と併用できません。

## 次のステップ { #next-steps }

- [スイートの設定](../guides/configuration.md): smates が検出した設定を調整し、トレースでは
  見えない依存を宣言します。
- [どのテストにも紐づかない変更](../guides/orphans.md): オーファンの処理手順の詳細。
- [環境変数](../guides/environment.md): 追跡される変数と無視される変数。
- [シャドーモード・flaky・統計](../guides/trust.md): 選択をどこまで信頼できるかを測ります。
- [設計](../DESIGN.md): 内部構造、依存キー、健全性の議論。

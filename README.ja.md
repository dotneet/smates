# smates

**変更が影響しうるテストだけを実行する** — TypeScript/JavaScript (vitest, jest,
`node --test`, bun)、Go、Python (pytest, uv) 対応。CI でもローカルでも使えます。

smates は実際のテスト実行をトレースして、各テストが *本当に使ったもの*
(実行されたコード、読まれたファイル、列挙されたディレクトリ、起動された子プロセス)を学習し、
静的な import グラフを安全網として重ねます。テストは **前回成功時から依存するものが
何ひとつ変わっていない場合にだけ** スキップされます。どのテストも依存していない変更
(Dockerfile、CI スクリプト、サービス側で適用される SQL など)は、テストを *追加* することしか
できない小さな LLM エージェント(Claude Haiku)に渡されます。エージェントがなければ、
影響しうるスイートを丸ごと実行します。

## 特長

- **単一の静的バイナリ**(約 5 MB、ランタイム依存なし)。インストールは 1 秒程度。
- **コード変更もプラグイン導入も不要。** トレース用フックは smates がテストコマンドを
  起動するときだけ環境変数(`NODE_OPTIONS`、`PYTHONPATH`、`GOFLAGS`)経由で注入されます。
- **静的解析では分からない依存を捕捉**: 計算されたパスでの `import()`/`require()`、
  `importlib`、実行時に読む fixture・設定ファイル、テストが走査するディレクトリ、
  サブプロセスとして起動されるスクリプト、共有 pytest fixture。
- **安全側に倒れる設計**: 内容ハッシュによる指紋、失敗したテストは必ず再実行、新しい
  テストは必ず実行、不確かなものはすべて「多めに実行する」側に倒れます。
- **失敗の原因を指し示す**: 失敗したテストごとに、その入力のうち変更されたファイルを
  提示します。エージェントがあれば原因を 1 行で説明します([失敗ヒント](#失敗ヒント))。
- **どこでも動く**: GitHub Action 同梱。他の CI でも同じ CLI と 1 ファイルの状態だけで動作。

## 判定方法

テスト単位 `T`(テストファイル。Go はパッケージ)ごとに、使った入力と、最後に実行した時点の
git blob ハッシュを保存します。

```
D(T) = traced(T)   T の実行中(子プロセス含む)に実行・読み込みされたファイル
     ∪ static(T)   現時点で計算した import グラフの閉包(新しい import も即座に反映)
     ∪ inputs      lockfile、マニフェスト、ランナー設定、smates.yml、指定した glob
```

前回成功していて、`D(T)` の全ファイルのハッシュが今も同じときだけ `T` を **スキップ**し、
それ以外は理由付きで **選択** します(`smates run --explain`)。比較は `git diff` ではなく
内容ハッシュなので、履歴不要(`fetch-depth: 1` で可)、未コミットの変更にも対応し、
rebase やマージコミットにも惑わされません。

**オーファン(どのテストにも紐付かない変更)** は次の順で扱います。

1. テストのソースや依存ファイルがそのファイル名に言及していれば選択(文字列で参照される
   fixture・マイグレーション・スキーマなど)。
2. 残りは **エージェント** に問い合わせ(`ANTHROPIC_API_KEY` がある場合)。読み取り専用
   ツールでリポジトリを調べ、特定のテストを追加するか全実行を求めます。テストを削除する
   ことはできないため、誤答や指示注入があっても「追加し損ねる」以上の害はありません。
3. それ以外の場合は、そのファイルが影響しうるスイートを丸ごと実行します。

## クイックスタート

```bash
curl -fsSL https://raw.githubusercontent.com/dotneet/smates/main/install.sh | sh

smates init --github # スイート検出、smates.yml(と GitHub ワークフロー)生成、.smates/ を gitignore
smates run --all     # 一度すべて実行して各テストの依存を記録
smates run           # 以後は変更の影響を受けるテストだけ実行
smates why src/pricing.ts     # このファイルに依存するテストは?
smates deps src/cart.test.ts  # このテストは何に依存している?
```

### GitHub Actions

```yaml
      - uses: dotneet/smates@v1
        with:
          anthropic-api-key: ${{ secrets.ANTHROPIC_API_KEY }}   # 任意
```

デフォルトブランチへの push では全テストを実行して状態を更新し、プルリクエストでは
影響のあるテストだけを実行します。状態はブランチごとに Actions cache に保存され、
デフォルトブランチの状態にフォールバックします。詳細は
[docs/ci/github-actions.md](docs/ci/github-actions.md)、他の CI は
[docs/ci/other-ci.md](docs/ci/other-ci.md) を参照してください。

### プルリクエストのサマリ

`comment: true`(ジョブに `permissions: pull-requests: write` が必要)を指定すると、
選択結果のサマリをプルリクエストのコメントとして投稿し、push のたびに同じコメントを
更新します。変更が影響するテスト数とスイート、変更ファイルごとに選ばれたテストとその
依存の種類(静的 import、前回実行時に追跡した読み込み、スイート入力など)、どのテストも
依存していない変更の扱い(参照スキャン・エージェント・`orphans` ポリシー)、スキップした
テストの記録済み実行時間を表示します。フォークからのプルリクエストや権限不足の場合は
notice を出してスキップし、ジョブは失敗させません。シャード実行では各シャードが担当分の
コメントを個別に持ちます。他の CI では `smates run --markdown <file>` や
`smates select --format markdown` で同じ Markdown を出力できます。

## シャドーモード

`smates run --shadow` は全テストを実行しつつ選択結果を評価し、「選択していれば
見逃していた失敗テスト」を報告します。新しいリポジトリで選択結果を信頼する前の試用や、
夜間ジョブでの継続的な検証に使えます。

## flaky テストの検出

smates は各テストの全入力の内容ハッシュを知っているので、**入力が何ひとつ変わって
いないのにテスト結果が pass と fail の間で反転した** 場合、そのテストは flaky である
(または smates から見えないもの — 時刻、ネットワーク、環境変数、リポジトリ外のファイル —
に依存している)と、ヒューリスティックではなく証拠に基づいて判定できます。たとえば
失敗したテストが再実行で成功した場合や、デフォルトブランチの全件実行で、何も触れて
いないテストが失敗した場合です。

`smates run` はこれを実行ログ、`--report` の `flaky`、ジョブサマリ、GitHub の出力
(`flaky` / `flaky-tests`)で報告し、証拠(反転回数、最後のコミット、直近の結果)を
状態ファイルに記録します。`smates flaky`(`--format json`、`--clear [テスト...]`)で
一覧できます。flaky な失敗も通常どおり実行を失敗させ、次回も必ず選択されます。

`smates.yml` に `flaky: {retry: 1}` を書くと、*既に flaky と分かっている* テストが失敗
したときに 1 回だけ再実行し、成功すれば実行全体を失敗にしません(そのスイート実行の
失敗がすべてそうしたテストである場合に限ります。反転は報告されます)。既定値は 0 で、
失敗を隠すことはありません。

## 失敗ヒント

テストが失敗したとき、smates はそのテストが使ったファイルと、そのうち変更されたものを
既に知っています。そこで `smates run` は失敗したテストごとに **入力のうち変更された
ファイル** を一覧にします。たいていは数個、多くの場合 1 つです。

```
smates: failure hints for 1 failing test unit(s):
  web: src/cart.test.ts — 2 of its 37 known inputs changed since its last recorded run (3f2a1c9, passed)
      src/price.ts  (source, direct import, modified)
      fixtures/prices.json  (data, traced, modified)
    → agent (high confidence): src/price.ts:12 — roundCents now truncates instead of rounding, so the total is off by one cent
```

- 「変更」は、そのテストを最後に実行したときに記録した内容との比較です(記録のない
  テストは、今回の選択で変更と判定されたファイルとの比較)。候補は、テストが直接
  import するコード、その他のコード、テストファイル自身、データ、設定、ディレクトリ
  列挙の順に並べ、同じ種類の中では更新時刻の新しい順にします。
- **入力が何も変わっていない** 場合は、flaky なテストか環境要因(時刻、ネットワーク、
  実行順、トレースで見えないもの)を示唆します。前回も成功していなければ、原因はその
  実行より前にあります。どのテストにも紐付かない変更ファイルがあれば、それを候補として
  示します。
- 実行ログ、ステップサマリー(`--summary` / `$GITHUB_STEP_SUMMARY`)、JSON レポート
  (`failure_hints`。vitest、jest、pytest、`go test`、JUnit ではランナーの失敗出力も含む)
  に出力されます。`--no-record` での実行にはテストごとの結果がないため、ヒントは出ません。
- **エージェントがある場合**(API キーあり、`--no-agent` なし、`agent.explain_failures` が
  false でない)、最大 `agent.explain_max` 件(既定 3)の失敗について、推定原因を 1 行
  (ファイル、行、理由、確信度)で示します。失敗ごとにツールなしの 1 リクエストで、失敗
  出力と上位候補の diff(サイズ上限あり。git-ignore されたファイルは送りません)を送ります。
  後続のスイートの実行中にバックグラウンドで行い、上限 30 秒。結果は内容をキーに状態へ
  キャッシュされ、エラーは注記を表示するだけで実行を失敗させません。

設定・コマンド・制限事項の詳細は [README.md](README.md) と [docs/DESIGN.md](docs/DESIGN.md) を参照してください。

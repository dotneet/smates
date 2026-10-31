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

## シャドーモード

`smates run --shadow` は全テストを実行しつつ選択結果を評価し、「選択していれば
見逃していた失敗テスト」を報告します。新しいリポジトリで選択結果を信頼する前の試用や、
夜間ジョブでの継続的な検証に使えます。

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

## ウォッチモード

`smates watch` はファイルを保存するたびに、その変更が影響しうるテストだけを再実行します。
`smates run` と同じトレース済みの依存関係を使うため、ランナー固有のウォッチモードと違って
対応するすべてのランナー(go test、pytest、vitest、jest、node --test、bun、custom)で、
言語をまたいで動きます。たとえば `testdata/orders.json` を編集すればそれを読む Go パッケージの
テストが、pytest のテストが読む YAML を編集すればそのテストファイルが再実行されます。

```text
$ smates watch
── 10:42:07 ── 1 changed: internal/store/testdata/orders.json
smates: selected 1 of 16 test unit(s) across 2 suite(s) (94% skipped)
    + internal/store  [changed: internal/store/testdata/orders.json]
ok  	example.com/shop/internal/store	0.170s
── ✓ PASS · 1 passed · 600ms
   watching for changes (a: run all · f: rerun failed · enter: rerun · q: quit)
```

- 各サイクルは記録付きの `smates run` そのものなので、状態は常に最新に保たれ、失敗した
  テストは成功するまで再実行されます。起動時に現在の選択結果を一度実行します。`--` より前は
  いつもの `-C`、`--config`、`--state`、`--suite`、`-v`、`--` 以降はランナーに渡されます。
- 変更検出は `git status` のポーリングです(ファイルシステム監視なし・追加依存なし、
  `.gitignore` を尊重し、git のロックは取りません)。アトミック保存・フォーマッタ・checkout
  などの連続した変更はデバウンスして 1 回にまとめ、テスト実行中に保存されたファイルは
  終了後にちょうど 1 回の追加実行を起こします。smates の状態ファイル、`node_modules`、
  Python や Vite のキャッシュ、カバレッジ、JUnit レポート、エディタの一時ファイルでは
  起動しません。毎回の実行で書き換わるファイルは 3 回連続した時点で再実行のきっかけから
  外れます。巨大なリポジトリでは `git config core.fsmonitor true` と
  `core.untrackedCache true` でポーリングが軽くなります。
- キー操作(標準入力が端末のとき): `a` 全テスト実行、`f` 成功していないテストを再実行、
  `Enter` 直前の選択を再実行、`q` 終了。Ctrl-C は実行中のテスト(子プロセスのツリー全体)を
  止めて終了します。
- どのテストにも紐付かない変更はオーファンポリシーに従いますが、対象はウォッチ中に変更された
  ファイルだけで、`--agent` を付けない限りエージェントは呼びません。`orphans: agent` の場合は
  `smates run --no-agent` と同じく、影響しうるスイートを丸ごと実行します。

設定・コマンド・制限事項の詳細は [README.md](README.md) と [docs/DESIGN.md](docs/DESIGN.md) を参照してください。

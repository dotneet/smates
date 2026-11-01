# ウォッチモード { #watch-mode }

`smates watch` は、ファイルを保存するたびに、その変更が影響しうるテストだけを再実行します。言語や
ランナーを問わず、作業中に使えます。

```text
$ smates watch
smates: watching ~/src/shop for changes (go, pytest); orphan policy: agent (...)

── 10:42:07 ── 1 changed: internal/store/testdata/orders.json
smates: selected 1 of 16 test unit(s) across 2 suite(s) (94% skipped)
    + internal/store  [changed: internal/store/testdata/orders.json]
ok  	example.com/shop/internal/store	0.170s
── ✓ PASS · 1 passed · 600ms
   watching for changes (a: run all · f: rerun failed · enter: rerun · q: quit)
```

ランナー固有のウォッチモードが知っているのは、そのランナーの import だけです。`smates watch` は
`smates run` と同じトレース済みの依存を使うので、すべてのランナー（go test、pytest、vitest、jest、
node --test、bun、custom）で、言語をまたいで動きます。`testdata/orders.json` を編集すればそれを読む
Go パッケージが、pytest のテストが読み込む YAML を編集すればそのテストファイルが再実行されます。

## キー操作 { #keys }

標準入力が端末のときに使えます。

| キー | 動作 |
|---|---|
| `a` | すべてのテストを実行 |
| `f` | 成功していないテストを再実行 |
| `Enter` | 直前の選択を再実行 |
| `q` | 終了 |
| Ctrl-C | 実行中のテスト（プロセスツリー全体）を止めて終了 |

## 仕組み { #how-it-works }

- **各サイクルは記録付きの `smates run` です。** ステートは常に最新に保たれ、失敗したテストは成功する
  まで再実行されます。起動時に、現在の選択を一度実行します。
- **変更は `git status` のポーリングで検出します。** ファイルシステムの監視も追加の依存もありません。
  `.gitignore` を尊重し、git 自身のロックは取りません。
- **連続した変更はまとめます。** アトミック保存、フォーマッタ、checkout などは 1 回の実行になります。
  テストの実行中に保存されたファイルは、終了後にちょうど 1 回の追加実行を起こします。
- **ノイズは除外します。** smates のステート、`node_modules`、Python や Vite のキャッシュ、カバレッジ、
  JUnit レポート、エディタの一時ファイルでは実行しません。毎回の実行で書き換わるファイルは、3 回続いた
  時点で再実行のきっかけから外れます。

## オプション { #options }

```bash
smates watch --fail-fast -- -v
```

| フラグ | 動作 |
|---|---|
| `--interval 500ms` | 作業ツリーをポーリングする間隔（既定 `500ms`） |
| `--debounce 300ms` | ファイルの変更がこの時間止まるまで待ってから実行（既定 `300ms`） |
| `--fail-fast` | 各サイクルを `smates run --fail-fast` で実行。失敗しそうなテストから実行し、1 つ失敗したらそれ以上起動しません |
| `--agent` | オーファンについて [LLM エージェント](agent.md) に問い合わせます（下記参照） |
| `--clear` | 各実行の前に画面をクリア |
| `-C`、`--config`、`--state`、`--suite`、`-v` | `smates run` と同じ |
| `-- <引数>` | ランナーに渡します |

## ウォッチ中のオーファン { #orphans-while-watching }

オーファンとは、どのテストにも紐づかない変更のことです（[どのテストにも紐づかない変更](orphans.md)
を参照）。ウォッチモードがオーファンポリシーを適用するのは、ウォッチ中に変更されたファイルだけです。

`--agent` を付けない限りエージェントは呼ばないので、保存を繰り返しても LLM の呼び出しが連発することは
ありません。その場合 `orphans: agent` では、`smates run --no-agent` と同じく、オーファンが影響しうる
スイートを丸ごと実行します。

!!! tip "大きなリポジトリでは"
    `git config core.fsmonitor true` と `git config core.untrackedCache true` で、1 回のポーリングが
    軽くなります。

## 関連項目 { #see-also }

- [クイックスタート](../getting-started/quickstart.md)
- [コマンド](../reference/cli.md)
- [設計: ウォッチモード](../DESIGN.md#6-watch-mode)

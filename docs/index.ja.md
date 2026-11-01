# smates { #smates }

smates は、変更が影響しうるテストだけを実行するツールです。TypeScript/JavaScript、Go、
Python に対応し、CI でも手元のマシンでも使えます。

```text
$ smates run
smates: selected 7 of 412 test unit(s) across 3 suite(s) (98% skipped)
smates: web: running 5 test unit(s) with vitest
...
smates: done in 6.2s: ran 7 of 412 test unit(s), exit code 0
```

## なぜ必要か { #why }

ほとんどの変更はリポジトリのごく一部にしか触れませんが、CI はプッシュのたびにすべての
テストを実行します。落ちるはずのないテストを何分も待ち、無関係な flaky なテストが落ちれば、
もう一度フル実行をやり直すことになります。

smates は各テストが実際に使ったものを記録し、次の実行では、前回成功してから入力が
変わっていないテストをスキップします。実行するテストが減るので実行時間が短くなり、
無関係なテストが flaky に落ちる機会も減ります。

## 仕組み { #how-it-works }

<div class="grid" markdown>

<div class="card" markdown>

**1. 各テストが使うものを記録する**

いつものテストコマンドを実行している間に、各テストが実行したコード、読んだファイル、
列挙したディレクトリ、起動したプロセスをトレースします。安全網として静的な import
グラフも加えます。

</div>

<div class="card" markdown>

**2. すべての入力をハッシュする**

各入力を内容ハッシュとともに保存します。内容で比較するので git の履歴は不要で、
未コミットの変更、rebase、CI の新しいチェックアウトでも正しく動きます。

</div>

<div class="card" markdown>

**3. 変わったものだけを実行する**

テストがスキップされるのは、前回成功していて、その入力がすべて今も同じハッシュのとき
だけです。それ以外は理由付きで実行します（`smates run --explain`）。

</div>

</div>

どのテストも依存していない変更（Dockerfile や CI スクリプトなど）も無視はしません。
そのファイル名に言及しているテストを探し、テストを *追加* することしかできない小さな LLM
エージェントに尋ねることもでき、それもなければ影響しうるスイートを丸ごと実行します。
詳しくは [選択の仕組み](getting-started/how-it-works.md) を参照してください。

## 特長 { #key-properties }

- **単一の静的バイナリ**（約 5 MB、ランタイム依存なし）。
- **コード変更もプラグインも不要。** トレース用フックは、smates がテストコマンドを実行する
  間だけ環境変数（`NODE_OPTIONS`、`PYTHONPATH`、`GOFLAGS`）経由で注入されます。
- **静的解析では見えない依存も捉える**: 動的な `import()`/`require()`、`importlib`、
  実行時に読む fixture や設定ファイル、テストが走査するディレクトリ、サブプロセスとして
  起動するスクリプト、共有された pytest の fixture。
- **安全側に倒れる設計**: 失敗したテストは必ず再実行し、新しいテストは必ず実行し、
  不確かなことはすべて「多めに実行する」側に倒します。
- **失敗の原因を指し示す**: 失敗したテストごとに、その入力のうち変更されたファイルを
  示します（[失敗ヒント](guides/failure-hints.md)）。
- **どの CI でも動く**: GitHub Action を同梱。それ以外の CI でも同じ CLI と 1 つの
  ステートファイルで動きます。

## 対応ランナー { #supported-runners }

| ランナー | テストユニット |
|---|---|
| `vitest`、`jest`、`node --test`、`bun` | テストファイル |
| `pytest` | テストファイル |
| `unittest` | テストモジュール |
| `go` | パッケージ（`granularity: func` ならテスト関数） |
| `custom`（任意のコマンド） | テストファイル |

*テストユニット* は、smates がまとめて選択・スキップする単位です。ランナーごとのトレースの
詳細は [言語とランナー](guides/runners.md) にあります。

## 試してみる { #try-it }

```bash
curl -fsSL https://raw.githubusercontent.com/dotneet/smates/main/install.sh | sh
smates run --all     # リポジトリで: 全テストを一度実行して記録する
# ... コードを編集 ...
smates run           # 変更が影響しうるテストだけを実行する
```

始めるのに設定は要りません。smates がスイートを自動で検出します。手順は
[クイックスタート](getting-started/quickstart.md) で順を追って説明しています。

## 次に読むもの { #where-to-go-next }

<div class="grid cards" markdown>

- **[クイックスタート](getting-started/quickstart.md)**

    ---

    smates をインストールして一度記録し、5 分で選択的な実行を体験します。

- **[選択の仕組み](getting-started/how-it-works.md)**

    ---

    スキップの規則、選択理由、そして迷ったら多めに実行する理由。

- **[GitHub Actions](ci/github-actions.md)**

    ---

    ワークフローに 1 ステップ追加するだけ。ステートはブランチごとにキャッシュされます。

- **[コーディングエージェント](guides/coding-agents.md)**

    ---

    Claude Code や Codex などが、変更のたびに影響するテストだけを実行できるようにします。

</div>

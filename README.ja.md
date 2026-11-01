# smates

**変更が影響しうるテストだけを実行する** — TypeScript/JavaScript（vitest、jest、
`node --test`、bun）、Go、Python（pytest、unittest、uv）に対応。CI でもローカルでも使えます。

**ドキュメント: <https://dotneet.github.io/smates/ja/>**（[English](README.md)）

smates は実際のテスト実行をトレースして、各テストが *本当に使ったもの*（実行された
コード、読まれたファイル、列挙されたディレクトリ、子プロセス）を記録し、静的な import
グラフを安全網として重ねます。テストは **前回成功したときから、依存するものが何ひとつ
変わっていない場合にだけ** スキップされます。

```text
$ smates run
smates: selected 7 of 412 test unit(s) across 3 suite(s) (98% skipped)
smates: web: running 5 test unit(s) with vitest
...
smates: done in 6.2s: ran 7 of 412 test unit(s), exit code 0
```

- **単一の静的バイナリ**。ランタイム依存、コード変更、プラグイン導入のいずれも不要です。
  トレース用フックは、smates が既存のテストコマンドを実行している間だけ注入されます。
- **静的解析では分からない依存も捕捉**: 計算されたパスでの `import()`/`require()`、
  実行時に読まれる fixture や設定ファイル、テストが走査するディレクトリ、サブプロセス。
- **安全側に倒れる設計**: 内容ハッシュによる指紋。失敗したテストと新しいテストは必ず
  実行し、不確かなものはすべて「多めに実行する」側に倒します。
- **理由を説明する**: 各テストが選ばれた理由と、失敗したテストが使った入力のうち変更
  されたものを示します。
- **どこでも動く**: GitHub Action を同梱。ほかの CI でも同じ CLI と 1 つのステート
  ファイルで動きます。

## インストール

```bash
curl -fsSL https://raw.githubusercontent.com/dotneet/smates/main/install.sh | sh
```

[リリース](https://github.com/dotneet/smates/releases)からのダウンロードや
`go install github.com/dotneet/smates/cmd/smates@latest` も使えます。詳しくは
[インストール](https://dotneet.github.io/smates/ja/getting-started/install/)を参照してください。

## クイックスタート

```bash
smates init          # スイートを検出して smates.yml を書く（--github でワークフローも）
smates run --all     # 一度すべて実行し、各テストが使ったものを記録する
# ... コードを編集 ...
smates run           # 変更が影響しうるテストだけを実行する
smates run --explain # ...各テストが選ばれた理由も表示する
```

続きは[クイックスタート](https://dotneet.github.io/smates/ja/getting-started/quickstart/)と
[選択の仕組み](https://dotneet.github.io/smates/ja/getting-started/how-it-works/)へ。

## 対応ランナー

| 言語 | ランナー | 単位 |
|---|---|---|
| TypeScript / JavaScript | vitest、jest、`node --test`、bun | テストファイル |
| Python | pytest、unittest（uv、virtualenv） | テストファイル / モジュール |
| Go | `go test` | パッケージ、またはテスト関数 |
| その他 | `custom` コマンド | テストファイル |

## ドキュメント

| | |
|---|---|
| [はじめに](https://dotneet.github.io/smates/ja/getting-started/quickstart/) | インストール、最初の実行、選択の仕組み |
| [ガイド](https://dotneet.github.io/smates/ja/guides/configuration/) | 設定、ランナー、オーファンとエージェント、git フック、ウォッチモード、worktree |
| [CI](https://dotneet.github.io/smates/ja/ci/github-actions/) | GitHub Actions、GitLab、CircleCI、Buildkite、Jenkins |
| [コーディングエージェント](https://dotneet.github.io/smates/ja/guides/coding-agents/) | Claude Code や Codex などから使う（[docs/AGENTS.md](docs/AGENTS.md)、`smates guide`） |
| [リファレンス](https://dotneet.github.io/smates/ja/reference/cli/) | コマンド、`smates.yml`、終了コード、制限事項 |
| [設計](docs/DESIGN.md) | 内部の仕組み（英語） |

サイトは [`docs/`](docs/) から MkDocs Material でビルドしています
（`uvx --with-requirements docs/requirements.txt mkdocs serve`）。

## ライセンス

[MIT](LICENSE)

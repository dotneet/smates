# クイックスタート { #quick-start }

インストールから、変更が影響しうるテストだけを実行するところまで 5 分です。`PATH` に smates
があり（[インストール](install.md)）、テストがすでに動く git リポジトリを用意してください。

## 1. 設定ファイルを書く { #1-write-the-configuration }

```text
$ smates init
wrote smates.yml with 2 suite(s):
  - web (vitest) in packages/web
  - api (go) in services/api

Next steps:
  smates run --all     # run everything once and record what each test uses
  smates run           # afterwards: run only what your changes can affect
  smates why <file>    # see which tests depend on a file
  smates hook install  # optional: run the affected tests before every git push
```

`smates init` はテストスイートを検出して `smates.yml` を書き出します。記録したステートを置く
`.smates/` も `.gitignore` に追加します。*スイート* とは、1 つのディレクトリに対する 1 回の
テストランナーの起動のことです。

`smates init --github` を使うと GitHub Actions のワークフローも書き出します
（[設定される内容](#what-init-github-sets-up)）。

!!! tip "試すだけなら設定は不要"
    `smates.yml` がなくても、smates はコマンドのたびにスイートを自動検出します。
    `smates list` で検出結果を確認できます。`smates init` はその結果を編集可能な
    ファイルとして固定します。

## 2. 一度記録する { #2-record-once }

```bash
smates run --all
```

すべてのテストをいつものテストコマンドで一度実行し、各テストが使ったもの（実行したコード、
読んだファイル、参照した環境変数）を記録します。出力はランナー自身のもので、その前後に
smates の行が入ります。

```text
smates: selected 412 of 412 test unit(s) across 2 suite(s) (0% skipped)
smates: web: running 380 test unit(s) with vitest
...
smates: done in 1m 52s: ran 412 of 412 test unit(s), exit code 0
```

記録は `.smates/state.json.gz`（*ステート*）に保存されます。CI では、デフォルトブランチへの
プッシュのたびにステートを更新します（[GitHub Actions](../ci/github-actions.md)）。

## 3. 何か変更してもう一度実行する { #3-change-something-and-run-again }

ソースファイルを編集してから実行します。

```text
$ smates run --explain
smates: selected 3 of 412 test unit(s) across 2 suite(s) (99% skipped)
  web (vitest): 3/380
    + src/cart.test.ts  [changed: src/pricing.ts]
    + src/checkout.test.ts  [changed: src/pricing.ts]
    + src/invoice.test.ts  [changed: src/pricing.ts]
  api (go): 0/32
smates: web: running 3 test unit(s) with vitest
...
smates: done in 2.1s: ran 3 of 412 test unit(s), exit code 0
```

`src/pricing.ts` を使うテストだけが実行されました。`--explain` は選択されたテストごとに
1 行、角かっこ内に理由を付けて表示します。単に `smates run` とすると
`selected ... of ...` の行だけが表示されます。理由の一覧は
[選択の仕組み](how-it-works.md#why-a-test-was-selected) にあります。

何も変更せずにもう一度 `smates run` を実行すると、0 件が選択されてすぐに終わります。

## 4. 依存関係を調べる { #4-inspect-the-dependency-map }

あるファイルに依存しているテスト:

```text
$ smates why src/pricing.ts
web	src/cart.test.ts	(static/inputs, recorded)
web	src/checkout.test.ts	(static/inputs, recorded)
web	src/invoice.test.ts	(recorded)
```

あるテストが依存しているもの:

```text
$ smates deps src/cart.test.ts
$TZ	recorded
fixtures/prices.json	recorded
package.json	static,recorded
smates.yml	static,recorded
src/cart.test.ts	static,recorded
src/pricing.ts	static,recorded
```

`static` はテストを実行しなくても分かる依存（import グラフや、`package.json` などスイートの
入力）、`recorded` は前回の実行で観測された依存です。`$TZ` はテストが読んだ環境変数です。
どちらのコマンドも `--format json` に対応しています。

## 5. セットアップを確認する { #5-check-the-setup }

| コマンド | 表示する内容 |
|---|---|
| `smates list` | スイートごとのランナー、ルート、テストユニット数、コマンド、テストファイルのパターンの出どころ。 |
| `smates doctor` | セットアップの確認: スイート、ツールチェーン、`node_modules` の有無、起動していないサービス、記録済みのステート、エージェントの認証情報。確認に失敗すると終了コード 1 になります。 |

```text
$ smates list
web	runner=vitest	root=packages/web	units=380	command=pnpm exec vitest run	tests=vitest.config.ts test.include
api	runner=go	root=services/api	units=32	command=go test	tests=runner defaults
```

## `init --github` で設定される内容 { #what-init-github-sets-up }

`smates init --github` は `dotneet/smates@v1` アクションを使う
`.github/workflows/smates.yml` を書き出し、リポジトリの内容を読んで中身を埋めます。

- **ツールチェーン**: `mise.toml` / `.tool-versions`（`jdx/mise-action` 経由）、
  `.nvmrc` / `.node-version` または `package.json` の `engines.node` / `volta`、
  `.python-version` から設定します。
- **サービスと環境変数**: `docker-compose.yml` / `compose.yaml` やコード中の `localhost`
  の URL から分かるデータベース、`.env.example` にありコードが読む変数を、確認して有効化
  するためのコメントアウトされた `services:` / `env:` ブロックとして書きます。
- **既存のワークフロー**: すでにテストを実行しているワークフローがあれば、2 つ目は
  作りません。代わりに、そのステップを smates アクションに置き換えるパッチを表示します
  （`--patch FILE` で `git apply` 用に書き出し、`--force` で新しいワークフローを書きます）。
  対象を絞った実行やマトリクスのレグのように正確に置き換えられないステップは、報告して
  そのままにします。pull request でしか実行されないワークフローには、ステートを記録する
  トリガも追加します（[既存のワークフローを切り替える](../ci/github-actions.md#switch-existing-workflows) を参照）。
- **既存の `smates.yml`**: 手を付けず、そのスイートからワークフロー（またはパッチ）だけを
  生成します。`--force` を付けると書き直します。

Makefile の `test` ターゲットのフラグや変数は、`smates.yml` にコメントの `command:` /
`env:` ヒントとして書かれます。テストを実行するワークフローのステップの `env:` も同様です。
どちらかにある `go test` のフラグ `-p`、`-tags`、`-race`、`-timeout` は、スイートの
`command:` そのものに入ります。`smates init --format json` は、確認すべき項目も含めて
これらをスクリプトやコーディングエージェント向けに出力します。

## 次のステップ { #next-steps }

- [選択の仕組み](how-it-works.md): テストが実行されるかスキップされるかを決めるもの、そして
  迷ったら多めに実行する理由。
- [GitHub Actions](../ci/github-actions.md): プルリクエストでは選択的に、デフォルトブランチ
  では全件を実行します。
- [git フックと Claude Code](../guides/git-hooks.md): プッシュのたびに影響するテストを実行します。
- [コーディングエージェント](../guides/coding-agents.md): 変更のたびにエージェントが影響する
  テストだけを実行できるようにします。
- [スイートの設定](../guides/configuration.md): `smates init` が書いた設定を調整します。

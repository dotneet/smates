# コーディングエージェントから使う { #use-smates-from-a-coding-agent }

コーディングエージェント（Claude Code、Codex、Cursor など）は、1 つのタスクの中で何度も自分の変更を
テストします。このページでは、影響するテストだけを実行し、短い出力を読み、どのテストにも紐づかない
変更を自分で判断する方法を説明します。

## エージェントに smates を教える { #point-the-agent-at-smates }

```bash
smates init --agents-md   # AGENTS.md（または CLAUDE.md）に smates の短い節を 1 回だけ追記
smates guide              # コーディングエージェント向けの 1 ページのガイドを表示
```

`smates guide` は [docs/AGENTS.md](../AGENTS.md) を表示します。smates を使うリポジトリで作業する
エージェント向けに書かれたページです。リポジトリに `AGENTS.md` か `CLAUDE.md` があると、`smates init`
はこのガイドを指す短い節の追加を提案し、`--agents-md` でその節を追記します。

## 基本のループ { #the-loop }

エージェントは変更のたびに次を実行します。

```bash
smates run --output failures
```

失敗があると、出力の最後に短いサマリーが出ます。

```text
smates: FAILED 1 of 7 test unit(s) in 3.2s:
  web: src/cart.test.ts
    changed inputs: src/price.ts (modified), fixtures/prices.json (modified)
smates: rerun them: smates run --output failures --only web:src/cart.test.ts
```

1. コードを直します。
2. 表示された `--only` のコマンドを、成功するまで再実行します。
3. 最後に、普通の `smates run --output failures` で締めくくります。失敗したテストに加え、新たに影響を
   受けるテストも実行します。

エージェントが自分でテストスイート全体を実行する必要はありません。壊れうるものは smates が選んでいます。

### 出力を小さく: `--output failures` { #small-output-output-failures }

エージェントはコマンドが出力したものをすべて読みます。`--output failures` は出力を小さく保ちます。

- ランナーのバッチごとの出力はいったん保持し、そのバッチが失敗したときだけ表示します。vitest、jest、
  pytest、`go test`、JUnit レポートでは、失敗したテスト自身の出力だけを表示します。それ以外の
  ランナーでは失敗したバッチの出力を最大 200 行（先頭 30 行と末尾 170 行）表示します。
- smates 自身の進捗行は省きます。選択結果の行は残ります。
- 成功したら 1 行で終わり、失敗したら上のサマリーで終わります。サマリーには失敗したテスト、それぞれの
  変更された入力（[失敗ヒント](failure-hints.md) から）、再実行のコマンドが含まれます。

毎回フラグを付ける代わりに、エージェントの環境に `SMATES_OUTPUT=agent` を一度設定しておけます。両方が
ある場合はフラグが優先です。

### 特定のテストだけを実行: `--only` { #run-exactly-some-tests-only }

`--only suite:unit`（複数指定可）は、指定したテストユニット（テストファイル、Go ならパッケージの
ディレクトリ）だけを実行し、通常どおり記録します。ほかのテストや変更は次の実行に持ち越されます。
ユニット名は `smates list` で確認できます。

### プログラム向けの JSON: `--report -` { #json-for-a-program-report- }

`smates run --report -` は JSON レポート（選択結果、テストごとの結果、`exit_code`）を標準出力に書き、
それ以外の出力はすべて標準エラーに回します。

## 長い実行 { #long-runs }

エージェントのツールにはタイムアウトがあることが多く、出力のない実行は固まっているように見えます。

- **ハートビート。** failures モードでは、出力のない状態が 30 秒続くたびに
  `smates: still running web (3 of 5 test unit(s) started, 1m 30s)` のような行を出します。
  `SMATES_HEARTBEAT=10s` で間隔を変え、`0` で止められます。
- **予算。** 選択したテストの記録済み所要時間の合計が `--budget`（既定 `90s`、`0` で無効）を超えると、
  `smates run` は最初に実行の分け方を表示します。スイートを 1 つずつ実行する（`--suite <name>`）か、
  `--report smates-report.json` を付けてバックグラウンドで実行し、あとでレポートを読む方法です。
- **時間の上限。** `--max-time 5m` を付けると、記録済みの所要時間から見て 5 分より後に終わるバッチは
  開始しません。外れたテストは選択されたまま残り、後の実行で通常の順序で実行されます（CI ではすべて実行します）。
  [時間の上限](../reference/cli.md#time-limit) を参照してください。

## エージェントの環境で実行できないテスト { #tests-the-agents-environment-cannot-run }

エージェントは、一部のテストが必要とするデータベース、ブラウザ、サービスのない worktree や
サンドボックスで作業することがよくあります。そこではそれらのテストが、変更とは関係のない理由で
失敗します。必要なものを [`requires`](../reference/configuration.md#requires) で宣言してください。

```yaml
suites:
  - name: api
    runner: go
    requires:
      - tcp: 127.0.0.1:5432
        tests: ["internal/store/**"]   # データベースが必要なのはこれらのパッケージだけ
```

そのポートで何も待ち受けていなければ、`smates run` はそれらのテストを開始しません。スイートの
ほかのテストはいつもどおり実行されます。外れたテストは記録されずに選択されたまま残るので、
データベースが動いている実行（または CI）で実行されます。ログには失敗したチェックが表示され、
ほかのテストが失敗していなければ終了コードは 4 になります。

## Stop フック { #stop-hooks }

`smates hook install --claude` と `smates hook install --codex` は、エージェントがターンを終えるたびに
影響するテストを実行し、失敗があれば作業を続けさせる Stop フックを追加します。`requires` やフックの
時間の上限（既定 8 分）で外れたテストではブロックしません。
[git フックと Claude Code](git-hooks.md#claude-code-stop-hook) を参照してください。

## 終了コード { #exit-codes }

終了コードで「コードを直す」のか「セットアップを直す」のかが分かります。

| コード | 意味 | すること |
|---|---|---|
| 0 | 成功（または実行するものがない） | 完了 |
| 1 | テストが失敗した | コード（またはテスト）を直す |
| 2 | 使い方の誤り（どのテストにも一致しない `--add` を含む） | コマンドラインを直す |
| 3 | 設定の誤り | `smates.yml` を直す（`smates config ...`）、または `--suite`/`--only` の名前を直す（`smates list`） |
| 4 | 環境の問題（起動できないランナー、git がない、ステートを保存できない、スイートの `requires` を満たさない など） | `smates doctor --format json` を実行する。失敗したチェックごとに `fix` があります。`requires` の場合、テストは満たされる実行のために選択されたまま残ります |

3 や 4 をテストの失敗として扱わないでください。セットアップを直してから再実行します。テストも失敗して
いた場合、終了コードは 1 です。すべてのケースは [終了コード](../reference/exit-codes.md) を参照して
ください。

## 呼び出し元のエージェントに判断させる { #letting-the-calling-agent-decide }

変更の中には *オーファン*（どのテストにも紐づかない変更。Dockerfile や SQL ファイルなど）があります
（[どのテストにも紐づかない変更](orphans.md) を参照）。既定では smates は自前の
[LLM エージェント](agent.md) に問い合わせるか、スイートを丸ごと実行します。しかしコーディング
エージェントは、自分が何をなぜ変更したかを既に知っているので、代わりに判断できます。

```bash
smates select --format json        # orphan_candidates を読む
smates run --output failures --orphans=skip --add api:tests/test_users.py
```

### 1. 候補を読む { #1-read-the-candidates }

`smates select --format json` は、オーファンごとに `orphan_candidates` の要素を 1 つ出力します。JSON の
ほかのフィールドは変わりません。

```json
{
  "orphan_candidates": [
    {
      "path": "db/seed.sql",
      "status": "modified",
      "referenced_by": [
        { "suite": "api", "test": "tests/test_db.py", "file": "tests/conftest.py", "selected": true }
      ],
      "suites": ["api"],
      "handling": "reference"
    }
  ]
}
```

| フィールド | 意味 |
|---|---|
| `path` | 変更されたファイル |
| `status` | `added`、`modified`、`deleted` のいずれか |
| `referenced_by` | そのファイル名に言及しているテスト。テスト、言及している `file`、そのテストが既に選択されているか（`selected`） |
| `suites` | オーファンポリシーがこのファイルのために丸ごと実行するスイート |
| `handling` | この選択での扱い: `reference`、`untested`、`agent`、`no-impact`、`policy`、`ignored`、`skipped`（未処理なら空） |

### 2. テストを追加する { #2-add-tests }

`--add <suite>:<unit>` でテストユニットを追加します（複数指定可）。ログ上の理由は `caller` です。

- ユニットにはテストファイルも指定できます。`--add api:tests/test_users.py`、Go のパッケージユニットなら
  `--add pkg/foo_test.go` です。
- パスはリポジトリ相対、カレントディレクトリ相対、または（`suite:` の後なら）スイートの root 相対で
  書けます。
- そのユニットを持つスイートが 1 つだけなら、スイート名は省略できます。
- 実行中のどのテストにも一致しない名前はエラーです（終了コード 2）。

### 3. この実行のポリシーを決める { #3-choose-the-policy-for-this-run }

`--orphans=agent|full|skip` は、1 回の実行に限って `smates.yml` の `orphans:` を上書きします。

| 値 | 動作 |
|---|---|
| `agent` | 組み込みのエージェントに問い合わせます |
| `full` | 影響しうるスイートを丸ごと実行します（`orphans: all`） |
| `skip` | 名前による参照で説明できなかったものに、オーファンポリシーを適用しません |

どちらのフラグも `smates select`（結果のプレビュー）と、git フックの run フラグ
（`smates hook install -- --orphans=full`）で使えます。どちらも `--only` とは併用できません。

### 安全性の原則 { #the-safety-principle }

呼び出し側にできるのはテストの **追加** だけです。決定的な選択や名前による参照で選ばれたテストを
外すことはできません。

`--orphans=skip` は、変更を自分で判断した呼び出し側のための、唯一の明示的なオプトアウトです。
オーファンがテストを壊しえないと確信できるときだけ使ってください。ログ、ジョブサマリー、実行記録
（`smates stats`）では、こうしたオーファンは *skipped by caller* として表示されます。処理済みの
オーファンと同じく、同じブランチで再び報告されることはありません。

オーファンのためにスイートが丸ごと実行され、呼び出し側が何も判断しなかった場合（API キーがない場合
など）は、ログに代わりの判断方法が表示されます。

## Stop フック { #stop-hooks }

Claude Code では、Claude がターンを終えるたびに影響するテストを実行し、失敗を Claude に返す Stop
フックを使えます（`smates hook install --claude`）。[git フックと Claude Code](git-hooks.md#claude-code-stop-hook)
を参照してください。

## 関連項目 { #see-also }

- [エージェント向けガイド](../AGENTS.md): `smates guide` が表示するページ
- [コマンド](../reference/cli.md): `smates run` のすべてのフラグ
- [終了コード](../reference/exit-codes.md)

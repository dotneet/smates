# 失敗ヒント { #failure-hints }

テストが失敗したとき、smates はそのテストが使ったファイルと、そのうちどれが変更されたかを既に知って
います。そこで失敗したテストごとに、**入力のうち変更されたファイル** を一覧にします。たいていは数個、
多くの場合 1 つです。

```text
smates: failure hints for 1 failing test unit(s):
  web: src/cart.test.ts — 2 of its 37 known inputs changed since its last recorded run (3f2a1c9, passed)
      src/price.ts  (source, direct import, modified)
      fixtures/prices.json  (data, traced, modified)
      $TZ  (environment variable, read in its last run, changed)
    → agent (high confidence): src/price.ts:12 — roundCents now truncates instead of rounding, so the total is off by one cent
```

まず先頭の候補を見てください。最後の行は、オプションの [エージェントによる説明](#agent-explanations)
です。

## 候補の求め方 { #how-candidates-are-found }

「変更」は、*そのテスト* を最後に実行したときに記録した内容との比較です。記録のないテストは、今回の
選択で変更と判定されたファイルと比べます。

候補は種類ごとに次の順で並べます。

1. テストが直接 import するコード
2. テストが使うその他のコード
3. テストファイル自身と、テストが読んだ環境変数
4. データ
5. 設定（スイートの inputs、マニフェスト、ロックファイル）
6. ディレクトリの列挙

同じ種類の中では、更新時刻の新しいもの、テストに近いもの（先頭から共通するディレクトリが多いもの）、
パスの順に並べます。

候補はテストごとに最大 10 個までです。`--report` では、省いた数を `more` として出します。実行ログ、
ステップサマリー、`--output failures` の短いサマリーには先頭の 5 個を出し、残りの数を添えます。

## 入力が何も変わっていない場合 { #when-none-of-its-inputs-changed }

ヒントはそう表示します。これは flaky なテストか環境要因（時刻、ネットワーク、実行順、トレースで見えない
もの）を示唆します。前回も成功していなかったなら、原因はその実行より前にあります。

どのテストにも紐づかない変更ファイル（オーファン。[どのテストにも紐づかない変更](orphans.md) を参照）が
あれば、代わりにそれを候補として示します。smates が原因を特定できない変更とは、まさにそれだからです。
flaky かどうかの確認は [flaky なテスト](trust.md#flaky-tests) を参照してください。

## ヒントが出る場所 { #where-hints-appear }

- 実行ログ
- ステップサマリー（`--summary`、GitHub Actions では `$GITHUB_STEP_SUMMARY`）
- JSON レポート（`--report`）の `failure_hints`。vitest、jest、pytest、`go test`、JUnit レポートでは
  ランナーの失敗出力も含みます
- `smates run --output failures` の短いサマリーの `changed inputs:`
  （[コーディングエージェントから使う](coding-agents.md) を参照）

`--no-record` での実行でも、トレースなしでファイル単位の結果を報告できるランナー（vitest、jest、pytest、
`go test`、JUnit）ではヒントが出ます。それ以外のランナーは、記録しない実行ではテストごとの結果を返しません。

## エージェントによる説明 { #agent-explanations }

[LLM エージェント](agent.md) が使えると、smates は最初のいくつかの失敗に、推定原因を 1 行（ファイル、行、
理由、確信度）で添えます。

```text
    → agent (high confidence): src/price.ts:12 — roundCents now truncates instead of rounding, so the total is off by one cent
```

API キーが設定されていて、`--no-agent` でなく、`agent.explain_failures` が `false` でなく、`agent.command`
が設定されていないときに行います。

```yaml
agent:
  explain_failures: true   # 既定
  explain_max: 3           # 1 回の実行で説明する失敗テストの数（既定 3）
```

- **失敗ごとにツールなしの 1 リクエスト。** 候補の一覧、失敗出力、上位の候補の diff を、それぞれサイズの
  上限付きで送ります。git-ignore されたファイルは送りません。
- **バックグラウンドで実行。** スイートが終わるとすぐにリクエストを始め、後続のスイートと並行して進めます。
  上限は 1 件 30 秒です。
- **キャッシュあり。** 回答は内容をキーにステートへキャッシュされ、同じ失敗を二度説明することはありません。
- **失敗しても害はありません。** エラーは注記を 1 行出すだけで、終了コードは変わりません。
- **組み込みのバックエンドのみ。** `agent.command` を設定すると、API キーがあっても説明は行いません。

## 関連項目 { #see-also }

- [シャドーモード・flaky・統計](trust.md)
- [LLM エージェント](agent.md)
- [設計: 失敗ヒント](../DESIGN.md#failure-hints)

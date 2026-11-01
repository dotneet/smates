# シャドーモード・flaky・統計 { #shadow-mode-flaky-tests-and-stats }

選択結果に頼る前に、自分のリポジトリでそれが正しく働くという根拠がほしいはずです。このページでは、
そのための 3 つの道具を説明します。シャドーモードは選択結果を全件実行と突き合わせ、flaky の検出は理由
なく結果が反転するテストを見つけ、`smates stats` は smates が削減した時間などを集計します。

## シャドーモード { #shadow-mode }

```text
$ smates run --shadow
smates: selected 7 of 412 test unit(s) across 3 suite(s) (98% skipped)
...
smates: shadow: every failing test was selected (7/412 would have run)
```

`smates run --shadow` は **すべての** テストを実行しつつ、並行して選択結果を評価します。そして、選択して
いればスキップしていたはずの失敗テスト（*見逃し*）を報告します。

```text
smates: SHADOW: 1 failing test(s) would NOT have been selected:
  - web: src/report.test.ts
```

見逃しは、smates から見えない依存があることを意味します。サーバーのコード、ネットワーク上のサービス、
無視している環境変数などです。それを宣言すれば（`depends_on`、`rules`、`inputs`。
[サーバーを叩くテスト](servers.md) を参照）、次からはそのテストも選択されます。

### より軽く: `--shadow --no-record` { #cheaper-shadow-no-record }

`smates run --shadow --no-record` はトレースを省くので大幅に軽く、ステートも変更しません。トレースなしでも
ファイル単位の結果を報告できるランナー（vitest、jest、pytest、`go test`、`junit:` を設定したランナー）では、
この場合も見逃しを検出できます。それ以外のランナーでは、失敗した実行の未選択テストを「見逃しの可能性」
として報告します。

### 導入の進め方 { #recommended-rollout }

1. 新しいリポジトリでは、CI の全件実行を `smates run --shadow` に置き換えます。全テストが実行される
   ので、失うものはありません。
2. ログと `smates stats` で見逃しを確認し、1 件ずつ宣言で直します。
3. 見逃しがゼロのまま続いたら、pull request を普通の `smates run` に切り替えます。
4. 新たな死角を捉えるため、夜間の `--shadow` ジョブはずっと残しておきます。

??? note "詳細: シャドーモードとほかのフラグ"
    シャドーモードでは `go test` のキャッシュ結果を使いません（`-count=1`）。go はモジュール外のファイルの
    変更を確認せずにキャッシュを再利用するためです。`--shadow` は `--fail-fast` と併用できません。

## flaky なテスト { #flaky-tests }

smates は flaky なテストを、ヒューリスティックではなく証拠に基づいて検出します。smates は各テストの全入力の
内容ハッシュを知っています。入力が何も変わっていないのにテストの結果が pass と fail の間で反転したら、
そのテストは flaky であるか、smates から見えないもの（時刻、ネットワーク、無視している環境変数、
リポジトリ外のファイル）に依存しています。

典型的な証拠は次のとおりです。

- 失敗したテストが、再実行で成功した。
- デフォルトブランチの全件実行で、何も触れていないテストが失敗した。

```text
$ smates flaky
SUITE  UNIT                FLIPS  LAST FLIP   LAST SEEN             COMMIT   RECENT (oldest first)       ENV READ
web    src/clock.test.ts   3      fail->pass  2026-10-30T09:12:44Z  3f2a1c9  pass fail pass pass fail pass  TZ
```

- `smates run` は flaky なテストを、ログ、`--report`（`flaky`）、ジョブサマリー、アクションの出力
  （`flaky` / `flaky-tests`）で報告します。
- 証拠（反転回数、最後のコミット、直近の結果）はステートに残ります。`smates flaky` はそれを、各テストが
  読む環境変数（反転の前後で値は同じでした）とともに一覧にします。
- `smates flaky --format json` で同じ内容を JSON で出力します。
- `smates flaky --clear [test...]` は、指定したテスト（指定しなければすべて）の証拠を消去します。
  flaky なテストを直したあとに使います。

**検出が失敗を隠すことはありません。** flaky な失敗も実行を失敗させ、次回も必ず選択されます。

### 既知の flaky なテストを再試行する { #retry-known-flaky-tests }

```yaml
flaky:
  retry: 1   # 既定 0: 失敗を決して隠さない
```

スイート実行の失敗がすべて *既に flaky と分かっている* テストだった場合、同じテスト一式でスイート実行を
`retry` 回（最大 5）までやり直します。それで成功すれば実行は失敗にならず、反転は報告されます。

再試行するのは失敗したテストだけでなく、失敗した実行と同じ一式です。実行は、どのテストにも紐づかない
理由（未処理のエラー、teardown の失敗、クラッシュ）で失敗することもあり、flaky なテストだけを再実行すると
それを隠してしまうからです。

## 統計 { #stats }

`smates stats` は、ステートに記録された実行を集計します。

```text
$ smates stats --since 30d
smates: 214 runs since 2026-10-02 00:00, 2026-10-02 to 2026-11-01
  state: .smates/state.json.gz

Runs
  total:         214, 9 failed (exit code ≠ 0)
...
Selection
  test units:    ran 1840 of 88168 (98% skipped)
  time saved:    about 41h of recorded test time skipped
...
```

集計する内容は次のとおりです。

| セクション | 内容 |
|---|---|
| Runs | モード別（選択実行、全件実行、シャドー）とソース別（CI、ローカル、ウォッチ）の実行数 |
| Selection | 全ユニットのうち実行したテストユニット数（スキップ率）、スキップしたユニットの記録済み所要時間（削減時間）と 30 日あたりの換算 |
| Shadow mode | シャドー実行の回数、見逃しの件数と率 |
| Orphans | オーファン（どのテストにも紐づかない変更）の処理の内訳（参照スキャン、テストされていないコード、エージェント、スイート全体の実行、呼び出し側によるスキップ）、エージェントの呼び出し回数とトークン数、バックエンドが報告する場合はコスト（`claude -p` など） |
| Flaky tests | 検出した反転の件数と、現在 flaky とされているテスト数 |
| Recent runs | 直近の実行を 1 行ずつ |

| フラグ | 動作 |
|---|---|
| `--since 30d` | その時点以降の実行だけ。経過時間（`30d`、`2w`、`12h`）、日付（`2026-01-02`）、RFC 3339 の時刻 |
| `--branch NAME` | そのブランチでの実行だけ |
| `--ci` | CI での実行だけ |
| `--recent N` | 直近 N 件の実行を一覧表示（既定 5） |
| `--format json` | 機械可読な出力 |

### 数字の出どころ { #where-the-numbers-come-from }

- `smates run` は実行ごとに短い記録を追加します。`--no-record` と `--dry-run` では記録しません。
- ステートは、180 日以内の直近 1000 件の記録を保持します。
- `smates state merge` は記録の和集合を取るので、CI で復元したステートの統計には、そのステートが経てきた
  すべての実行が含まれます。
- シャード実行はシャードごとに 1 件記録します。オーファンはシャード 1 だけが数えます。

GitHub Actions のジョブサマリーには、累積の削減時間が 1 行で表示されます。

## 関連項目 { #see-also }

- [GitHub Actions](../ci/github-actions.md): 夜間のシャドージョブ、`flaky` の出力
- [失敗ヒント](failure-hints.md): テストが失敗した理由
- [コマンド](../reference/cli.md#smates-flaky): `flaky` と `stats` のすべてのフラグ
- [制限事項](../reference/limitations.md): smates から見えないもの

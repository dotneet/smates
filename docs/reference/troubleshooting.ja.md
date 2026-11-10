# トラブルシューティング { #troubleshooting }

よくある問題への答えを、状況を確かめるコマンドとともにまとめたページです。まずは `smates doctor` から始めてください。

## まずは smates doctor { #start-here-smates-doctor }

```text
$ smates doctor
smates 1.2.3
  ok    repository /home/me/shop
  ok    config smates.yml
  ok    suite web (vitest, root packages/web): 42 test units; command: vitest run
  FAIL  suite e2e: no node_modules (or Yarn PnP loader) from e2e up to the repository root: ...
  warn  agent unavailable (no credentials: set ANTHROPIC_API_KEY (or ANTHROPIC_AUTH_TOKEN), ...
```

失敗したチェックがあると終了コード 1 になります。`smates doctor --format json` では、各チェックに安定した `id` と `fix`（実行するコマンド、設定の変更、リンク）が付きます。チェックの一覧は [`smates doctor`](cli.md#smates-doctor) にあります。

## 毎回実行されるテストがある { #a-test-runs-every-time }

なぜ選択されたのかを smates に尋ねます。

```bash
smates run --explain          # または: smates select --explain
```

| 理由 | 意味 | 対処 |
|---|---|---|
| `failed: last run: fail` | 前回成功しなかった。 | 直します。再実行で通るなら `smates flaky` を確認します。 |
| `new: no recorded run` | このテストの記録がステートにない。 | ステートが復元されているか確認します（[後述](#everything-runs-on-every-ci-run)）。 |
| `changed: <file>` | 前回成功してからその入力が変わった。 | そのファイルが毎回変わるなら、下の項目を見てください。 |
| `env changed: <VAR>` | 読んでいる変数の値が違う。 | 実行やマシンごとに値が違うなら `env_ignore` に追加します。 |

次に、テストが何に依存しているかを確認します。

```bash
smates deps src/cart.test.ts
```

いつまでも落ち着かないテストのよくある原因:

- **テストが毎回作り直すファイル。** git-ignored のパスにある、テスト（やビルド）が毎回書き直すファイルを読むテストは、そのたびに新しい内容に依存します。git-ignored のパスの存在を確かめてから作成するテストは、もう 1 回だけ実行されて落ち着きます。自分で生成したものを読むテストは、新しくチェックアウトするたびに実行されます。
- **`dir/**` への依存。** smates が中を見られないコマンド（`make`、バイナリ）を使うと、テストはそのディレクトリ配下のすべてのファイルに依存します。本当の入力を `rules` で宣言してツールを直接呼ぶか、余分な実行を受け入れてください。
- **ログの警告。** `N test file(s) passed but left no trace (their process was likely killed before writing it); they will run again next time` は、smates が使ったものを保存する前にテストプロセスが終了したことを示します（テスト内の `process.exit()`、強制終了されたワーカーなど）。`N test unit(s) ran and passed but left no trace`（または `N test file(s) passed but none left a trace`）は、そのバッチのテストプロセスがひとつもトレースされなかったことを示します。スイートの command が、smates のトレース用の環境変数（`NODE_OPTIONS`、`NODE_V8_COVERAGE`、`SMATES_*`）を落とすラッパー（turbo や nx などのタスクランナー、`env -i`、コンテナ）経由でテストを実行しているか、キャッシュした結果を再生している可能性が高いです。スイートの `command` をテストランナー自体にして `root:` をそのパッケージにするか、その環境変数を引き継いでください。`custom` のスイートでは、代わりに、記録はコマンドが起動する Node と Python のプロセス、または `junit:` のレポートからだけ行うと表示します。シェルスクリプトや JVM のビルドはどちらも残しません。こうしたユニットは未記録のまま残ります。CI で `--base` がなければ次回も新しいテストとして実行されますが、ローカル（または `--base` 付き）ではベースリビジョンに対する静的解析で判定され、実行時に読むものは見えません。
- **ブラウザのテスト。** `N test file(s) ran in a browser, which smates cannot trace; they will run again next time` は、ブラウザモードのテストか Storybook のストーリーを示します。ブラウザの中は何もトレースされないので、実行のたびに結果が不明になります。スイートに `trust_browser_passes: true` を設定すると、pass を静的な依存とともに記録します。`smates run --shadow` で確かめてください（[ブラウザモードと Storybook](../guides/runners.md#browser-mode-and-storybook) を参照）。この設定がオンのとき、`N browser test(s) passed but are not trusted` は、それでも記録しないストーリーとその理由（smates が解決できない import、`main.*` の `viteFinal` など）を示します。
- **untraced のテスト。** `N untraced test(s) passed but smates cannot tell all they depend on without coverage` は、`untraced` に挙げたテストのうち pass を記録しないものとその理由（プロセスを起動する import、実行時に名前を決めるモジュール、カバレッジなしではトレースできないランナー）を示します。[時間を測るテスト](../guides/runners.md#tests-that-measure-time) を参照してください。
- **ランナーへの引数。** `--` 以降に引数を付けた実行では、テストを成功として記録しません。常に使うフラグはスイートの `command` に移してください。
- **pytest 向けに書かれた unittest のモジュール。** 状態が不明のままなので毎回実行されます。ログで `runner: pytest` が提案されます。
- **記録されない Gradle のテストクラス。** `N test class file(s) passed but were not recorded (...)` が理由を示します。テスト用 JVM が Java 17 より前か、その Flight Recorder の記録がないかです。`smates doctor` が Java のバージョンを表示します（`toolchain-java`）。
- **テストを実行しなかった Gradle のテストクラス。** `N test class file(s) ran no test` は、テストタスクが実行しなかったクラスを挙げます。タスクが除外する JUnit のタグ、ビルドスクリプトのフィルタ、`Test` タスクを実行しないコマンドが原因です。それを実行するタスクを `command` に指定したスイートを作るか、`exclude:` で外してください。
- **Gradle 8.14 より前**は assumption による中断と無効化したテストを区別できないので、スキップしたテストを含むクラスは毎回実行されます（`smates doctor` の `toolchain-gradle`）。
- **flaky なテスト。** `smates flaky` は、入力が変わらないのに結果が反転したテストを一覧します。[シャドーモード・flaky・統計](../guides/trust.md) を参照してください。

## 小さな変更でほとんどのテストが選択される { #a-small-change-selects-most-tests }

`smates why <変更したファイル>` がスイートのほぼすべてのテストを挙げる場合、`granularity: func` の Go スイートなら、よくある原因を `smates doctor` が指摘します（`state-source-sweep`）。

```text
warn  suite api: 511 of 728 test units depend on (nearly) all 104 source files of internal/api/ as whole files, so a change to any of them selects all 511. ...
```

これらのテストは実行時にソースを読んでいます。すべてのテストが通るヘルパーが、ディレクトリのファイルを glob で集めたり、パースしたり、ハッシュを取ったりしています（ルート名の収集、命名規則の検査、キャッシュキーの計算など）。smates はその読み込みがファイルのどの部分を使ったかを判断できないため、読まれたファイルはそれぞれ、そのヘルパーを実行したすべてのテストの依存になります。実行したコードを宣言単位で数える Go の関数単位のユニットでは、ソースファイル全体が依存になっていれば、それは実行時の読み込みを意味します（または、関数のカバレッジプロファイルから、どの本体が実行されたかを判断できなかったことを意味します）。ファイル単位で追跡するスイートでは、パッケージのほとんどのモジュールを import しても同じ形になるため、このチェックの対象外です。そうしたスイートでも、`smates deps <test>` で同じような読み込みを確認できます。

読み込みをテストのプロセスの外へ移してください。テストに必要な情報は一度だけ計算します（生成したファイルやコミットしたファイルにして `go generate` やスクリプトで更新し、ソースとの一致は 1 つのテストで検査します）。あるいは、ソースの走査をすべてのテストが呼ぶヘルパーではなく、専用の 1 つのテストに置きます。そのうえで記録し直し（`smates run --all`）、`smates deps <test>` で確認します。

## smates でだけテストが失敗する { #a-test-fails-only-under-smates }

`smates run --no-record`（トレースなし）でもう一度実行してください。そこでは成功し、記録する実行では毎回失敗するなら、結果を変えているのはトレースです。よくあるのは時間を測るテストです。処理時間や 2 つの計測の比を assert すると、V8 のカバレッジがコードを不均一に遅くするため失敗します。スイートの `untraced` に挙げると、カバレッジなしで記録します（`smates doctor` が `suite-timing-tests` として提案します）。[時間を測るテスト](../guides/runners.md#tests-that-measure-time) を参照してください。

## 実行されるべきテストがスキップされた { #a-test-that-should-have-run-was-skipped }

1. 依存マップを確認します。`smates why <変更したファイル>` はそのファイルに依存するテストを、`smates deps <test>` はテストの依存を一覧します。
2. その変更が無視されたオーファン（どのテストにも紐づかない変更）だったかを確認します。ログには `ignored N unattributed change(s) (orphans: ignore)` や `skipped by caller`（`--orphans=skip`）と出るか、テストされていないコードの変更（`untested: skip`）として一覧されます。[どのテストにも紐づかない変更](../guides/orphans.md) を参照してください。
3. [制限事項](limitations.md) を確認します。別プロセスのサーバー、列挙による環境の読み取り、ブラウザで動くコードは既知の死角で、それぞれ対処法があります。
4. 測定します。`smates run --shadow` はすべてを実行し、選択していたら見逃していた失敗テストを報告します。`smates stats` は見逃し率の推移を表示します。

どれにも当てはまらなければ、`smates run --explain`、`smates why <file>`、`smates deps <test>` の出力を添えて [issue を作成](https://github.com/dotneet/smates/issues) してください。

## 同じファイルが毎回オーファンになる { #the-same-files-are-orphans-on-every-commit }

`.env.example`、デプロイの設定、どのスイートもカバーしないフロントエンドなどのファイルが、スイート全体を実行させ（あるいはエージェントを呼ばせ）続けることがあります。smates に探させましょう。

```bash
smates run --all                    # 先に: 実際の依存を記録する
smates orphans                      # 直近 100 コミットを再生し、ignore: のグロブを提案
smates orphans --apply --dry-run    # smates.yml への変更を表示
smates orphans --apply              # 書き込む
```

テストの動き方を変えうるファイル（CI のワークフロー、Makefile、Dockerfile、ツールチェーンのバージョン指定）は、代わりに `inputs:` や `rules:` の候補として提案されます。グロブを 1 つ手で追加するなら `smates config add-ignore 'deploy/**'` です。[どのテストにも紐づかない変更](../guides/orphans.md) を参照してください。

## 新しいテストファイルが拾われない { #a-new-test-file-is-not-picked-up }

smates は、どのファイルがテストかをランナーの設定から読みます。読めない場合（関数で組み立てる設定、`mergeConfig(...)`、スプレッド構文など）はデフォルトのパターンにフォールバックし、警告を出します。

```bash
smates list -v        # 各スイートのパターンの出どころ、パターン、すべてのユニット
```

新しいファイルが一覧になければ、`smates.yml` でそのスイートに `tests:`（と `exclude:`）を設定してください。`smates doctor` も同じ警告（`suite-test-discovery`）を出します。[スイートの設定](../guides/configuration.md) を参照してください。

## 終了コード 4: ランナーを起動できない { #exit-code-4-the-runner-cannot-start }

```text
smates: web: the runner exited with code 127 before any test reported a result: ...
```

スイートのコマンドがインストールされていない、`PATH` にない、または実行できません。`smates doctor` はスイートと、インストールのためのコマンドを示します。smates は `npx` や `bunx` にランナーをダウンロードさせないので、先にプロジェクトの依存をインストールしてください（`npm ci`、`pnpm install`、`uv sync` など）。[終了コード](exit-codes.md#4-environment-problem) を参照してください。

## Gradle のすべてのテストクラスが失敗する { #every-gradle-test-class-fails }

テストを 1 つも実行する前に失敗する Gradle のビルド（JDK がない、またはビルドには古すぎる）は、選択したすべてのテストクラスを失敗として報告します。ビルド自身のエラーは smates のサマリーより上の出力にあります。`smates doctor` が JDK を確認します（`toolchain-java`）。

## CI で毎回すべてが実行される { #everything-runs-on-every-ci-run }

ステートが復元されておらず、すべてのテストが新規扱いになっている可能性が高いです。

- **GitHub Actions:** 「Restore smates state」ステップのログでキャッシュがヒットしたか確認します。`state-path` は `smates.yml` の `state` で指定したファイルがあるディレクトリでなければなりません。異なるテストを実行するジョブ（マトリクスの各エントリ、固定数のシャード、ジョブに分けたスイート）には、それぞれ別の `cache-key` が必要です。ただし plan ジョブがそれらのために選択する場合（`--max-shards` で数を決めるシャード、ジョブごとにゲートするスイート）は、plan とそれらのジョブが 1 つのキーを共有し、マージのジョブがステートをまとめます。[GitHub Actions](../ci/github-actions.md) と [シャード数を選択に合わせる](../ci/github-actions.md#size-the-shards-to-the-selection) を参照してください。
- **その他の CI:** `smates run` の前に `.smates/` を復元し、後で保存します。テストが失敗したときも保存してください。[その他の CI](../ci/other-ci.md) を参照してください。
- **確認:** `smates state show` はステートの場所と記録内容を表示します。`smates doctor` は `no recorded state` と警告します。
- **初回の実行:** ステートがまったくないとき、smates はベースブランチと比較します。これには履歴が必要です（`fetch-depth: 0`）。履歴がなければ、1 回目はすべてを実行します。

デフォルトブランチでは、ステートを更新するために意図的にすべてのテストを実行します。`--select` で変えられます。

## 新しい worktree が空から、または別のステートから始まる { #a-new-worktree-starts-cold-or-from-the-wrong-state }

ステートのないリンクされた git worktree は、メイン worktree のステート、なければリモートのステートから 1 回だけシードされます。`smates state show` には `seeded: from ...` と表示されます。スナップショットは、その worktree の履歴にあるコミットで記録されたものだけを残し、そうでなければ変更されたファイルをデフォルトブランチと比べて判断します。

- シードを止める: `state_seed: false` または `SMATES_NO_STATE_SEED=1`
- 各 worktree が学んだことを共有する: `state_shared: true`
- いつでもリモートのステートから始める: `smates state pull`。worktree がシードされるのは 1 回だけで、ステートの隣のマーカー（`.smates/.state.json.gz.seeded`）がそれを覚えています。`smates state clear` の後も同様です。

`state_shared: true` では、保存時に別の worktree の実行がロックを手放すのを最大 1 分待ち、それでも取れなければ終了コード 4 で諦めます（テストの結果は報告されます）。クラッシュしたプロセスが残したロックは、2 分経つと引き継がれます。[並行する worktree](../guides/worktrees.md) を参照してください。

## 記録が遅い { #recording-is-slow }

記録（初回の実行、`--all`、デフォルトブランチでのすべての実行）は、選択的な実行より時間がかかります。よくある原因:

- **Python 3.8〜3.11** は `sys.setprofile` でトレースします。Python 3.12 以降は `sys.monitoring` を使い、はるかに軽くなります。
- **Go の `granularity: func`** はテスト関数ごとに 1 プロセスで記録します。関数どうしが状態を共有しないなら、`record_jobs` を上げて 1 つのパッケージの関数を同時に記録できます。
- **`isolate: true`** は記録中にファイルごとにランナーを起動します。共有ワーカーで動くファイルの依存を正確にする必要がなければ、オフのままにしてください。
- **`record_jobs` が小さい**と、ファイルごとに 1 プロセスで記録するランナー（bun、`isolate`）が遅くなります。
- **Gradle** は、テストクラスごとに Flight Recorder を有効にした JVM で記録します。1 クラスあたり約 1 秒です。`isolate: false` で Gradle 自身のフォークのしかたを保ちます（同じ JVM を使ったクラスどうしは依存を共有します）。

プルリクエストでの選択的な実行には影響しません。選択されたテストだけを起動するからです。

## エージェントが呼ばれない { #the-agent-is-not-called }

理由はログに出ます。たとえば `agent unavailable (...); running affected suites in full` です。`smates doctor` も同じ内容を報告します（`agent` チェック）。

| 原因 | 対処 |
|---|---|
| 環境に API キーがない | `ANTHROPIC_API_KEY`、`OPENAI_API_KEY`、`OPENROUTER_API_KEY` のいずれかを設定します（[smates が読む環境変数](environment-variables.md#agent-credentials)）。 |
| `agent.enabled: never`、または `orphans:` が `agent` でない | `smates.yml` を変えます。 |
| `--no-agent` | git フックは `--agent` 付きでインストールしない限りこれを渡します。`smates watch` には `--agent` が必要です。 |
| シャード実行（`--shard`） | シャードは、同じ変更に対してシャードなしの `smates select` や `smates run` がキャッシュした判断を再利用するだけです。 |
| フォークからのプルリクエスト | フォークにはシークレットが渡らないので、`orphans` のフォールバックが適用されます。 |
| オーファンがない | エージェントが呼ばれるのは、どのテストにも紐づかない変更があるとき（と失敗の説明）だけです。 |

[LLM エージェント](../guides/agent.md) を参照してください。

## 関連項目 { #see-also }

- [終了コード](exit-codes.md)
- [制限事項](limitations.md)
- [コマンド](cli.md)

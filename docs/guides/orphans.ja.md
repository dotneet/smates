# どのテストにも紐づかない変更 { #changes-no-test-explains }

Dockerfile や CI スクリプト、サービスが読み込む SQL ファイルのように、どのテストも依存していない
ファイルが変更されることがあります。smates はこうした変更を *オーファン* と呼びます。このページでは、
smates がオーファンをどう扱うか、そしてオーファンを減らす方法を説明します。

## オーファンとは { #what-an-orphan-is }

```text
$ git status --short
 M src/pricing.ts
 M Dockerfile
 M db/seed.sql
 M README.md
```

- `src/pricing.ts` は `src/cart.test.ts` の記録済みの依存なので、このテストが選択されます。
- `README.md` は組み込みの `ignore:` に含まれるので、テストは不要です。
- `Dockerfile` と `db/seed.sql` はどのテストの依存でもなく、無視の対象でもありません。
  これが **オーファン**（どのテストにも紐づかない変更）です。決定的な解析では、どのテストに影響するか
  判断できません。

オーファンとは、どのテストの依存集合にも含まれず（[選択の仕組み](../getting-started/how-it-works.md)
を参照）、`ignore:` にも一致しない変更ファイルのことです。典型的な例は次のとおりです。

| ファイル | トレースで見えない理由 |
|---|---|
| `Dockerfile`、`docker-compose.yml` | テストはコンテナの *中で* 動きますが、このファイルを読むテストプロセスはありません |
| `scripts/ci-setup.sh` | CI がテストの前に実行するもので、どのテストの中でも動きません |
| `db/seed.sql` | テストがアクセスするサーバーが、別プロセスで読み込みます |
| `deploy/values.yaml` | 本番環境でしか使われず、どのテストにも影響しません |

テストに関係するものもあれば、関係しないものもあります。smates はそれを自力では見分けられないため、
決まった手順で処理します。

## オーファンの処理手順 { #how-smates-handles-an-orphan }

各ステップは、それより前のステップで処理しきれなかったものを扱います。

1. **名前による参照。** ソースや依存の中でそのファイル名に言及しているテストを選択します。たとえば
   `"seed.sql"` を読み込むテストや、マイグレーションを列挙したフィクスチャなどです。
2. **テストされていないコードはスキップ。** オーファンがスイートのルート配下にある Go・Python・JS/TS
   のソースで、どのテストも（直接にも、他のコード経由でも）import しておらず実行もしていなければ、
   影響を受けるテストはありません。何も実行せず、サマリーとプルリクエストのコメントの
   「changes to untested code」に一覧が出ます。`main` パッケージや、テストのないコンポーネントなどが該当します。
3. **エージェント。** API キーが設定されていれば、[LLM エージェント](agent.md) が読み取り専用の
   ツールでリポジトリを調べます。そのうえで特定のテストを追加するか、全件実行を求めるか、どのテストにも
   影響しないと答えます。エージェントにできるのはテストの *追加* だけなので、誤った回答でも最悪の場合は
   テストを追加し損ねるだけです。
4. **スイート全体の実行。** それ以外の場合は、そのオーファンが影響しうるスイートを丸ごと実行します。

エージェントに問い合わせた実行では、次のようなログが出ます。

```text
smates: agent: asking claude-haiku-5-5 (anthropic) about 2 unattributed change(s): Dockerfile, db/seed.sql
smates: agent: db/seed.sql is loaded by the repository tests; the Dockerfile only builds the release image. (4 turns, 5120 in / 210 out tokens, 3.1s)
```

処理したオーファンはステートに書き込まれるので、同じブランチの以降の実行で再び処理されることは
ありません。smates を呼び出すコーディングエージェントは、ステップ 3 と 4 の代わりに自分で判断できます。
[呼び出し元のエージェントに判断させる](coding-agents.md#letting-the-calling-agent-decide) を
参照してください。

??? note "詳細: 「影響しうるスイート」の決まり方"
    ステップ 4 で影響しうるとみなすのは、`root` がそのファイルを含むスイートです。リポジトリの
    ルートを root とするスイート（`root: .`）はすべてのファイルを含みます。そのスイートしかファイルを
    含まない場合は、すべてのスイートを実行します。どのスイートの root にも入らないファイル
    （スイートが `packages/` 配下にあるリポジトリの、トップレベルの `Dockerfile` など）も、すべての
    スイートを実行させます。

??? note "詳細: ステップ 2 でテストされていないコードとみなさないもの"
    コードで書かれたツール設定（`vite.config.ts`、`setup.py`、テストのセットアップファイル、`.d.ts`）、
    `testdata/`・`fixtures/`・`__mocks__/` 配下のファイル、スイートが除外しているファイルは、
    ステップ 3 に進みます。まだ記録された実行がないテストを含むスイートの、root 配下のファイルも
    同様です。記録済みのステートがないときは import しか分からないため、ステップ 2 では何もスキップ
    しません。

    *別プロセスが* 実行するコード（e2e スイートがアクセスするサーバーなど）も、テストされていない
    ように見えます。`depends_on` か `rules` で宣言して、そのテストが選択されるようにしてください。
    [サーバーを叩くテスト](servers.md) を参照してください。

## ポリシーを選ぶ { #choose-a-policy }

`smates.yml` の `orphans:` で、ステップ 1 と 2 のあとに残ったオーファンの扱いを決めます。

```yaml
orphans: agent     # agent | all | ignore
untested: skip     # skip | orphan
```

| `orphans:` | 動作 |
|---|---|
| `agent`（既定） | エージェントに問い合わせます。API キーがないとき、またはエージェントが失敗したときは、影響しうるスイートを丸ごと実行します |
| `all` | 影響しうるスイートを丸ごと実行し、LLM は一切呼びません |
| `ignore` | 何もしません。ログに `ignored N unattributed change(s) (orphans: ignore)` と出ます |

`untested: orphan` にするとステップ 2 が無効になり、テストされていないソースも他のオーファンと
同じくエージェントかスイート全体の実行に回ります。import グラフを信頼するより、多めに実行したい
場合に使います。

1 回の実行に限って `orphans:` を上書きするには `smates run --orphans=agent|full|skip` を使います。
[コーディングエージェントから使う](coding-agents.md#letting-the-calling-agent-decide) を参照して
ください。

## テスト不要なファイルを無視する { #ignore-files-that-never-need-tests }

ほとんどのコミットに現れるオーファン（`.env.example`、デプロイ設定、どのスイートも扱わない
フロントエンドなど）は `ignore:` に書きます。

```yaml
ignore: ["deploy/**", ".env.example"]
```

書いたグロブは、組み込みのリストに追加されます。

- ドキュメント: `**/*.md`、`**/*.mdx`、`**/*.rst`、`docs/**`、`doc/**`
- ライセンス・変更履歴・オーナー: `LICENSE*`、`CHANGELOG*`、`CODEOWNERS`
- GitHub のメタデータ: issue と pull request のテンプレート、`dependabot.yml`
- エディタと dev container の設定: `.vscode/**`、`.idea/**`、`.editorconfig`、`.devcontainer/**`
- git のメタデータ: `.gitignore`、`.gitattributes`
- フォーマッタとリンタの設定: Prettier、ESLint、Biome、golangci-lint、Ruff、markdownlint、pre-commit

正確なグロブは
[internal/config/config.go](https://github.com/dotneet/smates/blob/main/internal/config/config.go)
の `DefaultIgnore` にあります。

!!! note
    `ignore:` が止めるのは、ファイルがオーファンになることだけです。無視対象のファイルを実際に読む
    テスト（`README.md` をパースするテストなど）があれば、そのファイルの変更でテストは選択されます。

YAML を手で編集せずにグロブを追加するには `smates config add-ignore 'deploy/**'` を実行します
（`--dry-run` で先に差分を確認できます）。

## 無視すべきものを見つける: `smates orphans` { #find-what-to-ignore-smates-orphans }

`smates orphans` は、直近のコミットを現在の依存マップに照らして再生し、オーファンになっていたはずの
ファイルを示します。`smates run --all` のあとに実行してください。記録された依存がないと静的な
import グラフしか分からないため、オーファンを多めに報告します。

```text
$ smates orphans
Replayed the last 100 first-parent commit(s) against the current dependency map.
23 of 100 commit(s) changed a file no test depends on; the agent would have been asked (orphans: agent).

Orphans by number of commits:
  9  .env.example         ignore: .env.example
  6  deploy/values.yaml   ignore: deploy/**
  4  Dockerfile           container definition: consider inputs:/rules: instead of ignore

Suggested ignore globs (no test depends on these files):
  .env.example  9 commit(s), 1 file(s)
  deploy/**     6 commit(s), 3 file(s)

May change how tests run; declare them in inputs: (every test) or rules: (some tests) instead of ignoring them:
  Dockerfile (container definition, 4 commit(s))

With these globs, 6 of 100 commit(s) would still have orphans (2 once the files above are in inputs:/rules:).

Paste into smates.yml:

ignore:
  - ".env.example"
  - "deploy/**"
```

- 各コミットを最初の親と比較し、`smates run` と同じ規則で判定します。
- `dir/**` のようにディレクトリごと提案するのは、その配下にテストの依存やテストが名前で参照する
  ファイルが 1 つもない場合だけです。
- テストの実行方法を変えうるファイル（CI ワークフロー、Makefile、Dockerfile、compose ファイル、
  `.nvmrc` や `go.mod` などのツールチェーン指定）は `ignore:` には提案しません。`inputs:`（全テストが
  依存）か `rules:`（一部のテストが依存）で宣言してください。

| フラグ | 動作 |
|---|---|
| `--history N` | 直近 N 個の first-parent コミットを再生します（既定 100、`0` は無制限） |
| `--since rev` | `rev` より後のコミットを再生します（`--history` を指定しない限り件数の上限なし） |
| `--apply` | 提案したグロブを `smates.yml` の `ignore:` に、コメントを保ったまま追記します。それ以外は変更しません |
| `--apply --dry-run` | 書き込まずに差分を表示します |
| `--suite NAME` | 再生をスイートに限定します（複数指定可） |
| `--format json` | 機械可読なレポートを出力します |

## `--suite` での実行とオーファン { #orphans-in-a-suite-run }

`--suite` で実行を絞った場合も、ほかのスイートのテストは依存元として数えます。ほかのスイートの
テストだけが依存しているファイルは、この実行のオーファンにはなりません。

実行から外したスイートの root 配下の変更は、そのスイートを実行するジョブに任せます。これにより、
スイートごとに分けた CI ジョブがステートを共有しても、各ジョブが自分のスイートのオーファンだけを
処理できます。

## 関連項目 { #see-also }

- [LLM エージェント](agent.md): ステップ 3 のプロバイダー、コスト、安全性
- [コーディングエージェントから使う](coding-agents.md): 呼び出し側にオーファンを判断させる
- [smates.yml リファレンス](../reference/configuration.md): `ignore`、`inputs`、`rules`、`orphans`、`untested`

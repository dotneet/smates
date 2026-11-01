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
D(T) = traced(T)   T の実行中(子プロセス含む)に実行・読み込みされたファイルと、
                   T のコードが読んだ環境変数
     ∪ static(T)   現時点で計算した import グラフの閉包(新しい import も即座に反映)
     ∪ inputs      lockfile、マニフェスト、ランナー設定、smates.yml、指定した glob
```

前回成功していて、`D(T)` の全ファイルのハッシュが今も同じときだけ `T` を **スキップ**し、
それ以外は理由付きで **選択** します(`smates run --explain`)。比較は `git diff` ではなく
内容ハッシュなので、履歴不要(`fetch-depth: 1` で可)、未コミットの変更にも対応し、
rebase やマージコミットにも惑わされません。

**環境変数** も、テストが読んだものはファイルと同じく依存として記録します(Node と Bun の
`process.env`。jest と vitest がテストファイルごとに作る複製や `import.meta.env` も含む。
Python の `os.environ` / `os.getenv`、Go の `os.Getenv` / `os.LookupEnv`)。
`smates deps` では `$TZ` のように表示され、値のハッシュで比較します(値そのものは保存せず、
未設定と空文字は区別します)。値が変わったテストだけが `[env changed: TZ]` の理由で選択され、
失敗ヒントや `smates flaky` にも現れます。数えるのはリポジトリのコードがスタック上にある
読み取りだけで、ランナー自身の参照や、環境全体のコピー(`{...process.env}`、
`dict(os.environ)`、子プロセスの起動)、テスト自身が設定した値は含めません。比較するのは
そのスイートのテストから見える値で、`smates.yml` の `env:` はシェルの値より優先されます。

CI で記録した状態を手元に `state pull` しても全件が再実行されないよう、マシンや CI と
ローカルで値が違う変数は既定で無視します: `PATH`、`HOME`、`USER`、`TMPDIR`、ロケール
(`LANG`、`LC_*`)、端末(`TERM`、`COLUMNS`、色の指定)、CI のメタデータ(`CI`、`GITHUB_*`、
`RUNNER_*` など)、プロキシ、ツールチェーンの場所(`GOPATH`、`VIRTUAL_ENV`、`npm_*` など)、
smates 自身が設定するもの(`SMATES_*`、`NODE_OPTIONS`、`PYTHONPATH`、`GOFLAGS` など)。
一覧は [internal/config/config.go](internal/config/config.go) の `DefaultEnvIgnore` です。
`smates.yml` の `env_ignore` で追加、`env_track` で既定の無視リストから除外できます
(トップレベルとスイートごと)。`env_ignore: ["*"]` で環境変数の追跡を止められます。

```yaml
env_ignore: ["BUILD_*", DEPLOY_ID]
env_track: [LANG]
```

見えないもの: ランタイム自身が参照する変数(日付の `TZ`、ロケール、`GODEBUG`)、ネイティブ
コード、Go の `syscall.Getenv`、Bun の `Bun.env` / `import.meta.env`、トレースできない
コマンド(シェルスクリプト、バイナリ)からの読み取り。Go の `t.Setenv` は元の値を戻すために
参照するので、変数を設定する Go のテストもその外側の値に依存します(必要以上に実行されることがあります)。値は SHA-256 のハッシュで保存する
ため、推測しやすい値(弱いパスワードなど)は状態ファイルを読める人に確かめられます。
気になる変数は `env_ignore` に入れてください。CI とローカルの違いの扱いは
[docs/DESIGN.md](docs/DESIGN.md) にあります。

**オーファン(どのテストにも紐付かない変更)** は次の順で扱います(`--suite` で絞った
場合も、他のスイートのテストが依存する変更はオーファンになりません)。

1. テストのソースや依存ファイルがそのファイル名に言及していれば選択(文字列で参照される
   fixture・マイグレーション・スキーマなど)。
2. 残りは **エージェント** に問い合わせ(`ANTHROPIC_API_KEY`・`OPENAI_API_KEY`・
   `OPENROUTER_API_KEY` のいずれかがある場合)。読み取り専用
   ツールでリポジトリを調べ、特定のテストを追加するか全実行を求めます。テストを削除する
   ことはできないため、誤答や指示注入があっても「追加し損ねる」以上の害はありません。
3. それ以外の場合は、そのファイルが影響しうるスイートを丸ごと実行します。

多くのコミットで繰り返しオーファンになるファイル(`.env.example`、デプロイ設定、どのスイートにも
属さないフロントエンドなど)は `ignore:` に入れるべきものです。**`smates orphans`** は直近の
first-parent コミット(既定 100 件。`--history N`、`--since rev`)を現在の依存マップに同じ規則で
当てはめ、オーファンになったはずのファイルを件数順に並べ、そのまま貼れる `ignore:` ブロック
(配下にテストの依存がなければ `dir/**` にまとめる)と、適用後もオーファンが残るコミット数を
表示します。テストの実行に影響しうるファイル(CI ワークフロー、Makefile、Dockerfile、compose、
ツールチェーンの版指定)は無視せず `inputs:` か `rules:` に書くよう案内します。依存が記録されて
いないと静的解析だけで判定するため結果は悲観的になります。先に `smates run --all` を実行して
ください(`--format json` で機械可読な出力)。

## クイックスタート

```bash
curl -fsSL https://raw.githubusercontent.com/dotneet/smates/main/install.sh | sh
# (SMATES_VERIFY=attestation で gh CLI によるビルドプロベナンスの検証も行います)

smates init --github # スイート検出、smates.yml(と GitHub ワークフロー)生成、.smates/ を gitignore
smates run --all     # 一度すべて実行して各テストの依存を記録
smates run           # 以後は変更の影響を受けるテストだけ実行
smates why src/pricing.ts     # このファイルに依存するテストは?
smates deps src/cart.test.ts  # このテストは何に依存している?
smates hook install  # 任意: git push の前に影響するテストを実行
```

### git フック

`smates hook install` は `pre-push` フック(`--hook pre-commit` で pre-commit)を書き込み、
`smates run --no-agent` を実行して、選ばれたテストが失敗したら push を止めます。
エージェントを使わないので API 呼び出しは発生しません(どのテストも依存していない変更は、
影響しうるスイートを丸ごと実行します)。`--agent` でエージェントを有効にでき、`--` の後に
`smates run` のフラグを渡せます(`smates hook install -- --explain`)。

- `core.hooksPath` を尊重し、リポジトリのすべての worktree で共有されます。
- smates が書いていない既存のフックは上書きしません。追加すべき行を表示して中断するか、
  `--append` でシェルスクリプトの末尾に追記します。husky や lefthook を使っている場合は、
  `.husky/pre-push` や `lefthook.yml` に追加する設定例を表示します。
- ローカルに state がなければ共有 state を取得し(`smates state pull`)、ローカルの state が
  1 週間より古いか、現在の履歴にないコミットで記録されたものなら、リモートの state を
  マージします(fetch は 1 時間に 1 回まで)。
- `git push --no-verify` または `SMATES_SKIP_HOOK=1` でスキップできます。`smates` が `PATH`
  にない環境では警告を出して push を通します。

smates は push されるコミットではなく作業ツリーの内容で判定するため、未コミットの変更
(pre-commit ではステージされていない変更)や未追跡ファイルもテスト対象に含まれます。
その場合や、push するブランチがチェックアウト中のものと異なる場合は警告を出します。
正確に判定したいときは、先にコミットするか stash してください。`smates hook uninstall` は
smates が書いた部分だけを削除し、`smates hook status` はフックの場所を表示します。

### GitHub Actions

```yaml
      - uses: dotneet/smates@v1
        with:
          anthropic-api-key: ${{ secrets.ANTHROPIC_API_KEY }}   # 任意(openai-api-key / openrouter-api-key も可)
```

デフォルトブランチへの push では全テストを実行して状態を更新し、プルリクエストでは
影響のあるテストだけを実行します。状態はブランチごとに Actions cache に保存され、
デフォルトブランチの状態にフォールバックします。`uses: dotneet/smates@<コミット SHA> # v1.2.3`
のようにコミット SHA で固定しても、そのコミットのリリースバイナリがインストールされます。
リリースのアーカイブにはビルドプロベナンスの attestation が付いており、`verify: attestation`
で必須にできます(既定の `auto` は検証できるときに検証)。詳細は
[docs/ci/github-actions.md](docs/ci/github-actions.md)、他の CI は
[docs/ci/other-ci.md](docs/ci/other-ci.md) を参照してください。

### プルリクエストのサマリ

`comment: true`(ジョブに `permissions: pull-requests: write` が必要)を指定すると、
選択結果のサマリをプルリクエストのコメントとして投稿し、push のたびに同じコメントを
更新します。変更が影響するテスト数とスイート、変更ファイルごとに選ばれたテストとその
依存の種類(静的 import、前回実行時に追跡した読み込み、スイート入力など)、どのテストも
依存していない変更の扱い(参照スキャン・エージェント・`orphans` ポリシー)、スキップした
テストの記録済み実行時間を表示します。フォークからのプルリクエストや権限不足の場合は
notice を出してスキップし、ジョブは失敗させません。シャード実行では各シャードが担当分の
コメントを個別に持ちます。他の CI では `smates run --markdown <file>` や
`smates select --format markdown` で同じ Markdown を出力できます。

## シャドーモード

`smates run --shadow` は全テストを実行しつつ選択結果を評価し、「選択していれば
見逃していた失敗テスト」を報告します。新しいリポジトリで選択結果を信頼する前の試用や、
夜間ジョブでの継続的な検証に使えます。

## 失敗しそうな順の実行と `--fail-fast`

`smates run` は選択したテストを次の順に起動します。前回成功しなかったテスト、テストファイル
自身またはそれが直接 import するファイルが変わったテスト、間接的な依存(推移的な import、
トレースで読んだデータ、設定、ファイル名による参照、エージェント)で選ばれたテスト、最後に
新規テストとその他です。同じ順位の中では記録済みの所要時間が短いものが先です。スイートは
最優先のテストの順位順に実行し、ランナーにもこの順でファイルを渡します(pytest と `go test`
はこの順で起動します。vitest と jest は独自のシーケンサで並べますが、こちらも前回失敗した
ファイルを先にします)。

**`--fail-fast`** を付けると、各スイートを順位ごとに別々のランナー呼び出しに分け、スイートを
またいで順位順に実行します。ある呼び出しが失敗したら、それ以降の呼び出しは起動しません
(実行中のものは完了を待ちます)。起動しなかったテストは記録しないので次回も選ばれ、ログ・
サマリ・`--report`(`not_run`)に一覧が出ます。ランナーの起動回数が増えるため、分割するのは
`--fail-fast` のときだけです。`--shadow` とは併用できません。`smates watch --fail-fast` で
ウォッチモードの各サイクルにも使えます。

## flaky テストの検出

smates は各テストの全入力の内容ハッシュを知っているので、**入力が何ひとつ変わって
いないのにテスト結果が pass と fail の間で反転した** 場合、そのテストは flaky である
(または smates から見えないもの — 時刻、ネットワーク、無視している・読み取りが見えない
環境変数、リポジトリ外のファイル — に依存している)と、ヒューリスティックではなく証拠に基づいて判定できます。たとえば
失敗したテストが再実行で成功した場合や、デフォルトブランチの全件実行で、何も触れて
いないテストが失敗した場合です。

`smates run` はこれを実行ログ、`--report` の `flaky`、ジョブサマリ、GitHub の出力
(`flaky` / `flaky-tests`)で報告し、証拠(反転回数、最後のコミット、直近の結果)を
状態ファイルに記録します。`smates flaky`(`--format json`、`--clear [テスト...]`)で
一覧できます(そのテストが読む環境変数も表示します。反転の前後で値は同じでした)。flaky な失敗も通常どおり実行を失敗させ、次回も必ず選択されます。

`smates.yml` に `flaky: {retry: 1}` を書くと、スイート実行の失敗がすべて *既に flaky と
分かっている* テストだったときに、同じテスト一式でスイート実行を 1 回だけやり直し、
成功すれば実行全体を失敗にしません(反転は報告されます)。失敗したテストだけでなく
同じ一式を再実行するので、テストに紐づかないエラー(未処理のエラーや teardown の失敗)
は隠れません。既定値は 0 で、失敗を隠すことはありません。

## 導入効果と信頼性の集計(`smates stats`)

`smates stats` は状態ファイルに記録された実行を集計します。実行したテストユニット数と
全ユニット数(スキップ率)、スキップしたユニットの記録済み所要時間の合計(削減できた時間の
見積もり。30 日あたりの換算も表示)、シャドーモードで見つかった見逃しの件数と率、
オーファン(どのテストの依存でもない変更)の件数と処理の内訳(参照スキャン、エージェント、
スイート全体の実行)、エージェントの呼び出し回数・トークン数・コスト(`claude -p` のように
バックエンドが報告する場合)、flaky の反転件数を表示します。

`--since 30d`(または `2026-01-02` などの日付)、`--branch <名前>`、`--ci`(CI での実行
のみ)で絞り込み、`--format json` で JSON を出力できます。`smates run` は終了時に実行ごとの
短い記録を追加します(`--no-record` と `--dry-run` では記録しません)。記録は直近 1000 件・
180 日以内に限って保持し、`smates state merge` では和集合を取るので、CI で復元した状態の
集計にはその状態が経てきた実行が含まれます。シャード実行ではシャードごとに 1 件記録します(オーファンはシャード 1 だけが数えます)。
ジョブサマリには累積の削減時間を 1 行で表示します。

## 失敗ヒント

テストが失敗したとき、smates はそのテストが使ったファイルと、そのうち変更されたものを
既に知っています。そこで `smates run` は失敗したテストごとに **入力のうち変更された
ファイル** を一覧にします。たいていは数個、多くの場合 1 つです。

```
smates: failure hints for 1 failing test unit(s):
  web: src/cart.test.ts — 2 of its 37 known inputs changed since its last recorded run (3f2a1c9, passed)
      src/price.ts  (source, direct import, modified)
      fixtures/prices.json  (data, traced, modified)
      $TZ  (environment variable, read in its last run, changed)
    → agent (high confidence): src/price.ts:12 — roundCents now truncates instead of rounding, so the total is off by one cent
```

- 「変更」は、そのテストを最後に実行したときに記録した内容との比較です(記録のない
  テストは、今回の選択で変更と判定されたファイルとの比較)。候補は、テストが直接
  import するコード、その他のコード、テストファイル自身と読んだ環境変数、データ、設定、
  ディレクトリ列挙の順に並べ、同じ種類の中では更新時刻の新しい順にします。
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

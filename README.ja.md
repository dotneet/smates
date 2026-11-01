# smates

**変更が影響しうるテストだけを実行する** — TypeScript/JavaScript (vitest, jest,
`node --test`, bun)、Go、Python (pytest, unittest, uv) 対応。CI でもローカルでも使えます。

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

テスト単位 `T`(テストファイル。Go はパッケージ、`granularity: func` ならテスト関数)ごとに、使った入力と、最後に実行した時点の
git blob ハッシュを保存します。

```
D(T) = traced(T)   T の実行中(子プロセス含む)に実行・読み込みされたファイルと、
                   T のコードが読んだ環境変数
     ∪ static(T)   現時点で計算した import グラフの閉包(新しい import も即座に反映)
     ∪ inputs      lockfile、マニフェスト、ランナー設定、指定した glob、smates.yml の
                   そのスイート自身の設定(ignore・agent・orphans や他スイートの
                   変更ではテストを再実行しません)
     ∪ depends_on  テストが別プロセスで叩くサーバーのコード(宣言したもの)
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
場合も、他のスイートのテストが依存する変更や、対象外のスイートの root 配下の変更は
オーファンになりません。後者はそのスイートを実行するジョブが扱います)。

1. テストのソースや依存ファイルがそのファイル名に言及していれば選択(文字列で参照される
   fixture・マイグレーション・スキーマなど)。
2. **テストの無いコード** はスキップ: スイートの root 配下にある Go・Python・JS/TS の
   ソースで、どのテストからも(他のコード経由でも)import されず、実行もされていないもの
   (`main` パッケージ、テストの無いコンポーネントなど)。影響を受けるテストが無いので何も
   実行せず、サマリーと PR コメントに「テストの無いコードの変更」として表示します。コードで
   書かれたツール設定(`vite.config.ts`、`setup.py`、setup ファイル)、`testdata/`・`fixtures/`・
   `__mocks__/` 配下、スイートの `exclude` に当たるファイルは対象外で、記録済みの状態が無い
   (import しか分からない)ときは適用しません。別プロセス
   で動くコード(e2e が叩くサーバー)は `rules` で宣言してください。
   `untested: orphan` にすると従来どおり次の手順で扱います。
3. 残りは **エージェント** に問い合わせ(`ANTHROPIC_API_KEY`・`OPENAI_API_KEY`・
   `OPENROUTER_API_KEY` のいずれかがある場合)。読み取り専用
   ツールでリポジトリを調べ、特定のテストを追加するか全実行を求めます。テストを削除する
   ことはできないため、誤答や指示注入があっても「追加し損ねる」以上の害はありません。
4. それ以外の場合は、そのファイルが影響しうるスイートを丸ごと実行します。

smates を呼ぶコーディングエージェントが `--add` と `--orphans` で代わりに判断することも
できます([呼び出し元のエージェントに判断させる](#呼び出し元のエージェントに判断させるletting-the-calling-agent-decide))。

多くのコミットで繰り返しオーファンになるファイル(`.env.example`、デプロイ設定、どのスイートにも
属さないフロントエンドなど)は `ignore:` に入れるべきものです。**`smates orphans`** は直近の
first-parent コミット(既定 100 件。`--history N`、`--since rev`)を現在の依存マップに同じ規則で
当てはめ、オーファンになったはずのファイルを件数順に並べ、そのまま貼れる `ignore:` ブロック
(配下にテストの依存がなければ `dir/**` にまとめる)と、適用後もオーファンが残るコミット数を
表示します。テストの実行に影響しうるファイル(CI ワークフロー、Makefile、Dockerfile、compose、
ツールチェーンの版指定)は無視せず `inputs:` か `rules:` に書くよう案内します。依存が記録されて
いないと静的解析だけで判定するため結果は悲観的になります。先に `smates run --all` を実行して
ください(`--format json` で機械可読な出力)。

### 呼び出し元のエージェントに判断させる(Letting the calling agent decide)

smates を呼ぶのがコーディングエージェント(Claude Code、Codex など)なら、何をなぜ変更したかは
そのエージェントが一番よく知っています。そこから別の LLM を呼ぶのは無駄で、スイートを丸ごと
実行するのは遅いので、オーファンの判断を呼び出し元に任せられます。

1. `smates select --format json` の `orphan_candidates` にオーファンごとの情報が入ります:
   `path`、`status`(`added`/`modified`/`deleted`)、`referenced_by`(そのファイル名に言及
   するテストと、言及している `file`、すでに `selected` か)、`suites`(オーファンポリシーが
   丸ごと実行するスイート)、`handling`(この選択での扱い: `reference`・`agent`・
   `no-impact`・`policy`・`ignored`・`skipped`)。JSON の既存フィールドは変わりません。
2. `smates run --add <suite>:<unit>` でテストを追加します(繰り返し指定可、理由は `caller`)。
   unit の代わりにテストファイルも指定できます(`--add api:tests/test_users.py`、Go の
   パッケージ単位なら `--add pkg/foo_test.go`。パスはリポジトリ相対、カレントディレクトリ相対、
   `suite:` の後ならスイートの root 相対。該当するスイートが 1 つならスイート名は省略可)。
   この実行のどのテストにも当たらない指定はエラー(終了コード 2)です。
3. `--orphans=agent|full|skip` でこの実行に限り `orphans:` の設定を上書きします。`agent` は
   組み込みエージェント、`full` は影響しうるスイートの丸ごと実行(`orphans: all`)、`skip` は
   参照スキャンで説明できなかった変更にオーファンポリシーを適用しません。

```sh
smates select --format json        # orphan_candidates を読む
smates run --orphans=skip --add api:tests/test_users.py --add web:src/app.test.ts
```

安全原則は変わりません。呼び出し元はテストを追加できるだけで、決定的な選択(と名前参照
スキャン)が選んだテストを外すことはできません。`--orphans=skip` は、変更を自分で判断した
呼び出し元のための唯一の明示的なオプトアウトです。ログ・ジョブサマリ・実行記録
(`smates stats`)ではそのオーファンが「skipped by caller」と表示され、処理済みのオーファンと
同じく同じブランチで再び報告されることはありません。どちらのフラグも `smates select`
(結果の確認用)と git フックの run フラグ(`smates hook install -- --orphans=full`)でも
使えます。オーファンのためにスイートが丸ごと実行され、呼び出し元が何も指定していない場合
(API キーがないときなど)は、代わりに判断する方法をログに表示します。

### サーバーを叩くテスト

E2E テスト(Playwright、vitest の e2e、HTTP API に対する pytest など)は、別プロセスや
コンテナで動くサーバー(docker compose で起動した Go のバックエンドなど)にアクセスする
ことがよくあります。トレースが見るのはテストのプロセスでサーバーではないため、サーバーの
コードを変えてもこれらのテストは選ばれません。E2E スイートに依存を宣言してください。

```yaml
  - name: e2e
    runner: custom
    root: e2e
    command: "npx playwright test"
    depends_on: [api]              # スイート api のコード
    # または: depends_on: [{paths: ["backend/**", "docker-compose.yml"], tests: ["admin/**"]}]
    # または: serves: ["backend/**"]
```

`{suite: api}`(または単に `api`)は、そのスイートの root 以下のファイルから、(どの
スイートのものであれ)テストファイル、無視・vendor 扱いのファイル、さらに下に root を持つ
別スイートのファイルを除いたものです。`paths` はリポジトリ相対の glob、`tests`(スイートの
root からの相対)で依存を付けるテストを絞れます。宣言ごとに、全ファイルをまとめたハッシュを
持つ 1 つのキーとして `D(T)` に加わる(HEAD で計算し、テスト実行時に記録。サーバーの規模に
比例して状態が大きくなることはありません)ので、変更するとテストが選ばれ、`--suite` で
絞ってもオーファンになりません。`--explain`、`smates why`、プルリクエストのサマリには
変更されたファイルと宣言名が出ます。

```
+ e2e/login.spec.ts  [changed: backend/api/user.go (depends_on suite api)]
```

`smates doctor` は、E2E らしいのに `depends_on`・`serves`・`rules` のどれもないスイート
(名前や root に e2e を含む、コマンドが Playwright や Cypress を実行する、テストが
`http://localhost` を呼ぶ)を指摘します。

## 対応ランナー

| ランナー | 単位 | 依存のトレース方法 | ファイルごとの結果 |
|---|---|---|---|
| `vitest` | テストファイル | ワーカーごとの V8 カバレッジ + fs フック。globalSetup の読み取りは全テストの依存 | 注入するレポーター |
| `jest` | テストファイル | ワーカーごとの V8 カバレッジ + fs フック。ワーカーはファイルごとに作り直す(`isolate: true` で厳密に 1 ファイル 1 プロセス) | 注入するレポーター |
| `node`(`node --test`) | テストファイル | テストプロセスごとの V8 カバレッジ + fs フック(`--test-isolation=none` では `isolate: true` で 1 ファイル 1 プロセス) | プロセスの終了コード |
| `bun` | テストファイル | ファイルごとの `bun test --coverage` + モジュールキャッシュ、`node:fs`・`Bun.file`・`Bun.Glob` の preload フック(記録時) | プロセスの終了コード |
| `pytest` | テストファイル | `sys.monitoring`(3.12+)/ `sys.setprofile`、audit フック。テスト・fixture・収集したモジュールごと、モジュールレベルの読み取りも | プラグイン |
| `unittest`(`python -m unittest`) | テストモジュール | Python と同じトレース。選択したモジュールを 1 プロセスで、モジュールごとのコンテキスト(import、`setUpModule`/`setUpClass`、テスト)を付ける `unittest` ラッパー経由で実行 | ラッパーの結果クラス |
| `go` | パッケージ | 静的 import 閉包(Go では正確)+ `go test -exec` 経由の `-test.testlogfile`。テストの Node/Python 子プロセス | バイナリの終了コード、ビルド失敗 |
| `custom` | テストファイル | コマンドが起動する Node/Python プロセス | 全体の終了コード |

Python のプロジェクトは、pytest を使っている形跡があれば `pytest`、なければ `unittest`
として検出します。形跡とは、`pytest.ini` や `conftest.py`、プロジェクトかその上位
ディレクトリのマニフェスト・ロックファイル・タスクファイル(`pyproject.toml`、`setup.cfg`、
`setup.py`、`tox.ini`、`noxfile.py`、`Pipfile`、`poetry.lock`/`pdm.lock`/`uv.lock`、
`*requirements*.txt`/`.in`、`requirements/` 配下のファイル)での pytest への言及、pytest を
実行する GitHub ワークフロー、pytest 向けに書かれたテストファイル(`import pytest`、
モジュールレベルの `def test_…` 関数)です。`unittest`
ランナーのテストファイルはディスカバリーに従い、既定はスイート root 以下の `test*.py`、
`python -m unittest discover …` のコマンドなら `-s`/`-p`/`-t`(開始ディレクトリ、パターン、
トップレベルディレクトリ)で決まります。選択したファイルは常にトップレベルディレクトリからの
ドット区切りのモジュール名で渡します(全実行でも同じなので、`__init__.py` のない
ディレクトリも飛ばされません)。モジュール名にならないパスのファイルはテストとみなしません。
記録時は選択したモジュールを Python シムに同梱の `python -m smates_unittest` で 1 プロセスで
実行し、モジュールの import 中に送出された例外は実行全体の中断ではなくそのモジュールの失敗に
なります。`isolate: true` ならモジュールごとに 1 プロセスで実行します。unittest のテストを
1 つも実行しなかったモジュールはソースで区別します。パターンに一致しただけの補助モジュールは
成功として記録し(`exclude` を勧める警告付き)、pytest 向けに書かれたもの(`import pytest`、
モジュールレベルの `def test_…` 関数)は成功として記録せず、結果を unknown として次回も
実行し、`runner: pytest` を勧めるメッセージを出します。どのモジュールもテストを実行しなかった
実行は、その中に pytest 向けのものがある場合だけ(unittest の終了コード 5 のまま)失敗になります。

```yaml
  - name: legacy
    runner: unittest                 # python -m unittest(pytest 不要)
    root: legacy
    command: "python3 -m unittest discover -s tests -p '*_check.py'"  # -s/-p/-t でテストファイルを指定
```

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
smates hook install --claude  # 任意: Claude Code が終了前に失敗したテストを直すようにする
smates guide         # コーディングエージェント向けの使い方(docs/AGENTS.md)
```

`init --github` は `mise.toml`/`.tool-versions`(`jdx/mise-action` を使用)、
`.nvmrc`/`.node-version` または `package.json` の `engines.node`/`volta`、`.python-version`
からツールチェーンを設定します。`docker-compose.yml`/`compose.yaml` のデータベースやコード中の
`localhost` URL、`.env.example` にありコードが読む環境変数は、確認して有効にするための
コメントアウトされた `services:`/`env:` ブロックになります。既存のワークフローがすでにテストを
実行している場合は 2 つ目のワークフローを書かず(`--force` で書く)、そのステップを smates
アクションに置き換えるパッチを表示します(`--patch FILE` で `git apply` 用に書き出し)。
`smates.yml` がすでにある場合、`smates init --github` はそれを変更せず、そのスイートから
ワークフロー(またはパッチ)だけを生成します(`--force` で `smates.yml` も書き直し)。
Makefile の `test` ターゲットのフラグや環境変数は、`smates.yml` にコメントの
`command:`/`env:` ヒントとして書かれます。`smates doctor` も同じ内容に加えて、
`node_modules` の欠如、`engines.node` を満たさない Node、起動していないサービスを報告します。
smates は `npx`/`bunx` によるランナーの暗黙のダウンロードを許さず、`node_modules/.bin` に
ないときはエラーにします。

### どのファイルがテストか

`smates.yml` で `tests:` を指定しないスイートでは、テストファイルをランナー自身の設定から
静的に(設定ファイルを実行せずに)読み取ります。

- **jest**: コマンドの `--config`、なければスイートのルートから上にたどって最も近い
  `jest.config.{js,ts,mjs,cjs,mts,cts,json}` か `package.json` の `"jest"`(jest と同じく
  最も近い `package.json` で探索を止めます)。`rootDir`、`roots`、`testMatch`(`?(x)` や
  `+(spec|test)` などの micromatch の extglob は変換し、`!` のパターンは除外)、`testRegex`、
  `testPathIgnorePatterns`(`<rootDir>` を置換)を読みます。
- **vitest**: コマンドの `--config`、なければ上にたどって最も近い `vitest.config.*` /
  `vite.config.*`。`root`、`test.root`、`test.dir`、`test.include`、`test.exclude`
  (`...configDefaults.exclude` も解釈)と、プロジェクト(`test.projects`、`test.workspace`、
  `vitest.workspace.*`)を読みます。ワークスペースは 1 つのスイートとし、各プロジェクトの
  include/exclude をそのルート以下に適用した和集合をテストファイルとします。読めない設定の
  プロジェクトは、そのディレクトリに既定のパターンを使います。ルートの vitest 設定がネストした
  パッケージ(独自の `vitest.config.*` を持ち、`projects` に含まれないもの)のテストを数えない
  ときは、自動検出でそのパッケージを別スイートにします。
- **pytest**: スイートのルートから上にたどって最初に見つかる `pytest.ini`、`.pytest.ini`、
  `pyproject.toml`(`[tool.pytest.ini_options]` / `[tool.pytest]`)、`tox.ini`(`[pytest]`)、
  `setup.cfg`(`[tool:pytest]`)。`python_files` と、その設定ファイルがスイートのルートにある
  場合は `testpaths` を読みます(pytest が `testpaths` を使うのは rootdir で引数なしに
  起動したときだけのため)。

JS/TS の設定は、オブジェクトリテラルを export していて(`module.exports = {...}`、
`export default {...}`、`defineConfig({...})`、またはそれが指すトップレベルの `const`)、値が文字列・配列・
正規表現のリテラルのときに読めます。それ以外(関数、`mergeConfig(base, ...)` などほかの関数呼び出し、`testMatch: dirs.map(...)`、後から `config.roots.push(...)` などで変更される `const`、オプションを
設定しうる `...spread`、jest の `projects`、Go の `regexp` にない先読みを使う `testRegex` など)
は既定の glob にフォールバックし、`smates list` / `smates doctor` が理由とともに警告します。
その場合は `smates.yml` に `tests:`(と `exclude:`)を書いてください。

`smates list` は各スイートのテストの出所を表示し(`tests=jest.config.js testMatch`、`-v` で
パターンも表示)、`smates doctor` も同様です。`smates.yml` の `tests:`/`exclude:` と違い、
設定から読み取ったパターンは全件実行(`smates run --all`)で全ファイルを明示的に渡す理由には
なりません。ランナー自身が見つける集合を表しているためです。

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

### 並行する worktree

コーディングエージェントは複数の `git worktree` で同時に作業することがよくあります。
state は worktree ごとの `.smates/state.json.gz` なので、新しい worktree は記録がない状態から
始まってしまいます。そこで、state のないリンクされた worktree では、state を必要とする最初の
コマンドが次の順で state を用意し(シード)、その出どころを 1 行表示します。

1. メインの worktree に state があれば、それをコピーします。
2. なければリモートの state(`smates state push`)を使います。fetch はフックと同じく
   1 時間に 1 回までで、別の worktree が取得済みの state ref は fetch せずに使います。

シードしたスナップショットは、この worktree の履歴にあるコミットで記録されたものだけを残します。
そうでなければテストごとの記録だけを引き継ぎ、変更ファイルはスナップショットがないときと
同じくデフォルトブランチと比べて判定します。シードは worktree ごとに 1 回だけで、
`smates state clear` の後は再シードしません。`smates.yml` の `state_seed: false` または
`SMATES_NO_STATE_SEED=1` で無効にできます。

どの worktree で学んだことも全 worktree で共有するには `state_shared: true` を設定します。
テストごとの記録、エージェントの回答、実行記録は git の共通ディレクトリにある 1 つのファイル
(`.git/smates/state.json.gz`。`smates state path` で表示)に置かれ、各 worktree は自分の
ファイル全体のスナップショット(`.smates/state.snapshot.json.gz`)だけを持ちます。そのため、
あるブランチでの実行が別のブランチの変更をこちらの変更に見せることはありません。保存時は
共有 state の隣のロックファイルを取り、ディスク上の内容にこの実行の変更をマージするので、
複数の worktree で同時に動く smates が互いの結果を失いません(同じテストを両方が実行した
場合は新しい結果を採用し、結果の履歴は統合します。`smates flaky --clear` や、全 worktree の
state を消す `smates state clear` が、それより前に始まった実行によって元に戻ることもありません)。クラッシュしたプロセスが残したロックは
2 分後に引き継ぎます。`smates state show` は state の場所、共有かどうか、シード元を表示します。

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

## コマンド

| コマンド | |
|---|---|
| `smates run [-- args]` | テストを選び、トレース付きで実行し、state を更新。`--all`、`--select`、`--shard I/N`、`--shadow`、`--fail-fast`、`--explain`、`--no-agent`、`--no-record`、`--report file.json`、`--markdown file.md`、`--summary file.md`、`--dry-run` |
| `smates select` | 選択結果だけを表示: `--format text\|files\|json\|github\|markdown` |
| `smates watch [-- args]` | 保存のたびに影響するテストを再実行 |
| `smates init [--github]` | `smates.yml` を生成(`--github` でワークフローも)。`--format json` は検出したスイート、書いたファイル、判断が必要な項目 `needs_review`(サービス、コードが読む環境変数、Makefile のフラグ、`depends_on` のない E2E スイートなど)を、根拠と推奨するコマンドや設定変更つきで出力 |
| `smates list` / `doctor` | スイートとテスト単位の一覧 / 環境チェック(失敗があれば終了コード 1)。`--format text\|json`: doctor の各チェックは安定した `id`、`status`(`ok\|warn\|fail`)、`message`、`fix`(`command`、`config` のキーと値、`doc`)を持つ |
| `smates why <file>` / `deps <test>` | 依存マップを調べる(`smates why '$TZ'` は変数を読むテスト): `--format text\|json` |
| `smates flaky` | flaky と判明したテストと根拠: `--format text\|json`、`--clear [test...]` |
| `smates stats` | 記録された実行の効果と選択の信頼性: `--since 30d\|2026-01-02`、`--branch`、`--ci`、`--format text\|json` |
| `smates orphans` | 直近のコミットを再生し、どのテストも説明しない変更に `ignore:` グロブ(と `inputs:`/`rules:`)を提案: `--history N`、`--since rev`、`--format text\|json`、`--suite`。`--apply` で提案した glob だけを `ignore:` に追記、`--apply --dry-run` で差分を表示 |
| `smates config add-ignore\|add-input <glob>...` / `config set <key> <value>` | YAML を手で編集せずに `smates.yml` を変更: トップレベルの `ignore:`/`inputs:` へ重複なく追記、またはスカラー値(`orphans`、`untested`、`state`、`agent.model`、`flaky.retry` など)を設定。コメントとレイアウトは保たれ、書き込む前に検証されます。`--dry-run` で差分表示、`--format json` |
| `smates state show\|pull\|push\|merge\|clear` | state の確認と移動(`show --format json`。`--json` は非推奨の別名) |
| `smates hook install\|uninstall\|status` | 影響するテストを実行する git フック |

## シャドーモード

`smates run --shadow` は全テストを実行しつつ選択結果を評価し、「選択していれば
見逃していた失敗テスト」を報告します。新しいリポジトリで選択結果を信頼する前の試用や、
夜間ジョブでの継続的な検証に使えます。`--no-record` を付けるとトレースを省き(大幅に
軽くなります)、state も更新しません。トレースなしでもファイル単位の結果を報告できる
ランナー(vitest、jest、pytest、`go test`、`junit:` を設定したランナー)では、この場合も
見逃しを検出できます(それ以外のランナーでは、失敗した実行の未選択テストは「見逃しの
可能性」として報告されます)。シャドーモードでは `go test` のキャッシュ結果を使いません
(`-count=1`)。go はモジュール外のファイルの変更を確認せずにキャッシュを再利用するためです。

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
  に出力されます。`--no-record` での実行でも、これらのランナーはトレースなしでファイル単位の
  結果を報告するのでヒントが出ます(それ以外のランナーは記録しない実行ではテストごとの
  結果を返しません)。
- **エージェントがある場合**(API キーあり、`--no-agent` なし、`agent.explain_failures` が
  false でない)、最大 `agent.explain_max` 件(既定 3)の失敗について、推定原因を 1 行
  (ファイル、行、理由、確信度)で示します。失敗ごとにツールなしの 1 リクエストで、失敗
  出力と上位候補の diff(サイズ上限あり。git-ignore されたファイルは送りません)を送ります。
  後続のスイートの実行中にバックグラウンドで行い、上限 30 秒。結果は内容をキーに状態へ
  キャッシュされ、エラーは注記を表示するだけで実行を失敗させません。

## Go のテスト関数単位の選択

Go の単位は既定ではパッケージです。1 つの大きなパッケージにテストが集中し、ほとんどの変更が
そこに届くプロジェクトでは、パッケージ単位の選択ではあまり時間が減りません。`go` スイートに
`granularity: func` を指定すると、トップレベルのテスト関数(`TestXxx`、シードコーパスを含む
`FuzzXxx`、`ExampleXxx`。サブテストは親と一緒)が単位になります(`internal/api#TestCreateUser`)。

```yaml
suites:
  - name: api
    runner: go
    granularity: func   # 既定は package
    record_jobs: 4      # 1 パッケージで同時に記録する関数の数(既定 1)
```

記録時は、パッケージごとにテストバイナリをリポジトリ内のリンクされるパッケージに対する
カバレッジ付きで 1 度だけビルドし(`go test -c -cover -coverpkg=...`。`-tags` や `-race`
などスイートのコマンドのビルドフラグを引き継ぎます)、関数ごとに別プロセスで実行します
(`-test.run '^TestX$'`、カバレッジプロファイル、テストログ)。各関数が依存するのは:

- 実行したコードを含む Go ファイル(ブロックが 1 つでも実行されたファイル。パッケージの
  初期化は毎プロセス走るので、すべての関数の依存になります)
- それ以外の到達しうる Go ファイルの *宣言*(関数本体を除いたファイル)。定数、変数の初期化式、
  型、シグネチャ、メソッドの変更はそれが見えるすべての関数を、関数本体の中だけの変更は
  それを実行した関数だけを選択します
- 自身の `_test.go`、そこで名前を使っているヘルパーを宣言するテストファイル、`TestMain`・
  `init`・呼び出しで初期化されるパッケージ変数を持つテストファイル
- パッケージ単位と同じく、テストログに記録されたファイル・ディレクトリ・環境変数、埋め込み
  ファイルなど静的閉包の Go 以外のファイル、`go.mod`/`go.sum`、子プロセスが使ったもの、
  サブプロセスとしてビルド・実行される Go パッケージの全体、パッケージディレクトリの一覧
  (ファイルが追加されるとそのパッケージの全関数が選択されます)

テストが自身のテストバイナリを再実行して動かすコード(ヘルパープロセスのパターン、
`exec.Command(os.Args[0], "-test.run=^TestX$")`)もそのテストの依存になります。バイナリが子に
関数のカバレッジディレクトリを渡すためです。カバレッジを書かずに終わった子(panic した、kill
された)がいると、その関数は到達しうるすべての Go ファイルに依存します。

コンパイルできないパッケージは、テストがどの関数本体を実行するかに関係なく `go test` で失敗します。
そのため、テスト関数を持つパッケージにはそれぞれ **コンパイルチェック** の単位
`internal/api#(build)` もあります。パッケージの静的閉包全体(自身の Go ファイルとテストファイル、
import するパッケージのファイル、埋め込みファイル)の内容に依存し、テストバイナリをビルドして vet
するだけで実行しない `go test -run '^$'` で動きます(smates が `-exec` のプログラムになり、
`TestMain` も走りません)。どのテストも実行しない関数本体の変更(型が合わなくなった未使用の
ヘルパー、未使用の import、vet の失敗)は、そのパッケージと import 元パッケージのチェックを選択し、
コンパイラの出力とともに失敗させます。通常の関数本体の変更では、それを実行した関数とこの軽い
チェックが選択されます。

変更されたテストファイルはそこで宣言された関数を、新しい関数は新規として選択します。記録しない
実行では `go test -run '^(TestA|TestB)$'` で選択された関数だけを実行し(パターンが長すぎる場合は
パッケージ全体)、`go test` の `--- FAIL` 行などから関数ごとの結果を読み取ります。カバレッジ付きで
ビルドできない場合や、スイートのコマンドが素の `go test` でない(または `-run`・`-skip`・
`-bench`・`-fuzz`・`-c` を渡す)場合は、そのパッケージをパッケージ単位で記録し、その旨を表示します。

コスト: 記録時は関数ごとに 1 プロセス(とパッケージごとに 1 回のカバレッジ付きビルド)かかる
ため、既定ブランチでの全件実行は遅くなります。選択的な実行では選ばれた関数だけが起動します。
同じパッケージの関数は `go test` と同じく 1 つずつ実行します(`record_jobs` で並列化)。関数は
プロセスを共有しないので、同じパッケージの別のテストがメモリに残した状態に頼るテストは
`go test` と違う振る舞いになります。リフレクションだけで実行されるコードは追跡できません。

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

## カバレッジツールとの併用

カバレッジの収集はスイートの `command` にそのまま書いてください。smates はその上で記録し、
レポートは smates なしで実行した場合と同じになります。

| ツール | 両立のしかた |
|---|---|
| `vitest --coverage`(v8 プロバイダ)、jest `--coverage --coverageProvider=v8` | `node:inspector` 経由で V8 のカバレッジを取得・停止するため、smates の `NODE_V8_COVERAGE` のファイルが空になります。smates はそれらが取得したカバレッジに含まれるスクリプトを代わりに記録します。 |
| `c8` | `NODE_V8_COVERAGE` が設定されていると c8 はそれを一時ディレクトリとして使うため、smates のファイルからレポートを作ります(`--reports-dir` の出力は通常どおり。`coverage/tmp` は作られません)。ランナーの起動ごとに別ディレクトリを使うので、c8 の `--clean` が他の起動のファイルを消すことはありません。 |
| `node --test --experimental-test-coverage` | そのまま動きます。smates のプリロードは `node_modules` ディレクトリに置かれ、カバレッジレポートから除外されます。 |
| ユーザー自身の `NODE_V8_COVERAGE`(環境変数またはスイートの `env`) | 記録中は置き換えられ、実行後に生のカバレッジファイルがそのディレクトリにコピーされます(既存のファイルを上書き・削除することはありません)。 |
| pytest-cov / coverage.py | すべてのコアに対応: `sys.settrace`(ctrace、pytrace)と `sys.monitoring`(`COVERAGE_CORE=sysmon`)。smates は専用の `sys.monitoring` ツール ID(3、4、2、5 の順。coverage.py は 1 を使い、使用中なら次に移ります)を取り、3.12 未満では既存のプロファイル関数を連鎖して呼びます。テスト終了後に pytest-cov がレポートのために読むファイルは、どのテストの依存にもなりません。 |
| pytest-xdist(`-n auto`) | プラグインは各ワーカーで読み込まれ、依存と結果は xdist なしと同じくテストファイル単位です。 |
| `go test -cover`、`-coverpkg`、`-coverprofile`、`-covermode` | smates の `-exec` ラッパーの影響を受けず、プロファイルは通常どおり書き出されます。`-coverpkg`(コマンド、`--` 以降、`GOFLAGS` のいずれか)を使うと go は指定パッケージをすべてのテストバイナリに組み込むため、各テストパッケージはそれらのソースファイルにも依存します(そこでのコンパイルエラーは全パッケージを失敗させます)。 |

制限事項: コマンド自身が `NODE_V8_COVERAGE` を設定する場合(`env NODE_V8_COVERAGE=… node`
や、それを export するスクリプト)、その配下のプロセスが実行したコードは smates から見えません
(静的グラフだけが頼りになります。環境変数かスイートの `env` で設定してください)。
また記録中の c8 は `coverage/tmp` を残さないため、後から `c8 report` を実行しても読むものが
ありません(同じコマンド内でレポートを出してください)。coverage.py の `sysmon` コアでは、
smates がテストの切り替えごとに監視イベントを再有効化するため coverage.py 側の処理も
テストごとに少し増えます(結果は変わりません)。

## AI コーディングエージェントから使う

**[docs/AGENTS.md](docs/AGENTS.md)** は、smates を使うリポジトリで作業するエージェント向けの
1 ページのガイドです(英語。`smates guide` で表示)。リポジトリに `AGENTS.md` か `CLAUDE.md`
があると、`smates init` はそこへ追記する短いセクションを提案します(`smates init --agents-md`
で一度だけ追記)。

エージェントはコマンドの出力をすべて読むため、`smates run --output failures`(または
環境変数 `SMATES_OUTPUT=agent`。フラグが優先)で出力を小さく保てます。

- ランナーのバッチごとの出力をバッファし、そのバッチが失敗したときだけ表示します。
  失敗したテストごとの出力を報告するランナー(vitest、jest、pytest、`go test`、JUnit)では
  その出力だけを、それ以外では失敗したバッチの出力を最大 200 行(先頭 30 行と末尾 170 行、
  間に省略の注記)表示します。
- smates 自身の進捗行は省き、選択結果の 1 行だけを残します。
- 最後に短いサマリを出します。成功なら 1 行、失敗なら失敗したテスト、それぞれの入力の
  うち変更されたもの(失敗ヒントから)、そしてそのテストだけを再実行するコマンドです。

```
smates: FAILED 1 of 7 test unit(s) in 3.2s:
  web: src/cart.test.ts
    changed inputs: src/price.ts (modified), fixtures/prices.json (modified)
smates: rerun them: smates run --output failures --only web:src/cart.test.ts
```

`--only suite:unit`(複数指定可)は指定したテスト単位(テストファイル、Go ではパッケージ
ディレクトリ)だけを実行し、通常どおり記録します。他のテストや変更は次回の実行に残ります。
`--report -` は JSON レポートを標準出力に書き、それ以外の出力はすべて標準エラーに出します。

**長時間の実行。** failures モードでは、出力のない状態が 30 秒続くたびに
`smates: still running <suite> (...)` という進捗行を出します(`SMATES_HEARTBEAT=10s` で間隔を
変更、`0` で無効)。ツールのタイムアウトが無出力の実行を停止と誤認しないためです。選択された
テストの記録済み実行時間の合計が `--budget`(既定 90s、`0` で無効)を超えると、`smates run` は
開始前に分割の仕方を表示します。スイートごとに実行する(`--suite`)か、`--report file.json`
を付けてバックグラウンドで実行します。

**Claude Code の Stop フック。** `smates hook install --claude` は `.claude/settings.json`
(`--user` なら `~/.claude/settings.json`。`smates.yml` のあるプロジェクトでのみ動作)に Stop
フックを追加します。Claude がターンを終えようとすると `smates hook claude-stop` が影響する
テストを実行し(`--output failures --no-agent`、`--` の後に追加のフラグ)、失敗があれば
`{"decision": "block", "reason": "<失敗したテストと再実行コマンド>"}` を出力して終了コード 0
で終わります。これは Claude Code が文書化している Stop フックの JSON 出力で、Claude は終了せず
reason を読んでテストを直します。フック入力の `stop_hook_active` が true(すでに Stop フックに
よって続行中)のときは決してブロックせず、何も選択されなければ state を書かずにすぐに終わり
(`state_shared: true` でも同じで、worktree ごとに動作)、smates 自身のエラー(`smates run` が
2/3/4 で終了する場合)はブロックせず終了コード 1 でユーザーに表示します。state を保存できなくても
テストが失敗していればブロックします。
エントリはファイルのテキストに差し込まれるので、それ以外(他のキーやフック、その順序や書式)は
1 バイトも変わらず、再インストールしても何も変わりません。`smates hook uninstall --claude` は
そのテキストだけを削除します(それによって空になる `Stop` や `hooks` も削除)。曖昧さなく編集できない
ファイル(不正な JSON、重複したキー、オブジェクトでない `hooks`、配列でない `Stop`)はエラーを報告して
変更しません。`smates hook status` で
状態を確認できます。`SMATES_SKIP_HOOK=1` でスキップでき、`smates` が未インストールの環境では
何もしません。

**終了コード**で「コードを直す」のか「環境・設定を直す」のかを区別できます。

| コード | 意味 |
|---|---|
| 0 | 成功 |
| 1 | テストが失敗した(ランナーの終了コードによらず 1。元のコードは `--report` の `exit_code`) |
| 2 | 使い方の誤り(コマンドラインが不正) |
| 3 | 設定エラー: `smates.yml` が不正、未知のランナー、解析できないスイートのコマンド、コマンドライン(`--suite`、`--only`)で指定したスイートやテストが設定にない、スイートが一つもない |
| 4 | 環境の問題: git がない、git リポジトリでない、スイートのコマンドを起動できない(`PATH` にない、実行権限がない)、`npx`/`bunx` がダウンロードしなければならないツール(`node_modules/.bin` にない)、`custom` 以外のランナーを記録しながら実行したとき、どのテストも結果を報告しないままランナーのプロセスが 126 または 127 で終了した(シェルの「実行できない」「コマンドが見つからない」。`npm test`、`yarn`、`pnpm exec`、`sh -c` 経由も含む)。`--no-record` や `custom` ランナーではテストが実行されても結果が報告されないことがあるため、テストの失敗(1)になります。失敗の出力とサマリを出してから 4 で終了します(別のスイートのテストが失敗していれば 1)。state を保存できない場合(別の worktree の実行が共有 state のロックを持っているなど)もサマリの後に 4 で終了します(テストが失敗していれば 1) |

## ライセンス

[MIT](LICENSE)

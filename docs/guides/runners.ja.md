# 言語とランナー { #languages-and-runners }

smates は、いま使っているテストランナーをそのまま使います。JavaScript と TypeScript では vitest、jest、`node --test`、bun、Python では pytest と unittest、そして `go test` です。このページでは、各ランナーがどうトレースされるか、何を設定する必要があるかを説明します。

## 対応ランナー { #supported-runners }

| ランナー | ユニット | 依存のトレース方法 | ファイルごとの結果 |
|---|---|---|---|
| `vitest` | テストファイル | ワーカーごとの V8 カバレッジ + fs フック。globalSetup での読み取りはすべてのテストの依存になる | 注入するレポーター |
| `jest` | テストファイル | ワーカーごとの V8 カバレッジ + fs フック。ワーカーはファイルごとに再起動（厳密にファイルごとに 1 プロセスにするには `isolate: true`） | 注入するレポーター |
| `node`（`node --test`） | テストファイル | テストプロセスごとの V8 カバレッジ + fs フック（`--test-isolation=none` でファイルごとに 1 プロセスにするには `isolate: true`） | プロセスの終了コード |
| `bun` | テストファイル | ファイルごとの `bun test --coverage` + モジュールキャッシュ、`node:fs`・`Bun.file`・`Bun.Glob` 用の preload フック（記録中） | プロセスの終了コード |
| `pytest` | テストファイル | `sys.monitoring`（3.12 以降）/ `sys.setprofile`、audit フック。テストごと、フィクスチャごと、収集したモジュールごと、およびモジュールレベルでの読み取り | プラグイン |
| `unittest`（`python -m unittest`） | テストモジュール | Python と同じトレース。選択したモジュールをラッパー経由で 1 プロセスで実行し、モジュールごとにコンテキストを分ける | ラッパーの結果クラス |
| `go` | パッケージ、または `granularity: func` でテスト関数 | 静的な import の閉包（Go では正確）+ `go test -exec` 経由の `-test.testlogfile`。テストの子プロセスの Node/Python | テストバイナリの終了コード、ビルドエラー |
| `custom` | テストファイル | コマンドが起動する Node/Python のプロセス | 全体の終了コード、または [JUnit レポート](configuration.md#per-file-results-from-a-junit-report-junit) |

*テストユニット*は smates が選択・スキップする単位です。トレースは smates が実行を*記録*しているときだけ行われます（`smates run` の既定。`--no-record` で無効にできます）。どのファイルがテストかは各ランナー自身の設定から読み取ります。[どのファイルがテストか](configuration.md#which-files-are-tests) を参照してください。

## JavaScript と TypeScript { #javascript-and-typescript }

smates は Node のプロセスに preload（`--require`）を注入します。これが各テストファイルの実行したコードの V8 カバレッジを集め、`fs` をフックして読み取り・一覧・存在確認したファイルを記録します。静的解析が import グラフを補うので、新しい import はすぐに反映されます（[import グラフが理解するもの](#what-the-import-graph-understands) を参照）。

ランナーのプロセス自身がテストファイルの外で実行するコードは、スイートのすべてのテストの依存になります。設定ファイル、グローバルセットアップとそれらが import するもの、独自のレポーターやリゾルバーです。どのテストにも属さず、リポジトリのコードを実行するヘルパープロセス（グローバルセットアップが起動したサーバーなど）が使うもの（そのコードと読み取るファイル）もすべて同じです。一方、ランナーのプロセスが読み取るだけのファイルは、グローバルセットアップでの読み取りを除いて依存になりません。ランナーは探索や変換のために、すべてのテストファイルとソースファイルを読むためです。

### vitest { #vitest }

vitest は既定で各ファイルを新しいワーカーで実行するので、ワーカーごとのカバレッジで正確に帰属できます。`globalSetup` での読み取りはすべてのテストの依存になります。プロジェクトが vitest の `isolate: false`（ワーカー共有）を設定している場合は、スイートに `isolate: true` を設定してください。記録中は smates が各ファイルを個別に実行します。

smates はファイルごとの結果を得るため、既存のレポーターの横に自分のレポーターを追加します。古い Vitest はプロジェクト内のレポーターしか読み込めないため、smates は実行の間だけレポーターを `node_modules/.cache/smates` にコピーし、終わったら削除します。`reporters` を自分で設定している場合や、smates が中身を見られない設定ファイル（`--config`、`-c`）やルート（`--root`、`--dir`）をコマンドが指定している場合、レポーターを置く `node_modules` ディレクトリがない場合は追加せず、実行が失敗すると記録したすべてのファイルが失敗扱いになります。[smates がコマンドに追加するもの](#what-smates-adds-to-the-command) も参照してください。

スイートのディレクトリにある Vitest と Vite の設定ファイル（`vitest.config.*`、`vite.config.*`、`vitest.e2e.config.ts` や `vite.storybook.config.ts` のようなプロジェクトごとのファイル、`vitest.workspace.*`）は、すべてのテストの入力です。設定が名前で読み込むファイル（`setupFiles`、`globalSetup`、ローカルの Vite プラグイン、エイリアスの参照先など）とその import 先は、テストされていないコードとして扱いません。テストが依存していない限り、変更は [オーファンのポリシー](orphans.md) に回ります。

### ブラウザモードと Storybook { #browser-mode-and-storybook }

ブラウザでテストを実行する vitest のプロジェクト（`test.browser`）や、Storybook の Vitest プラグイン（`@storybook/addon-vitest` の `storybookTest()`。以前の `@storybook/experimental-addon-test` も同じです）を使うプロジェクトも、他のプロジェクトと同じように読みます。違いは 2 点です。

- **ストーリーがテストになります。** プラグインは Storybook の設定ディレクトリからストーリーを読み、smates も同じものを読みます。設定ディレクトリは、プラグインの `configDir` オプション（リテラル、または `__dirname`・`import.meta.dirname`・`dirname` に対する `path.join` / `path.resolve`）です。指定がなければ `.storybook` です。smates は Storybook と同じく、リテラルとデフォルトを Vitest を実行するディレクトリ（スイートのルート）から、`path.join` の形はファイルのディレクトリから解決します。そうして解決したディレクトリが別の場所にあるプロジェクトファイルのものと異なる場合、そのストーリーは信頼しません（後述）。ストーリーは、その `main.*` の `stories`（文字列、ディレクトリ、`{ directory, files }` の指定。MDX のページはテストではありません）から読みます。Vitest の設定そのものが読めない場合（`mergeConfig(...)`）でも読めるので、ストーリーしかテストがないパッケージにもスイートができます。ストーリーが読めない場合は、プロジェクト配下のすべての `*.stories.*` ファイルを使います。これは推測なので、そのどれかが選択されたらスイート全体を実行します。`smates list` と `smates doctor` がそのことを伝えます。
- **ブラウザの中は何もトレースされません。** そうしたテストは実行のたびに結果が不明になり、次回も実行されます。スイートに `trust_browser_passes: true` を設定すると、代わりに pass を記録します。依存は smates が静的に分かるものです。テストの import クロージャ、Vitest の設定が読み込むもの（セットアップファイル、プラグイン、エイリアスの参照先）、ストーリーではさらに設定ディレクトリ、その `preview.*` と `main.*` の import クロージャ、`main.*` の `staticDirs` です。そのどれかが変わると、テストは再び選択されます。smates が解決できない import（Vite の設定でしか分からないエイリアス、仮想モジュール）を既知の依存に含むストーリーや、`main.*` が `viteFinal` や `previewAnnotations` を設定する、またはローカルのアドオンやプリセットを指定するストーリーは、信頼しません。smates が追えないファイルを読み込むことがあるためです。アドオンやフレームワークをパッケージとして扱うのは、パッケージ名を文字列で書くか、`getAbsolutePath(...)` や `require.resolve(...)` で解決する場合だけです。パス（`./preset`、`path.join(__dirname, "..", "preset")`、テンプレートリテラル）やそれ以外の式は、ローカルとして扱います。`...spread` を含む `{ name, options }` のオブジェクトも同じです。spread が名前を上書きしたり、パスを含むオプションを持ち込んだりすることがあるためです。信頼しないストーリーとその理由は、記録するときに表示します。頼る前に `smates run --shadow` で確かめてください。[制限事項](../reference/limitations.md#what-tracing-reaches) を参照してください。

スイートのコマンドが `--project <name>` で一部のプロジェクトを実行する場合、テストファイルはそのプロジェクトのものになります。ただし、smates が名前を読めないプロジェクト（リテラルの `test.name` がなく、Vitest がパッケージ名やディレクトリ名で名付けるもの）、ブラウザのプロジェクト、名前がパターンのときのすべてのプロジェクトは残します。コマンドが実行するプロジェクトのテストを取りこぼさないためです。

### jest { #jest }

jest のワーカーは 1 つで多数のファイルを実行するため、そのままでは依存が混ざります。記録中、smates はファイルごとにワーカーを再起動させます（Jest 30 以降では `--workerIdleMemoryLimit=0`、Jest 29 ではごく小さな上限）。それでも jest はまれに 2 つのファイルを 1 つのワーカーで続けて実行します。その場合は依存がまとめられますが、粗くなるだけで正しさは保たれます。

Jest 28 以前（または smates が `node_modules` から jest のバージョンを見つけられない場合）、`workerIdleMemoryLimit` を自分で設定している場合、テストを in band で実行する場合（`--runInBand`、`-i`、`jest.config.*` の `runInBand`）、smates が中身を見られない設定をコマンドが指定している場合（`--config`、`-c`、`--projects`、`--rootDir`）は、ワーカーを再起動させません。そのとき各テストファイルは、同じワーカーが実行したすべてのもの（in band では実行全体が使ったすべてのもの）に依存し、smates は依存がまとめられたことを警告します。

`isolate: true` にすると正確になります。記録中はファイルごとに jest のプロセスを 1 つ起動し、その分だけ起動コストがかかります。同時に動かす数の上限は `record_jobs` です（既定は CPU 数、`--runInBand` なら 1）。

smates は `testMatch` などのテストのパターンを jest の設定から読みますが、設定が指定する `preset` からは読みません。プリセットがそれらを設定している場合は、自分の設定に `testMatch` を書くか、スイートに `tests:` を設定してください。

### node --test { #node-test }

`node --test` は各ファイルを専用のプロセスで実行するので、終了コードはファイルごとです。node 自身の探索では、smates がテストとみなさないヘルパーまで実行されてしまうため、smates は常にファイルを明示的に渡します。コマンドが `--test-isolation=none`（全ファイルを 1 プロセス）を使っている場合は `isolate: true` を設定してください。

### bun { #bun }

記録中、smates はファイルごとに `bun test --coverage` を 1 回ずつ実行し、bun のカバレッジとモジュールキャッシュを読み取ります。`bunfig.toml` のコピーの先頭に入れた preload が、`node:fs`、`Bun.file`、`Bun.Glob` をフックします。記録しない実行では通常どおり 1 回の `bun test` を使います（その場合、ファイルごとの結果には JUnit レポートが必要です）。

### パッケージマネージャー、ワークスペース、パス { #package-managers-workspaces-and-paths }

- npm、pnpm、yarn、bun のいずれでも動きます。smates は `node_modules/.bin` のローカルなバイナリを直接実行し、なければロックファイルに応じて `pnpm exec`、`yarn`、`bunx`、`npx --no-install` を使います。
- smates は `npx` や `bunx` にランナーをダウンロードさせません。コマンドがレジストリからツールを取得しようとする場合、実行は止まり、先にインストールするよう案内します。
- npm/pnpm/yarn/bun のワークスペースと tsconfig の `paths` は静的解析が解決するので、モノレポのパッケージ間の import もたどれます（[import グラフが理解するもの](#what-the-import-graph-understands) を参照）。
- ロックファイル、マニフェスト、tsconfig は、すべてのテストの inputs になります。正確な一覧は [デフォルトの inputs](../reference/configuration.md#default-inputs) を参照してください。そこに含まれない tsconfig（入れ子のパッケージのものや、それらが extends するもの）も、その `paths` は import グラフの解決に使われます。
- vitest と bun のスイートでは、型のためだけの import（`import type`）は依存になりません。[制限事項](../reference/limitations.md#what-tracing-reaches) を参照してください。

### import グラフが理解するもの { #what-the-import-graph-understands }

静的解析は、TypeScript、Node、バンドラーと同じ規則で import を解決します。指定子が複数のファイルを指しうるときは、テストはそのすべてに依存します。

??? note "詳細: 静的解析が読み取るもの"
    - `.js`、`.mjs`、`.cjs`、`.jsx`、`.ts`、`.mts`、`.cts`、`.tsx`。`.vue`、`.svelte`、`.astro` のコンポーネントでは `<script>` ブロック、Astro のフロントマター、相対パスの `src` 属性
    - `import`、`export … from`、`import()`、`require()`、`require.resolve()`、`import.meta.resolve()`
    - 最も近い設定ディレクトリにある tsconfig と jsconfig の `paths` と `baseUrl`（`extends` もたどります）。対象は `tsconfig.json`、`jsconfig.json`、同じディレクトリの `tsconfig.*.json`（`tsconfig.app.json` など）と、それらが `references` に並べるプロジェクトです
    - package.json の `imports`（`#internal/x`）
    - 名前で参照するワークスペースのパッケージ。`exports`（すべての条件）、`main`、`module` をたどります。`package.json` のあるディレクトリはすべて対象になるので、ワークスペースの設定は読みません。エントリポイントが作業ツリーにない場合（ビルドしていない `dist/`）、その import はパッケージのすべてのソースファイルに依存します
    - ほかで解決できない `@/x` と `~/x` は最も近いパッケージの `src/x`（なければ `x`）、`$lib/x` は `src/lib/x` として扱います（ふつうバンドラーの設定で定義するエイリアスです）
    - `import.meta.glob(...)` のパターン、`require.context(dir)`、テンプレートリテラルの import（`` import(`./locales/${lang}.json`) `` は一致するすべてのファイルに依存します）、`new URL("./file", import.meta.url)`、`/// <reference path="...">`
    - `jest.mock`、`vi.mock` とその仲間（`doMock`、`requireActual` など）。モジュール自体と、隣の `__mocks__` ディレクトリにある手動モックです
    - bare な import（`lodash`、`@scope/pkg`）すべてについて、パッケージのルートとリポジトリのルートの `__mocks__/<name>`。Jest や Vitest が代わりに読み込むことがあります

    `node_modules` のパッケージは依存になりません（ロックファイルが代わりになります）。上記以外の、バンドラーの設定でしか定義されないエイリアスは解決しません。記録中はトレースが、それが読み込むファイルを捉えます。

### 記録のコスト { #the-cost-of-recording }

記録すると、Node の実行は smates なしの同じ実行より遅くなります。V8 は各プロセスが実行したすべてのスクリプトの正確なカバレッジを集め、プロセスが終わるたびに（vitest ではワーカーが止められる前にも）JSON として書き出します。`fs` のフックはすべてのファイルアクセスを見ており、smates は実行後にそれらのカバレッジファイルをすべて読みます。さらに jest はファイルごとにワーカーを再起動し、bun はファイルごとに 1 プロセスで実行します。どれだけ遅くなるかは、テストが読み込むコードの量と、起動するワーカープロセスの数によります。

`smates run --no-record` は、選択したテストをトレースせずに、ランナー本来の速さで実行します。選択は同じですが、状態は元のまま残ります。実行したテストは記録されない（新しい依存も結果も残らない）ので、次に記録する実行で再び選択されます。一度も記録されていないテストは new のままです。手元で素早く繰り返すときに使い、状態の更新は記録する実行（CI、または通常の `smates run`）に任せてください。トレースなしでもファイルごとの結果を報告できるランナー（vitest、jest、pytest、`go test`、および `junit:` レポートのあるスイート）では、どのファイルが失敗したかは引き続き報告されます。

## Python { #python }

smates はシムを `PYTHONPATH` に追加します。シムは `sys.monitoring`（Python 3.12 以降）または `sys.setprofile`（3.8〜3.11）でどの関数が実行されたかをトレースし、`open`、ディレクトリの一覧、`stat` は audit フックで捉えます。

### pytest か unittest か { #pytest-or-unittest }

Python のスイートは、pytest を示すものが 1 つでもあれば `pytest`、なければ `unittest` を使います。

- `pytest.ini` または `conftest.py`
- プロジェクトかその上のディレクトリにあるマニフェスト、ロックファイル、タスクファイル（`pyproject.toml`、`setup.cfg`、`setup.py`、`tox.ini`、`noxfile.py`、`Pipfile`、`poetry.lock`/`pdm.lock`/`uv.lock`、`*requirements*.txt`/`.in`、`requirements/` 以下のファイル）に pytest の名前がある
- pytest を実行する GitHub のワークフロー
- pytest 向けに書かれたテストファイル（`import pytest`、モジュールレベルの `def test_…` 関数）

### pytest { #pytest }

pytest は、実行されたものを各テスト・各フィクスチャ・収集した各モジュールに帰属させるプラグイン付きで実行します。モジュールが import 時に、あるいは `functools.cache` のようなキャッシュの裏で読み取ったデータはそのモジュールのものになり、そのモジュールを import するすべてのテストの依存になります。どのテストにも属さないところで起きたことは、そのプロセスのすべてのテストの依存になります。そこで読み取ったデータ（import 時の `conftest.py` による読み取り）、そこで起動したプロセス（セッションのフックが起動するサーバー）、どのテストも宣言していないフィクスチャ（`request.getfixturevalue` で要求するもの）です。pytest-xdist（`-n auto`）も動きます。プラグインはすべてのワーカーで読み込まれます。

### unittest { #unittest }

テストファイルは unittest の探索ルールに従います。既定ではスイートの root 以下の `test*.py`、または `python -m unittest discover …` コマンドの `-s`/`-p`/`-t`（開始ディレクトリ、パターン、トップレベルディレクトリ）です。

```yaml
  - name: legacy
    runner: unittest
    root: legacy
    command: "python3 -m unittest discover -s tests -p '*_check.py'"
```

smates は選択したファイルを、トップレベルディレクトリからのドット区切りのモジュール名として常に渡します。フル実行でも同じなので、`__init__.py` のないディレクトリが飛ばされることはありません。パスがモジュール名にならないファイルはテストではありません。

記録中、選択したモジュールはシムに同梱されたラッパー `python -m smates_unittest` を通して 1 プロセスで実行されます。ラッパーはモジュールごとのコンテキスト（import、`setUpModule`/`setUpClass`、テスト）を記録し、モジュールの import 中に発生した例外を、実行全体の終了ではなくそのモジュールの失敗として扱います。`isolate: true` にすると、代わりにモジュールごとに 1 プロセスで実行します。

??? note "詳細: テストを 1 つも実行しないモジュール"
    unittest のテストを 1 つも実行しないモジュールは、ソースを見て区別します。パターンに一致したヘルパーは成功扱いになり、`exclude` を勧める警告が出ます。pytest 向けに書かれたモジュールは成功として記録されることがなく、結果は不明のまま次回も実行され、`runner: pytest` を勧めるメッセージが出ます。どのモジュールもテストを実行しなかった実行が失敗する（unittest の終了コード 5）のは、その中に pytest 向けのモジュールがある場合だけです。

### import の解決 { #how-python-imports-are-resolved }

静的解析は、pytest や一般的な環境が `sys.path` に入れうるすべてのディレクトリに対してモジュール名を解決し、一致したものをすべて残します。これ以外の方法でコードを `sys.path` に入れる構成では、そのディレクトリをスイートの [`python_roots`](../reference/configuration.md#suite-keys) に追加してください。

??? note "詳細: import のルートと動的な import"
    ルートは次のとおりです。

    - リポジトリのルート、すべての Python のプロジェクトディレクトリ（`pyproject.toml`、`setup.py`、`setup.cfg`）とその `src/`、リポジトリの `src/`
    - pytest の `pythonpath`（`pyproject.toml`、`pytest.ini`、`tox.ini`、`setup.cfg`）と、setuptools（`packages.find.where`、`package-dir`）、poetry（`packages = [{ from = … }]`）、hatch（`packages`）のパッケージの場所
    - スイートの `python_roots`
    - import するファイルのトップレベルパッケージの親ディレクトリ。テストファイルではさらに、pytest がそのファイルのために読み込むすべての `conftest.py` のディレクトリ（pytest の既定の `prepend` import モード）
    - import するファイル自身のディレクトリ

    `from a import b` は、`a` とサブモジュール `a.b` の両方に依存します。`importlib.import_module`、`__import__`、`pytest.importorskip` は、名前がリテラルならたどります。リテラルの接頭辞だけが分かる場合（`import_module(f"app.plugins.{name}")`）は、その下のすべてのモジュールに依存します。

### インタプリタと virtualenv { #interpreters-and-virtualenvs }

既定のコマンドは、プロジェクトが uv を使っていれば（`uv.lock`、または `pyproject.toml` に `[tool.uv` のセクションがあれば）`uv run pytest`、そうでなければローカルの `.venv` か `venv` の Python、それもなければ `PATH` 上の `python3` です。smates はスイートのディレクトリとその上のディレクトリを探します。それ以外を使うには `command` を設定してください。

### Python のバージョン { #python-versions }

| バージョン | トレーサー | 記録中のオーバーヘッド |
|---|---|---|
| 3.12 以降 | `sys.monitoring` | 小さい |
| 3.8〜3.11 | `sys.setprofile` | はっきり遅くなる |

3.12 より前では、後から `sys.setprofile` で設定したプロファイラーが smates のトレーサーを置き換えてしまいます。

## Go { #go }

Go のユニットはパッケージです。依存は静的な import の閉包（Go では正確）に加え、テストバイナリが `go test -exec` 経由の `-test.testlogfile` で報告するもの（読み取ったファイル、一覧したディレクトリ、環境変数）です。テストが起動する Node と Python のプロセスもトレースします。ビルドエラーはそのパッケージに帰属させます。

バージョンを固定するファイル（`go.mod`、`go.sum`、`go.work`、`go.work.sum`）は、それを使ってビルドするすべてのパッケージの依存です。

??? note "詳細: 静的な閉包"
    静的な閉包に含まれるのは、パッケージ自身のファイル、それが import するすべてのパッケージのファイル（リポジトリの `go.mod` から、またはモジュールやワークスペースが依存を vendor している場合は `vendor/` から解決します）、`//go:embed` ディレクティブが指定するファイル、cgo のプリアンブルがパッケージ外の `-I${SRCDIR}/…` ディレクトリから include する C のヘッダーです。ここではビルド制約を評価しないので、すべてのプラットフォームとタグのファイルが含まれます。バージョンを固定するファイル（`go.mod`、`go.sum`、`go.work`、`go.work.sum`、vendor している場合は `vendor/modules.txt`）は、モジュールのすべてのパッケージの静的な依存です。スイートのディレクトリの `go.mod` と `go.sum`、リポジトリのルートの `go.work`、`go.work.sum`、`.tool-versions` は、さらにすべてのテストの inputs でもあります。

`go.work` のワークスペースでは、ワークスペースが使うどのモジュールで依存を上げても、ワークスペースのすべてのテストが選択されます。

??? note "詳細: `go.work` のワークスペース"
    Go はワークスペースが使うすべてのモジュールに対して、依存ごとに 1 つのバージョンを選びます。そのため、あるモジュールの `require` を上げると、そのモジュールを import しない別のモジュールのビルド結果も変わることがあります。そこで、ワークスペース内のすべての Go ファイルは、`use` に並ぶすべてのモジュールの `go.mod` と `go.sum`、およびローカルの `replace` 先の `go.mod` に依存します。smates が解析できない `go.work` では、その下にあるすべてのモジュールを対象にします。Go は実行したディレクトリに最も近い `go.work` を使うため、ファイルはその位置以上にあるすべての `go.work`（入れ子の `svc/go.work` とルートのもの）の固定ファイルに依存します。`GOWORK`（スイートの `env:` または smates の環境）が別の場所の `go.work` を指すときは、その固定ファイルをスイートのすべてのテストの inputs にします。`GOWORK=auto` は未設定と同じで、最も近い `go.work` が使われ、そこに並ぶ入れ子のモジュールはスイートに含まれたままです。

1 つのパッケージにほとんどのテストがある場合は、`granularity: func` を設定するとテスト関数単位で選択できます。[Go のテスト関数単位の選択](go-test-functions.md) を参照してください。

### テストのない Go パッケージ { #go-packages-without-tests }

既定では `_test.go` ファイルのあるディレクトリだけがユニットなので、選択的な実行はテストのないパッケージ（`cmd/` の下の `main` パッケージなど）をコンパイルしません。それが import するライブラリを変えると、ライブラリのテストは実行されますが `main` パッケージはビルドされません。また、`main` パッケージ自体の変更は [テストのないコード](orphans.md) として扱われます。`smates run --all` ではコンパイルされます（`go test ./...`）。選択的な実行でもこうしたパッケージをコンパイルするには、スイートの `tests` をそのファイルまで広げます。

```yaml
suites:
  - name: go
    runner: go
    tests: ["**/*_test.go", "**/cmd/**/*.go"]   # すべてのパッケージなら ["**/*.go"]
```

一致したディレクトリはそれぞれユニットになります。`go test ./cmd/app` がそれをコンパイルして `[no test files]` と報告するか、ビルドエラーで失敗させます。依存は他のパッケージと同じく静的な import の閉包です。smates は `[no test files]` を成功として記録します（`go test -json` でも同じです）。そのため、import するパッケージが変わったとき、または自分のファイルが変わったときに実行されます。`granularity: func` では、そのディレクトリは関数のユニットを持たないコンパイル検査（`cmd/app#(build)`）になります。

スイートのビルド制約で Go ファイルがすべて除外されるディレクトリはユニットになりません。テストファイルがすべて除外されるディレクトリはスキップとして記録され、次回も実行されます。

??? note "詳細: ビルド制約"
    スイートのビルド制約（`//go:build` 行、`_linux.go` のようなファイル名。タグは `GOFLAGS`、コマンド、そのまま渡す引数から）で Go ファイルがすべて除外されるディレクトリはユニットになりません。smates が判断できないときはユニットのままです。go list が失敗したとき、またはコマンドが `go test` そのものではない（`gotestsum -- -tags=...`、`make test`、`sh -c "..."`）か `--` の後ろに語を渡すときです。これらではタグが smates から見えないまま go test に届きます。

    そのようなディレクトリについて go test が「build constraints exclude all Go files」と報告しても、テストの失敗ではありません。ユニットはスキップとして記録されます（`granularity: func` ではそのテスト関数も）。実行が 0 で終了するのは、ほかに失敗がないと smates が判断できるときだけです。コマンドが go test 自身の終了コード 1 で終了し（`make` はレシピが失敗すると 2 で終了します）、go test のサマリー行（プレーンな出力、または `go test -json` のイベント）が smates に届いてほかのパッケージをすべて成功またはテストファイルなしと報告し、そのディレクトリの `[setup failed]` がどれもこの報告に続くときです。ラッパーが整形し直した出力（gotestsum 独自の行）や別の終了コードでは、実行は失敗のままです。タグを付けて go test をもう一度実行するラッパーはそのディレクトリをコンパイルし、そこで失敗したテストやビルドは実行を失敗させます。

    テストファイルが構成ですべて除外されるディレクトリ（`GOFLAGS` のタグ、別のプラットフォームでの `_linux_test.go` という名前）も `[no test files]` と報告しますが、成功ではなくスキップとして記録され、次の実行でもう一度実行されます。環境の `GOFLAGS`・`GOOS`・`GOARCH`・`CGO_ENABLED`、`go env -w`、ツールチェーン、プラットフォームは依存ではなく、それらのテストをコンパイルする構成では実行しなければならないためです。

コード生成の後でしかコンパイルできないパッケージは、他のビルドエラーと同じく失敗（exit 1）になり、環境エラーにはなりません。生成ファイルがないのか本当に壊れているのか、smates には区別できないためです。git が無視する生成ファイル（コードジェネレーターが作るファイル）があると、生成するまでローカルではこれらのユニットが失敗します。先にジェネレーターを実行するか、`exclude:` でそうしたパッケージを外してください。

## カスタムコマンド { #custom-commands }

`custom` ランナーは任意のコマンドを実行し、選択したテストファイルを末尾に追加するか `{tests}` の位置に入れます。`command` と `tests` が必要です。

```yaml
  - name: e2e
    runner: custom
    root: e2e
    tests: ["tests/**/*.spec.ts"]
    command: "npx playwright test"
```

記録中は、コマンドが起動する Node と Python のプロセスをトレースします。結果は全体の終了コードから決まります。ファイルごとに知りたい場合は [JUnit レポート](configuration.md#per-file-results-from-a-junit-report-junit) を指定してください。

## smates がコマンドに追加するもの { #what-smates-adds-to-the-command }

smates はテストファイルのほかに、その実行の間だけ、いくつかの引数をコマンドに追加します。

| ランナー | 追加するもの | 条件 |
|---|---|---|
| vitest | `--reporter=default`、`--reporter=<smates のレポーター>`、GitHub Actions では `--reporter=github-actions` も | レポーターを自分で設定しておらず、コマンドが設定やルート（`--config`、`-c`、`--root`、`--dir`）を指定しておらず、レポーターを置く `node_modules` ディレクトリがある場合（[vitest](#vitest) を参照） |
| jest | `--reporters=default --reporters=<smates のレポーター>` | `reporters` を自分で設定しておらず、コマンドが設定（`--config`、`-c`、`--projects`、`--rootDir`）を指定していない場合 |
| jest | `--workerIdleMemoryLimit=0`（Jest 30 以降）、`=1KB`（Jest 29） | 記録中。[jest](#jest) を参照 |
| jest | `--runTestsByPath` | すべてを実行するのではない場合。選択したファイルをパターンではなくパスとして扱わせるためです |
| bun | `--config=<bunfig.toml のコピー>`（smates の preload を先頭に入れ、カバレッジのしきい値を外したもの）、`--coverage --coverage-reporter=lcov --coverage-dir=…` | 記録中。ファイルごとに 1 回 |
| pytest | `--continue-on-collection-errors` | コマンドに `-x`/`--exitfirst` がない場合。1 つのモジュールの import に失敗しても、ほかのファイルが止まらないようにするためです |
| go（パッケージ単位。`granularity: func` は [Go のテスト関数単位の選択](go-test-functions.md) を参照） | `GOFLAGS` に `-exec=<smates のラッパー> -count=1`、およびテストのある各パッケージにテストファイルを 1 つ追加する `-overlay=<file>`（自分で指定した `-overlay` はそこにマージします） | `-exec` と `-overlay` は記録中（overlay は、テストバイナリを再実行した子プロセスにも専用のテストログを書かせるためです）。`-count=1` は記録中と `--shadow` のとき。キャッシュした結果を使わせないためです |

`node --test`、unittest、custom のスイートには、テストファイルだけを渡します（記録する unittest の実行は `python -m smates_unittest` を通ります。[unittest](#unittest) を参照）。すべてを実行するのではない場合、pytest の終了コード 5（テストが収集されなかった）は失敗になりません。

vitest や jest の実行が、どのテストファイルも実行する前に、smates のレポーターも読み込まないまま失敗した場合（たとえばランナーが追加した引数を受け付けない場合）は、`--runTestsByPath` 以外の smates が追加した引数を外して、もう一度実行します。そのときファイルごとの結果は全体の終了コードから決まり、smates はそのことを表示します。

## 子プロセス { #child-processes }

テストが起動する他のプロセスも、どのランナーでも追跡します。

- Node、Bun、Python の子プロセスは、どのスイートでも（pytest のテストが `node` を実行する場合や、Node のテストが `python3`、`pypy3`、`bun` を実行する場合も）、シェル経由（`sh -c "node x | tr"`）や `npm`、`npx`、`pnpm`、`yarn` 経由で起動された場合も自分自身をトレースし、起動したテストに帰属します。
- ランチャー（`uv run`、`uvx`、`bunx`）と `find -exec` のコマンドは、それが起動するコマンドとして解析します。
- シェルスクリプト、`grep`、`cat` などの読み取り専用のコマンドは、コマンドラインとスクリプトの内容から解析します。テストは、それらが指定したファイルに依存します。引数付きで呼ばれたスクリプトは位置パラメータ（`$1`、`"$@"`）も追跡します。
- smates が中を見られないコマンド（`make`、コンパイル済みのバイナリ）は、それが実行されたディレクトリ以下のすべて（`dir/**`）を依存にします。パスで指定されたプログラムならプログラム自体も、オプションの値（`--config=cfg/app.ini`、`-Dk=v`）やレスポンスファイル（`@args.txt`）で指定されたファイルも依存になります。
- `-I`、`-E`、`-S` 付きで起動された Python の子プロセスや、smates の環境変数を落とした環境で起動された Node、Bun、Python の子プロセスは自分自身をトレースできないため、中を見られないコマンドと同じに扱います。
- Go のテストの子プロセスについては [Go のテストの子プロセス](#child-processes-of-go-tests) を参照してください。

### Go のテストの子プロセス { #child-processes-of-go-tests }

Go にはテストが起動したプロセスにフックする仕組みがないため、smates は Go のテストログを通じて子プロセスを追跡します。

- テストバイナリとして再実行された子プロセス（ヘルパープロセスのパターン、`exec.Command(os.Args[0], "-test.run=^TestHelperProcess$")`）は自分のテストログを書き、それがテストの依存に統合されます。`os.Exit` で終わる、panic する、kill されるといった場合はログが失われ、テストはその Go モジュールのすべてのファイルに依存します。精度を保つには、ヘルパーで exit せずに return してください。
- それ以外のプログラム（`sh`、`git`、コンパイル済みのツール、CLI にディスパッチする `TestMain` が独自の引数で起動したバイナリ）は、それが実行されたディレクトリと `cmd.Dir` のツリー、およびリポジトリ内にあればプログラムのファイルを依存にします。
- Node、Bun、Python の子プロセスは、ほかと同じく自分自身をトレースします。

こうしたプログラムがそのディレクトリの外にあるパスを指定した場合（`sh -c "cat ../../testdata/x"`）は見えません。[制限事項](../reference/limitations.md) を参照してください。

テストがネットワーク越しにアクセスするサーバーは子プロセスではありません。そのコードは `depends_on` で宣言してください（[サーバーを叩くテスト](servers.md) を参照）。

## カバレッジツールとの併用 { #works-with-coverage-tools }

カバレッジの収集はスイートの `command` にそのまま残してください。smates はその上で記録し、レポートは smates がない場合と同じになります。

| ツール | 両立のしかた |
|---|---|
| `vitest --coverage`（v8 プロバイダー）、jest の `--coverage --coverageProvider=v8` | `node:inspector` 経由で V8 のカバレッジを取得して停止するため、smates の `NODE_V8_COVERAGE` のファイルが空になります。smates は代わりに、それらが取得したカバレッジに含まれるスクリプトを記録します。 |
| `c8` | `NODE_V8_COVERAGE` が設定されていると c8 はそれを一時ディレクトリとして使うので、smates のファイルを読んでレポートを作ります（`--reports-dir` の出力は通常どおりですが、`coverage/tmp` は作られません）。ランナーの呼び出しごとに専用のディレクトリを使うので、c8 の `--clean` が他の呼び出しのファイルを消すことはありません。 |
| `node --test --experimental-test-coverage` | そのまま動きます。smates の preload は `node_modules` ディレクトリにあり、カバレッジレポートからは除外されます。 |
| 自分で設定した `NODE_V8_COVERAGE`（環境変数またはスイートの `env`） | 記録中は置き換え、実行後に生のカバレッジファイルをそのディレクトリにコピーします（既存のファイルを上書き・削除することはありません）。 |
| pytest-cov / coverage.py | すべてのコアに対応します: `sys.settrace`（ctrace、pytrace）と `sys.monitoring`（`COVERAGE_CORE=sysmon`）。smates は専用の `sys.monitoring` のツール ID（3、4、2、5 のいずれか。coverage.py は 1 を使い、埋まっていれば次を使う）を取り、3.12 より前では既に設定されたプロファイル関数に処理をつなぎます。テスト後に pytest-cov がレポートのために読むファイルは、どのテストの依存にもなりません。 |
| pytest-xdist（`-n auto`） | プラグインがすべてのワーカーで読み込まれ、依存と結果は xdist なしと同じくテストファイルごとになります。 |
| `go test -cover`、`-coverpkg`、`-coverprofile`、`-covermode` | smates の `-exec` ラッパーの影響を受けず、プロファイルは通常どおり書き出されます。`-coverpkg`（コマンド、`--` の後、`GOFLAGS` のいずれか）を使うと、go は指定したパッケージをすべてのテストバイナリに組み込むため、すべてのテストパッケージがそれらのソースファイルにも依存します（そこでのコンパイルエラーはすべてを失敗させます）。 |

!!! warning
    `NODE_V8_COVERAGE` を自分で設定するコマンド（`env NODE_V8_COVERAGE=… node` や、それを export するスクリプト）を使うと、その配下のプロセスが実行したコードが smates から見えなくなります。環境変数かスイートの `env` で設定してください。また、smates の記録中は c8 が `coverage/tmp` を書き出さないため、後から `c8 report` を実行するのではなく、同じコマンドの中で c8 にレポートさせてください。

## 関連項目 { #see-also }

- [スイートの設定](configuration.md): `command`、`tests`、`isolate`、`junit`
- [Go のテスト関数単位の選択](go-test-functions.md): `granularity: func`
- [環境変数](environment.md): 言語ごとに何が読み取りとして数えられるか
- [制限事項](../reference/limitations.md)

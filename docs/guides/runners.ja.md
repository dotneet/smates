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

smates は Node のプロセスに preload（`--require`）を注入します。これが各テストファイルの実行したコードの V8 カバレッジを集め、`fs` をフックして読み取り・一覧・存在確認したファイルを記録します。静的解析が import グラフを補うので、新しい import はすぐに反映されます。

### vitest { #vitest }

vitest は既定で各ファイルを新しいワーカーで実行するので、ワーカーごとのカバレッジで正確に帰属できます。`globalSetup` での読み取りはすべてのテストの依存になります。プロジェクトが vitest の `isolate: false`（ワーカー共有）を設定している場合は、スイートに `isolate: true` を設定してください。記録中は smates が各ファイルを個別に実行します。

smates はファイルごとの結果を得るため、既存のレポーターの横に自分のレポーターを追加します。`reporters` を自分で設定している場合は追加せず、実行が失敗すると記録したすべてのファイルが失敗扱いになります。

### jest { #jest }

jest のワーカーは 1 つで多数のファイルを実行するため、そのままでは依存が混ざります。記録中、smates はファイルごとにワーカーを再起動させます（Jest 30 では `--workerIdleMemoryLimit=0`、Jest 29 ではごく小さな上限）。それでも jest はまれに 2 つのファイルを 1 つのワーカーで続けて実行します。その場合は依存がまとめられますが、粗くなるだけで正しさは保たれます。

`isolate: true` にすると正確になります。記録中はファイルごとに jest のプロセスを 1 つ起動し、その分だけ起動コストがかかります。同時に動かす数の上限は `record_jobs` です（既定は CPU 数、`--runInBand` なら 1）。

### node --test { #node-test }

`node --test` は各ファイルを専用のプロセスで実行するので、終了コードはファイルごとです。node 自身の探索では、smates がテストとみなさないヘルパーまで実行されてしまうため、smates は常にファイルを明示的に渡します。コマンドが `--test-isolation=none`（全ファイルを 1 プロセス）を使っている場合は `isolate: true` を設定してください。

### bun { #bun }

記録中、smates はファイルごとに `bun test --coverage` を 1 回ずつ実行し、bun のカバレッジとモジュールキャッシュを読み取ります。`bunfig.toml` のコピーの先頭に入れた preload が、`node:fs`、`Bun.file`、`Bun.Glob` をフックします。記録しない実行では通常どおり 1 回の `bun test` を使います（その場合、ファイルごとの結果には JUnit レポートが必要です）。

### パッケージマネージャー、ワークスペース、パス { #package-managers-workspaces-and-paths }

- npm、pnpm、yarn、bun のいずれでも動きます。smates は `node_modules/.bin` のローカルなバイナリを直接実行し、なければロックファイルに応じて `pnpm exec`、`yarn`、`bunx`、`npx --no-install` を使います。
- smates は `npx` や `bunx` にランナーをダウンロードさせません。コマンドがレジストリからツールを取得しようとする場合、実行は止まり、先にインストールするよう案内します。
- npm/pnpm/yarn/bun のワークスペースと tsconfig の `paths` は静的解析が解決するので、モノレポのパッケージ間の import もたどれます。
- スイートのディレクトリとその上のすべてのディレクトリにあるロックファイル、`package.json`、`tsconfig*.json`、`.npmrc`、`.nvmrc` などは、すべてのテストの inputs になります。

## Python { #python }

smates はシムを `PYTHONPATH` に追加します。シムは `sys.monitoring`（Python 3.12 以降）または `sys.setprofile`（3.8〜3.11）でどの関数が実行されたかをトレースし、`open`、ディレクトリの一覧、`stat` は audit フックで捉えます。

### pytest か unittest か { #pytest-or-unittest }

Python のスイートは、pytest を示すものが 1 つでもあれば `pytest`、なければ `unittest` を使います。

- `pytest.ini` または `conftest.py`
- プロジェクトかその上のディレクトリにあるマニフェスト、ロックファイル、タスクファイル（`pyproject.toml`、`setup.cfg`、`setup.py`、`tox.ini`、`noxfile.py`、`Pipfile`、`poetry.lock`/`pdm.lock`/`uv.lock`、`*requirements*.txt`/`.in`、`requirements/` 以下のファイル）に pytest の名前がある
- pytest を実行する GitHub のワークフロー
- pytest 向けに書かれたテストファイル（`import pytest`、モジュールレベルの `def test_…` 関数）

### pytest { #pytest }

pytest は、実行されたものを各テスト・各フィクスチャ・収集した各モジュールに帰属させるプラグイン付きで実行します。モジュールが import 時に、あるいは `functools.cache` のようなキャッシュの裏で読み取ったデータはそのモジュールのものになり、そのモジュールを import するすべてのテストの依存になります。pytest-xdist（`-n auto`）も動きます。プラグインはすべてのワーカーで読み込まれます。

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

### インタプリタと virtualenv { #interpreters-and-virtualenvs }

既定のコマンドは、プロジェクトが uv を使っていれば（`uv.lock`、または `pyproject.toml` に `[tool.uv` のセクションがあれば）`uv run pytest`、そうでなければローカルの `.venv` か `venv` の Python、それもなければ `PATH` 上の `python3` です。smates はスイートのディレクトリとその上のディレクトリを探します。それ以外を使うには `command` を設定してください。

### Python のバージョン { #python-versions }

| バージョン | トレーサー | 記録中のオーバーヘッド |
|---|---|---|
| 3.12 以降 | `sys.monitoring` | 小さい |
| 3.8〜3.11 | `sys.setprofile` | はっきり遅くなる |

3.12 より前では、後から `sys.setprofile` で設定したプロファイラーが smates のトレーサーを置き換えてしまいます。

## Go { #go }

Go のユニットはパッケージです。依存は静的な import の閉包（Go では正確）に加え、テストバイナリが `go test -exec` 経由の `-test.testlogfile` で報告するもの（読み取ったファイル、一覧したディレクトリ、環境変数）です。テストが起動する Node と Python のプロセスもトレースします。ビルドエラーはそのパッケージに帰属させます。`go.mod`、`go.sum`、`go.work`、`.tool-versions` は inputs です。

1 つのパッケージにほとんどのテストがある場合は、`granularity: func` を設定するとテスト関数単位で選択できます。[Go のテスト関数単位の選択](go-test-functions.md) を参照してください。

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

## 子プロセス { #child-processes }

テストが起動する他のプロセスも、どのランナーでも追跡します。

- Node と Python の子プロセスは、シェル経由（`sh -c "node x | tr"`）で起動された場合も自分自身をトレースし、起動したテストに帰属します。
- シェルスクリプト、`grep`、`cat` などの読み取り専用のコマンドは、コマンドラインとスクリプトの内容から解析します。テストは、それらが指定したファイルに依存します。
- smates が中を見られないコマンド（`make`、コンパイル済みのバイナリ）は、それが実行されたディレクトリ以下のすべて（`dir/**`）を依存にします。パスで指定されたプログラムなら、そのプログラム自体も依存になります。

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

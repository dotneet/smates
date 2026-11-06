# スイートの設定 { #configure-suites }

`smates.yml` には、リポジトリにどんなテストスイートがあり、どう実行し、トレースでは見えない何に依存しているかを書きます。このページではよく使う設定を順に説明します。すべてのキーは [smates.yml リファレンス](../reference/configuration.md) にあります。

```yaml
version: 1
suites:
  - name: web
    runner: vitest
    root: packages/web
  - name: api
    runner: go
    root: services/api
ignore: ["design/**"]
```

*スイート*は、あるディレクトリを起点にしたテストランナーの 1 回の呼び出しです。*テストユニット*はその中で smates が選択・スキップする単位で、テストファイル、または Go のパッケージ（あるいはテスト関数）です。

## smates.yml が必要なとき { #when-you-need-smatesyml }

必要ないかもしれません。設定ファイルがなければ、smates はリポジトリの構成（package.json のスクリプトやランナーの設定ファイル、`go.mod`、pytest や unittest のファイル）からスイートを検出し、各ランナーの既定値を使います。検出結果は次のコマンドで確認できます。

```bash
smates list
```

```text
web	runner=vitest	root=packages/web	units=42	command=./node_modules/.bin/vitest run	tests=vitest.config.ts test.include
api	runner=go	root=services/api	units=9	command=go test	tests=runner defaults
```

検出が何かを見落としたりまとめすぎたりするとき、ランナーに特定のコマンドが必要なとき、トレースでは見えないものにテストが依存しているときに `smates.yml` を書きます。`smates init` を使うと検出結果から書き出せるので、そこから始めるのが簡単です。smates はリポジトリのルートで `smates.yml`、`smates.yaml`、`.smates.yml`、`.smates.yaml` を探します。

## スイートを定義する { #define-a-suite }

`suites:` の各エントリに必須なのは `runner` だけです。

| キー | 意味 | 既定値 |
|---|---|---|
| `runner` | `vitest`、`jest`、`node`、`bun`、`pytest`、`unittest`、`go`、`custom` のいずれか | 必須 |
| `root` | コマンドを実行するディレクトリ（リポジトリルートからの相対パス） | `.` |
| `name` | 出力、`--suite`、`--only suite:unit`、`depends_on` で使う名前 | root（root が `.` ならランナー名） |

名前は一意である必要があります。各ランナーが何をトレースし、結果をどう報告するかは [言語とランナー](runners.md) を参照してください。

## テストファイルを選ぶ { #choose-the-test-files }

既定では、smates はランナー自身の設定からテストファイルを読み取ります。そのため、ランナーが実行するのと同じファイルを数えます。上書きするには `tests` と `exclude` を使います。どちらもスイートの root からの相対 glob です。

```yaml
  - name: web
    runner: vitest
    root: packages/web
    tests: ["src/**/*.test.ts"]
    exclude: ["src/legacy/**"]
```

!!! note
    `custom` ランナーには既定値がありません。`custom` スイートには `tests:` が必要で、ないとテストユニットが 0 件になります。

### どのファイルがテストか { #which-files-are-tests }

`tests:` がない場合、smates はランナーの設定を静的に読み取ります。設定ファイルを実行することはありません。

| ランナー | 読み取る場所 | 対応するオプション |
|---|---|---|
| `jest` | コマンドの `--config`。なければスイートの root から上へ向かって最も近い `jest.config.{js,ts,mjs,cjs,mts,cts,json}` または `package.json` の `"jest"`（jest と同じく最も近い `package.json` で止まる） | `rootDir`、`roots`、`testMatch`（`?(x)` や `+(spec\|test)` などの micromatch の extglob は変換し、`!` パターンは除外として扱う）、`testRegex`、`testPathIgnorePatterns`（`<rootDir>` は置換） |
| `vitest` | コマンドの `--config`。なければスイートの root から上へ向かって最も近い `vitest.config.*` / `vite.config.*` | `root`、`test.root`、`test.dir`、`test.include`、`test.exclude`（`...configDefaults.exclude` も解釈）、プロジェクト（`test.projects`、`test.workspace`、`vitest.workspace.*`。コマンドの `--project` で絞り込み）、Storybook の Vitest プラグインのストーリー（[ブラウザモードと Storybook](runners.md#browser-mode-and-storybook) を参照） |
| `pytest` | スイートの root から上へ向かって最初に見つかる `pytest.ini`、`.pytest.ini`、`pyproject.toml`（`[tool.pytest.ini_options]` / `[tool.pytest]`）、`tox.ini`（`[pytest]`）、`setup.cfg`（`[tool:pytest]`） | `python_files`、`norecursedirs`。`testpaths` はそのファイルがスイートの root にあるときだけ（pytest も rootdir で起動したときだけ使うため） |
| `unittest` | `python -m unittest discover …` コマンドの `-s`/`-p`/`-t` | 既定はスイートの root 以下の `test*.py` |
| `go` | `testdata/` と `vendor/` を除く `**/*_test.go` | スイートの中に入れ子になったモジュールは、`go.work` のワークスペースに含まれるときだけそのスイートに属する |
| `node` | node の既定パターン（`*.test.*`、`*-test.*`、`*_test.*`、`test-*.*`、`test.*`、`test/` 以下のファイル） | `test/` 以下のヘルパーとフィクスチャは除外 |
| `bun` | bun の既定パターン（`*.test.*`、`*.spec.*`、`*_test.*`、`*_spec.*`） | |

JS/TS の設定ファイルを解釈できるのは、オブジェクトリテラルを export していて（`module.exports = {...}`、`export default {...}`、`defineConfig({...})`、またはトップレベルの `const` を名前で export）、オプションが文字列・配列・正規表現のリテラルの場合です。

それ以外はランナーの既定の glob にフォールバックし、`smates list` と `smates doctor` が理由とともに警告します。たとえば関数、`mergeConfig(base, ...)` のような別の呼び出し、`testMatch: dirs.map(...)`、後から変更される `const`（`config.roots.push(...)`）、オプションを設定しうる `...spread`、jest の `projects`、先読み・後読みを含む `testRegex` などです。その場合は `tests:`（と `exclude:`）を設定してください。

??? note "詳細: vitest のワークスペース、フル実行、`smates list -v`"
    - vitest のワークスペースは 1 つのスイートになり、テストファイルは各プロジェクトの和集合です。各プロジェクトの include/exclude はそのプロジェクトの root 以下に適用されます。設定を読み取れないプロジェクトは、そのディレクトリに既定値を使います。
    - ルートの vitest 設定が入れ子のパッケージのテストを含まない場合（そのパッケージに独自の `vitest.config.*` があり、どの `projects` にも含まれていない場合）、自動検出はそのパッケージを別のスイートにします。
    - `smates list` はパターンの出どころを表示します（`tests=jest.config.js testMatch`）。`-v` を付けるとパターンも表示します。`smates doctor` でも同じ情報が出ます。
    - ランナーの設定から導いたパターンは、ランナーが自分で見つけるファイルを表すものです。そのため、フル実行（`smates run --all`）ではランナー自身にテストを探させます。`smates.yml` に `tests:` や `exclude:` を書いた場合は、フル実行でも smates が常にファイルを明示的に渡します。

## コマンドを設定する { #set-the-command }

各ランナーには既定のコマンドがあります（`./node_modules/.bin/vitest run`、`go test`、`uv run pytest` など）。プロジェクトで別の方法でテストを実行している場合は `command` を設定します。

```yaml
  - name: web
    runner: vitest
    root: packages/web
    command: "pnpm exec vitest run --project unit"
```

smates は選択したテストファイルをコマンドの末尾に追加します。別の位置に置きたい場合は、そこに `{tests}` と書きます。

```yaml
    command: "pnpm exec vitest run {tests} --reporter=dot"
```

`smates run -- <args>` で渡した引数はテストファイルの直前に入ります。こうした実行ではテストが絞り込まれている可能性があるため、テストが成功として記録されることはありません。常に使うフラグは `command` に書いてください。`custom` ランナーでは `command` が必須です。

## トレースでは見えない依存を宣言する { #declare-what-tracing-cannot-see }

トレースは、テストが実行したファイルと読み取ったファイルを記録します。観測できない依存を追加する設定が 2 つあります。

```yaml
  - name: api
    runner: pytest
    root: services/api
    inputs: ["/proto/**", "schema.graphql"]
    rules:
      - tests: ["tests/db/**"]
        deps: ["migrations/**"]
```

- `inputs`: スイートのすべてのテストが依存する glob です。スイートの root からの相対パスで、先頭に `/` を付けるとリポジトリルートからの相対パスになります。ロックファイル、マニフェスト、ランナー自身の設定ファイルは既定で inputs に入っています。
- `rules`: 一部のテストだけの依存です。`tests` はスイートの root からの相対 glob、`deps` はリポジトリルートからの相対 glob です。`reaches` を使うと、置き場所ではなく import するものでテストを選べます（次の節）。

ネットワーク越しにテストがアクセスするサーバーは別のケースです。そのコードは `depends_on` か `serves` で宣言します（[サーバーを叩くテスト](servers.md) を参照）。

### データベースを使うテスト { #tests-backed-by-a-database }

データベースのスキーマは、たいていテストの前に別のステップ（`make migrate` や CI のジョブ）でマイグレーションツールが適用します。テストデータも同じように投入されることがあります。テストのプロセスはそれらのファイルを読まないので、マイグレーションの変更は [オーファン](orphans.md) になります。エージェントが判断するか、スイート全体が実行されます。パスの glob で「データベースを使うテスト」を表すのは難しく、使わないテストと同じ場所に並んでいます。共通しているのは接続に使うヘルパーを import していることなので、そのヘルパーを `reaches` で指定します。

```yaml
  - name: go
    runner: go
    rules:
      - reaches: ["pkg/shared/dbtest/**", "$APP_DB_*"]
        deps: ["database/migrations/**", "database/fixtures/**"]
```

静的 import の閉包が `pkg/shared/dbtest/` に届くテスト（直接、別のヘルパー経由、または同じ Go パッケージのほかのテストファイルから）と、前回記録した実行で `APP_DB_*` に一致する変数を読んだテストが、マイグレーションとフィクスチャを依存として得ます。するとマイグレーションの変更はそれらのテストを選び、オーファンではなくなります。

リストを漏れなく書く必要があるのはこのためです。別の方法で接続するテスト（自前の `sql.Open`、ルールが挙げていないヘルパー、子プロセス）は deps を得ず、マイグレーションがカバーされた後はその変更でも実行されません。ヘルパーをすべて挙げ、接続が読む変数も加えてください。その変数を読むテストは、glob に一致するものを何も import していなくても残ります（Go が記録しない `TestMain` や `init` での読み取りでも、ソースに変数名があれば十分です）。ランナーがすべてのテストのために読み込むセットアップファイル（Vitest の `setupFiles`）は、すべてのテストが import するものとして扱います。smates が判断できないときはルールを適用します。静的解析が無効なとき、テストのファイルを読むアナライザがないとき、smates が解決できないもの（バンドラのエイリアス）をテストが import するとき、そして `$NAME` の項目でテストがまだ一度も実行されていないとき、または変数が `env_ignore` にあるときです。`smates doctor` はこうしたルールごとに適用されるテストユニットの数を表示し、1 つにも適用されないルールや、どのファイルにも一致しない glob（誤記、ディレクトリではなく import パス）を警告します。

## 環境変数を設定する { #set-environment-variables }

`env` はスイートのコマンドに渡す変数を設定します。

```yaml
    env: { TZ: UTC, APP_MODE: test }
```

値はスイートの設定の一部なので、変更するとそのスイートが再実行されます。また、テストが読み取った変数を比較するときには、シェルの値よりこちらが優先されます。[環境変数](environment.md) を参照してください。

## 記録中はファイルごとに 1 プロセス: `isolate` { #one-process-per-file-while-recording-isolate }

ランナーによっては、複数のテストファイルでワーカープロセスを共有し、その結果ファイル同士の依存が混ざります。`isolate: true` を設定すると、記録中は各ファイルを専用のプロセスで実行します（記録しない実行は変わりません）。

| ランナー | `isolate: true` を設定する場面 |
|---|---|
| `jest` | 厳密にファイルごとに 1 プロセスにしたいとき（smates は既定でもファイルごとにワーカーを再起動させます） |
| `vitest` | プロジェクトが vitest の `isolate: false` を設定しているとき |
| `node` | コマンドが `--test-isolation=none` を使っているとき |
| `unittest` | 全モジュールを 1 プロセスで実行する代わりに、モジュールごとに別プロセスで実行したいとき |

`bun` は記録中、常にファイルごとに 1 プロセスで実行します。`record_jobs` は、これらのプロセスを同時にいくつ動かすかの上限です（既定値は bun と、`--runInBand` のようにファイルを直列に実行するコマンドでは 1、それ以外は CPU 数）。Go のスイートでは `record_jobs` の意味が異なります。[Go のテスト関数単位の選択](go-test-functions.md#what-recording-costs) を参照してください。

## JUnit レポートからファイルごとの結果を得る: `junit` { #per-file-results-from-a-junit-report-junit }

`custom` ランナーが知っているのはコマンドの終了コードだけです。そのコマンドが多数のファイルを実行すると、1 つの失敗ですべてが失敗扱いになり、次回すべて再実行されます。コマンドが JUnit XML レポートを書き出すなら、`junit` にそのパス（スイートの root からの相対パス）を指定してください。smates はそこからファイルごとの結果を読み取ります。

```yaml
  - name: e2e
    runner: custom
    root: e2e
    tests: ["tests/**/*.spec.ts"]
    command: "npx playwright test --reporter=junit"
    env: { PLAYWRIGHT_JUNIT_OUTPUT_NAME: reports/junit.xml }
    junit: reports/junit.xml
```

ファイルは、テストケースまたはテストスイートの `file` 属性から取ります。それがなければ、既存のファイルを指していればスイート名かクラス名を使います。

## 1 つのリポジトリに複数のスイート { #several-suites-in-one-repository }

ランナーの呼び出しごとに 1 つのスイートを書きます。モノレポでは、たいていパッケージやサービスごとに 1 つです。

```yaml
version: 1
suites:
  - name: web
    runner: vitest
    root: packages/web
  - name: shared
    runner: jest
    root: packages/shared
  - name: api
    runner: go
    root: services/api
  - name: tools
    runner: pytest
    root: tools
```

静的解析は npm/pnpm/yarn/bun のワークスペースや tsconfig の `paths` も含めてスイートをまたいで import をたどります。そのため、`packages/shared` を変更すると、それを import している `web` のテストが選択されます。`tests` の glob が異なれば、2 つのスイートが同じ root を共有しても構いません（1 つのパッケージのユニットテストと e2e テストなど）。1 つのスイートだけを実行するには `smates run --suite web` とします。

## リポジトリ全体の設定 { #repository-wide-settings }

次のトップレベルのキーはすべてのスイートに適用されます。

```yaml
ignore: ["design/**", "deploy/**"]
inputs: [".github/workflows/test.yml"]
full_run_branches: [main, release]
```

- `ignore`: *オーファン*（どのテストにも紐づかない変更。[どのテストにも紐づかない変更](orphans.md) を参照）として扱わない変更ファイルです。ドキュメント、ライセンス、エディタ設定、lint やフォーマッタの設定といった組み込みの既定値に追加されます。ignore したファイルでも、それを読むテストはそのファイルに依存したままです。
- `inputs`: すべてのスイートのすべてのテストが依存する、リポジトリルートからの相対 glob です。CI のワークフローやツールチェーンのバージョン指定などに使います。
- `full_run_branches`: CI 上で `smates run` がすべてを実行するブランチです（プルリクエストではなく push のビルド。既定はリポジトリのデフォルトブランチ）。そこでのフル実行によって、記録されたステートが完全に保たれます。手元では、`smates run` は常に選択します。

その他のトップレベルのキー（`orphans`、`untested`、`env_ignore`、`env_track`、`state*`、`flaky`、`agent`）はそれぞれのページと [リファレンス](../reference/configuration.md) で説明しています。

## どの編集でテストが再実行されるか { #which-edits-rerun-tests }

どのテストも、`smates.yml` にある自分のスイートの設定に依存しています。`--explain` では `[changed: smates.yml]` と表示されます。対象になるのはファイル全体ではなく、書かれたとおりのそのスイートのエントリと、トップレベルの `env_track` です。

| 編集した箇所 | 結果 |
|---|---|
| スイートのエントリ（`command`、`env`、`tests`、`isolate` など） | そのスイートのすべてのテストが再実行される |
| トップレベルの `env_track` | すべてのテストが再実行される |
| 別のスイートのエントリ | このスイートには影響しない |
| `ignore`、`orphans`、`untested`、`agent`、`flaky`、トップレベルの `env_ignore` | 何も実行されない |
| スイートの `volatile` や `max_age` | それらが選ぶユニットだけ（テストをいつ実行するかを決めるだけで、どう実行するかは変えない） |
| `inputs` または `rules` | 追加したファイルが新しい依存になり、それを得たテストが 1 回実行される |

smates が補う既定値（検出したコマンドやテストの glob）はハッシュに含まれません。既定値はマシンに依存する（たとえば virtualenv があるかどうか）ため、含めると CI で記録したステートをノート PC で使ったときにすべてのスイートが再実行されてしまいます。`smates.yml` 自体を読むテストは、他のファイルと同じくその内容に依存します。

## コマンドラインから smates.yml を編集する { #edit-smatesyml-from-the-command-line }

`smates config` を使うと、YAML を手で編集せずにファイルを変更できます。コメントとレイアウトは保たれ、書き込む前に結果が検証されます。

```bash
smates config add-ignore 'design/**' 'deploy/**'      # トップレベルの ignore: に追加
smates config add-input .github/workflows/test.yml     # トップレベルの inputs: に追加
smates config set orphans all                          # トップレベルのスカラー値を設定
```

`--dry-run` を付けると、書き込まずに差分を表示します。

```text
$ smates config add-ignore 'design/**' --dry-run
would add to ignore: design/**

--- a/smates.yml
+++ b/smates.yml
@@ -4,3 +4,5 @@
     runner: custom
     root: e2e
     command: "npx playwright test"
+ignore:
+  - "design/**"
(dry run: smates.yml not changed)
```

`add-ignore` と `add-input` は既にある値を追加しません。`set` で設定できるのはトップレベルのスカラー値（`version`、`orphans`、`untested`、`state`、`state_seed`、`state_shared`、`flaky.retry`、`agent.*` のキー）です。スイートの設定は手で編集してください。`--format json` で機械可読な結果が得られます。

## 関連項目 { #see-also }

- [smates.yml リファレンス](../reference/configuration.md): すべてのキーと既定値
- [言語とランナー](runners.md): 各ランナーが何をトレースするか
- [サーバーを叩くテスト](servers.md): `depends_on` と `serves`
- [どのテストにも紐づかない変更](orphans.md): `ignore`、`orphans`、`smates orphans --apply`

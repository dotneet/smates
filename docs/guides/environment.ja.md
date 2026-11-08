# 環境変数 { #environment-variables }

テストが読み取る環境変数は、ファイルと同じく依存になります。値が変わると、それを読んだテストが再実行されます。このページでは、何が読み取りとして数えられるか、値をどう比較するか、どの変数を追跡するかの調整方法を説明します。

```text
$ TZ=Asia/Tokyo smates run --explain
  web (vitest): 1/40
    + packages/web/src/dates.test.ts  [env changed: TZ]
```

```text
$ smates why '$TZ'
web	packages/web/src/dates.test.ts	(recorded)
```

`smates deps <test>` では、変数は `$NAME` と表示されます。追跡は既定で有効です。

## 読み取りとして数えるもの { #what-counts-as-a-read }

| 言語 | 追跡する | 追跡しない |
|---|---|---|
| Node.js | `process.env.X`、`"X" in process.env`。jest がファイルごとに作る `process.env` のコピーや vitest の `import.meta.env` 経由も含む | — |
| Bun | `process.env.X` | `Bun.env`、`import.meta.env` |
| Python | `os.environ["X"]`、`os.environ.get("X")`、`os.getenv("X")`、`"X" in os.environ` | — |
| Go | `os.Getenv`、`os.LookupEnv`（テストログから） | `syscall.Getenv` |

読み取りとして数えるのは、次の条件をすべて満たす場合だけです。

- **スタック上にリポジトリのコードがある。** ランナーやレポーター、ライブラリが自分の都合で行う参照まで数えると、すべてのテストがそれに依存してしまいます。テストが呼び出したライブラリ（`AWS_REGION` を読む SDK など）による読み取りは数えます。
- **テストフレームワークが、これからスタブする値を退避しているのではない**（`vi.stubEnv`、`monkeypatch.setenv`）。
- **値がプロセス起動時のままである。** `TZ` を設定してから読み戻すテストは、外側の `TZ` には依存しません。
- **環境全体のコピーの一部ではない**（`{...process.env}`、`dict(os.environ)`、`os.environ.copy()`、子プロセスの起動）。コピーを受け取って起動した子プロセスは、自分の読み取りを記録します。

モジュールの import 中（プロセスごとに 1 回）の読み取りは、ファイルの読み取りと同じく、そのモジュールに依存するすべてのテストの依存になります。

## 値の比較方法 { #how-values-are-compared }

smates が保存するのは値のハッシュだけで、値そのものは保存しません。そのため、ステートファイルから秘密情報が漏れることはありません。未設定と空文字は区別されるので、テストが存在を確認した変数を新たに設定すると、そのテストが選択されます。

比較する値は、そのスイートのテストから見える値です。`smates.yml` のスイートの `env:` はシェルの値より優先されます。

```yaml
  - name: web
    runner: vitest
    root: packages/web
    env: { TZ: UTC }
```

この設定なら、`TZ=Asia/Tokyo smates run` としても `web` では何も選択されません。テストから見える値は常に `UTC` だからです。`env:` の値を変更すると、`env:` はスイートの設定の一部なので、スイート全体が再実行されます。

!!! warning "推測しやすい値"
    ランダムなトークンならハッシュで守られますが、推測しやすい値（弱いパスワードや短い列挙値）は、ステートファイルを読める人なら照合できてしまいます。そのような変数は `env_ignore` に入れてください。

## 既定で無視する変数 { #variables-ignored-by-default }

ステートはたいてい CI で記録してノート PC に取り込みますが、そこではテストと無関係な理由で多くの変数が異なります。以下は既定で無視するので、ローカルでの最初の実行ですべてのテストが選択されることはありません。

| 分類 | 例 |
|---|---|
| ユーザー、マシン、シェル | `PATH`、`HOME`、`USER`、`SHELL`、`PWD`、`TMPDIR`、`HOSTNAME`、`XDG_*`、`SSH_*`、`EDITOR`、Windows の同等のもの |
| ロケール | `LANG`、`LANGUAGE`、`LC_*` |
| 端末と色 | `TERM`、`COLUMNS`、`LINES`、`NO_COLOR`、`FORCE_COLOR`、`TMUX*`、`VSCODE_*` |
| CI システム | `CI`、`GITHUB_*`、`RUNNER_*`、`GITLAB_*`、`CI_*`、`CIRCLE*`、`BUILDKITE*`、`JENKINS_*` など |
| プロキシ | `HTTP_PROXY`、`HTTPS_PROXY`、`NO_PROXY`、それぞれの小文字版 |
| ツールチェーンとランナーのワーカー | `npm_*`、`NVM_*`、`VIRTUAL_ENV`、`CONDA_*`、`UV_*`、`GOPATH`、`GOCACHE`、`JEST_WORKER_ID`、`PYTEST_*` など |
| smates がトレースのために設定するもの | `SMATES_*`、`NODE_OPTIONS`、`NODE_V8_COVERAGE`、`BUN_OPTIONS`、`PYTHONPATH`、`GOFLAGS` など |

完全なリストは [internal/config/config.go](https://github.com/dotneet/smates/blob/main/internal/config/config.go) の `DefaultEnvIgnore` です。名前は大文字小文字を区別し、`*` の glob で照合します。追跡の対象として残るのは、プロジェクトが定義する変数（`DATABASE_URL`、`APP_*`）、`TZ`、`NODE_ENV` などです。

## 追跡を調整する: `env_ignore` と `env_track` { #tune-tracking-env_ignore-and-env_track }

```yaml
env_ignore: ["BUILD_*", DEPLOY_ID]   # 変わってもテストを選択しない
env_track: [LANG]                    # 既定のリストにある変数を追跡する
suites:
  - name: web
    runner: vitest
    env_ignore: [SENTRY_DSN]         # このスイートだけ
    env_track: [TERM]
```

- `env_ignore` は既定のリストに名前か glob を追加します。記録時にも比較時にも適用されるので、追加したその時点から、その変数でテストが選択されなくなります。
- `env_track` は既定のリストから名前を外します。意味があるのは、名前で読み取られる変数だけです。`env_ignore` を打ち消すことはできず、`env_ignore`（トップレベルでもスイートでも）にある名前は無視されたままです。
- どちらもトップレベルとスイートごとに書けます。スイートのリストはトップレベルのリストに追加されます。
- `env_ignore: ["*"]` で環境変数の追跡を無効にできます。

??? note "詳細: どの編集でテストが再実行されるか"
    スイートの `env:`、`env_ignore`、`env_track`、およびトップレベルの `env_track` はスイートの設定の一部なので、変更するとスイートが再実行されます。トップレベルの `env_ignore` はそうではなく、編集しても何も再実行されません。

    テストの実行時に無視されていた変数は記録されていません。そのため、トップレベルの `env_ignore` から外しただけでは何も選択されず、それを読むテストは次に実行されたときからその変数に依存します。すべてのテストについてすぐに記録するには `smates run --all` を実行します。

## 環境の列挙は追跡しない { #enumerating-the-environment-is-not-tracked }

環境を列挙して自分で変数を選び出すコードは、コピーと同じ操作で読み取るため、選び出した変数はどれも記録されません。

```ts
// 追跡されない: MYAPP_URL を変えても、MYAPP_TOKEN を追加しても何も選択されない
const config = Object.fromEntries(
  Object.entries(process.env).filter(([k]) => k.startsWith("MYAPP_")),
);
```

`{k: v for k, v in os.environ.items() if k.startswith("MYAPP_")}`、pydantic-settings などの設定ローダーも同様です。`env_track` は役に立たず、`inputs:` と `rules:` が受け取るのは変数ではなくファイルです。こうした設定を依存にするには、次のいずれかを行います。

- テスト対象のコードで変数を名前で読む（`process.env.MYAPP_URL`、`os.environ["MYAPP_URL"]`）
- スイートの `env:` で設定する（値を変えるとスイートが再実行される）
- テストが読むファイルから読み込む（dotenv のファイルは、git の管理下でも ignore されていてもファイルの依存になる）
- 環境を変えた後に `smates run --all` を実行する

## 制限事項 { #limitations }

- **ランタイム自身が参照するものは見えません。** 日付の書式化に使う `TZ`、ロケール、`GODEBUG`、ネイティブコードによる読み取りなどです。テストがタイムゾーンに依存するなら、スイートの `env:` で `TZ` を設定して、どのマシンでも同じ条件で実行されるようにしてください。
- **トレースできないコマンド**: シェルスクリプトやコンパイル済みのバイナリが読む変数は見えません。
- **Go の `t.Setenv`** は元に戻すために古い値を参照するので、変数を設定するテストも外側の値に依存します。必要以上に実行されることはあっても、実行されるべきときに実行されないことはありません。
- **Bun**: `Bun.env` と `import.meta.env` はトレースされません。`process.env` を使ってください。

詳しくは [制限事項](../reference/limitations.md) を参照してください。

## 関連項目 { #see-also }

- [スイートの設定](configuration.md#set-environment-variables): スイートの `env:`
- [smates が読む環境変数](../reference/environment-variables.md): smates 自身の設定
- [smates.yml リファレンス](../reference/configuration.md)

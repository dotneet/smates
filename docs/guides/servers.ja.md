# サーバーを叩くテスト { #tests-that-talk-to-a-server }

エンドツーエンドテストは、別のプロセスやコンテナで動くサーバーにアクセスすることがよくあります。docker compose で起動したバックエンドに対する Playwright、HTTP API に対する pytest などです。トレースはサーバーへのリクエストの先までは追えないので、テストがどのコードを動かしているかを `depends_on` か `serves` で smates に伝えます。

```yaml
suites:
  - name: api
    runner: go
    root: services/api
  - name: e2e
    runner: custom
    root: e2e
    tests: ["tests/**/*.spec.ts"]
    command: "npx playwright test"
    depends_on: [api]          # スイート api のコード
```

これでサーバーを変更すると e2e のテストが選択されます。

```text
+ e2e/tests/login.spec.ts  [changed: services/api/user.go (depends_on suite api)]
```

## 何が問題か { #the-problem }

smates がトレースするのは、テストのプロセスとその子プロセスが実行・読み取りしたファイルです。別に起動されたサーバー（docker compose、CI のサービス、テスト前に動かすスクリプトなど）はテストの子プロセスではありません。宣言がなければ次のようになります。

- サーバーのコードを変更しても、e2e のテストは 1 つも選択されません。
- 他のスイートのテストがそのファイルをカバーしていない限り、変更したファイルは*オーファン*（どのテストにも紐づかない変更。[どのテストにも紐づかない変更](orphans.md) を参照）になります。

## 依存を宣言する: `depends_on` { #declare-the-dependency-depends_on }

`depends_on` はリストです。各エントリは、別のスイートのコードか、パスの集合のどちらかです。

```yaml
    depends_on:
      - api                              # {suite: api} と同じ
      - suite: worker
      - paths: ["docker-compose.yml", "services/api/migrations/**"]
        tests: ["admin/**"]              # 省略可: このエントリを持つテストを絞る
```

| エントリ | 意味 |
|---|---|
| `api` または `{suite: api}` | スイート `api` の root 以下のファイルから、テストファイル（どのスイートのものでも）、ignore されたファイルとベンダーのファイル、その下に root を持つスイートのファイルを除いたもの |
| `{paths: [globs]}` | リポジトリルートからの相対 glob（先頭の `./` は取り除かれます） |
| `tests: [globs]` | エントリを一部のテストだけに絞ります。スイートの root からの相対 glob です（既定はすべてのテスト） |

各エントリには `suite` か `paths` のどちらか一方が必要で、両方は書けません。スイートが自分自身に依存することもできません。依存する側のスイートのテストファイルが追加されることはありません。

### `serves`: 短い書き方 { #serves-the-short-form }

`serves` は `paths` のエントリ 1 つの省略形です。サーバーが独立したスイートではない場合に使います。

```yaml
    serves: ["backend/**", "docker-compose.yml"]
    # 次と同じ: depends_on: [{paths: ["backend/**", "docker-compose.yml"]}]
```

## 追跡の仕組み { #how-it-is-tracked }

各宣言は、適用される各テストにとって 1 つの依存になります。そのハッシュは現在のコミットにおける全ファイルを対象にするので、記録されたステートはサーバーの大きさに応じて増えません。それらのファイルのどれかを変更すると、次のようになります。

- その宣言を持つテストが選択されます。
- `--suite e2e` で 1 つのスイートだけを実行する場合も、オーファンにはなりません。

## どこに表示されるか { #where-it-shows }

`smates run --explain` と `smates select --explain` は、変更されたファイルと宣言を示します。

```text
  e2e (custom): 1/2
    + e2e/admin/users.spec.ts  [changed: docker-compose.yml (depends_on docker-compose.yml)]
```

`smates why` は、宣言を通じてファイルに依存するテストを一覧します。

```text
$ smates why services/api/user.go
e2e	e2e/tests/login.spec.ts	(depends_on suite api)
e2e	e2e/admin/users.spec.ts	(depends_on suite api)
```

プルリクエストのサマリー（`--markdown`、`--format markdown`）では、こうしたテストが変更されたファイルの下に「server code its tests exercise」としてまとめられます。失敗した e2e テストの失敗ヒントでは、宣言が変更されたファイルに展開されます。

## 例: docker compose と Playwright { #example-docker-compose-and-playwright }

Go の API と worker がコンテナで動き、`e2e/` の Playwright のテストが `http://localhost:8080` の API を呼び出す構成です。

```text
.
├── docker-compose.yml
├── services/
│   ├── api/        # Go。独自のユニットテストあり
│   └── worker/     # Python。独自のテストなし
└── e2e/
    ├── playwright.config.ts
    └── tests/
        ├── login.spec.ts
        └── admin/users.spec.ts
```

```yaml
version: 1
suites:
  - name: api
    runner: go
    root: services/api
  - name: e2e
    runner: custom
    root: e2e
    tests: ["tests/**/*.spec.ts"]
    command: "npx playwright test"
    depends_on:
      - api                                   # テストを除いた API のコード
      - paths: ["docker-compose.yml"]         # スタックの起動方法
    serves: ["services/worker/**"]            # worker のコード（スイートではない）
```

テストの前にスタックを起動し、いつもどおり smates を実行します。

```bash
docker compose up -d --wait
smates run
```

この設定では次のようになります。

| 変更 | 実行されるもの |
|---|---|
| `services/api/handlers/user.go` | そのファイルに届く `api` のパッケージのテストと、すべての e2e テスト |
| `services/api/handlers/user_test.go` | そのパッケージのテストだけ（テストファイルは `{suite: api}` から除かれる） |
| `services/worker/jobs.py` | すべての e2e テスト |
| `e2e/tests/login.spec.ts` | `login.spec.ts` |
| `README.md` | 何も実行されない |

サーバーの一部にしか触れない e2e テストがある場合は、`tests:` でエントリを絞ります。たとえば `{paths: ["services/api/admin/**"], tests: ["tests/admin/**"]}` とすると、管理用 API の依存は admin のテストだけに付きます。

## smates doctor が宣言漏れを指摘する { #smates-doctor-points-at-missing-declarations }

`smates doctor` は、エンドツーエンドテストに見えるのに `depends_on`、`serves`、`rules` のどれも宣言していないスイートについて警告します。名前か root に e2e を含む、コマンドが Playwright か Cypress を実行する、テストファイルが `http://localhost` を呼び出す、のいずれかに当てはまるとエンドツーエンドテストとみなします（呼び出すサーバーを自分で起動するファイル、たとえばポート 0 の `httptest` サーバーや `http.server`、または nock、msw、`responses` でリクエストを横取りするファイルは数えません）。

```text
suite e2e looks like end-to-end tests (runs playwright) but declares no depends_on, serves or rules: tracing cannot follow requests into a server process, so changes to the server's code select none of its tests. Declare it, e.g. depends_on: [<server suite>] or serves: ["backend/**"]
```

`smates init --format json` も、同じ指摘を `needs_review` の項目として報告します。

## 関連項目 { #see-also }

- [スイートの設定](configuration.md): トレースでは見えない他の依存のための `inputs` と `rules`
- [smates.yml リファレンス](../reference/configuration.md#depends_on): `depends_on` のすべてのキー
- [どのテストにも紐づかない変更](orphans.md)
- [制限事項](../reference/limitations.md)

# Go のテスト関数単位の選択 { #go-test-functions }

`granularity: func` を設定すると、smates はパッケージ全体ではなく Go のテスト関数を 1 つずつ選択します。1 つのパッケージにほとんどのテストがあり、ほとんどの変更がそこに届くため、パッケージ単位の選択ではあまり節約にならない場合に使います。

```yaml
suites:
  - name: api
    runner: go
    granularity: func
```

ユニットはトップレベルのテスト関数（`internal/api#TestCreateUser`）になり、関数本体の編集は、その本体を実行した関数だけを選択します。

```text
$ smates run --explain
smates: selected 2 of 3 test unit(s) across 1 suite(s) (33% skipped)
  api (go): 2/3
    + internal/api#TestPlaceOrder  [changed: internal/api/order.go]
    + internal/api#(build)  [changed: internal/api/order.go]
```

`internal/api#(build)` はパッケージのコンパイルチェックです（[後述](#compile-checks)）。既定の `granularity: package` では、パッケージごとに 1 つのユニットのままです。

## テスト関数として扱うもの { #what-counts-as-a-test-function }

パッケージの `_test.go` ファイルにあるトップレベルの `TestXxx(*testing.T)`、`FuzzXxx(*testing.F)`（シードコーパスを含む）、`ExampleXxx()` のすべてです。サブテストは親と一緒に実行します。パースできないテストファイルがあると、パッケージ自体が 1 つのユニットになり、全体として実行され（そして失敗し）ます。

スイートのビルド設定ではコンパイルされないテストファイル（macOS 上の `//go:build linux` のファイルや、`GOFLAGS` にもコマンドにも渡されないタグ）にだけ宣言された関数は、その環境ではユニットになりません。

## 関数が依存するもの { #what-a-function-depends-on }

記録中、smates は各パッケージのテストバイナリを、リンクされるリポジトリ内のパッケージを対象にしたカバレッジ付きで 1 回だけビルドします（`go test -c -cover -coverpkg=...`。`-tags` や `-race` などスイートのコマンドのビルドフラグも使います）。そのうえで、各関数を専用のプロセスで実行します（`-test.run '^TestX$'`、カバレッジプロファイルとテストログ付き）。関数が依存するのは次のものです。

- **実行したファイル。** 関数のブロックが 1 つでも実行された Go のファイルすべて。パッケージの初期化はどのプロセスでも実行されるので、すべての関数の依存になります。
- **到達しうる宣言。** import の閉包にある他のすべての Go ファイルについて、関数本体を除いた部分。定数、変数の初期化式、型、シグネチャ、メソッドを変更すると、それが見える可能性のあるすべての関数が選択されます。本体の中だけの編集なら、それを実行した関数だけが選択されます。
- **テストファイル。** 自分の `_test.go` ファイル、自分が名前で参照するヘルパーを宣言したテストファイル、`TestMain` や `init`、関数呼び出しで初期化されるパッケージレベル変数を含むテストファイル。
- **パッケージ単位のユニットが持つものすべて。** テストログに記録されたファイルとディレクトリ、読み取った環境変数、埋め込みファイルなど Go 以外のファイル、`go.mod`/`go.sum`、子プロセスが使ったもの。サブプロセスとしてビルド・実行する Go のパッケージ（`go run ../cmd/tool`）は内容全体が依存になります。
- **パッケージディレクトリの一覧。** パッケージにファイルを追加すると、そのパッケージのすべての関数が選択されます。

帰属の単位はファイルです。`user.go` の 1 行を実行した関数は `user.go` 全体に依存します。宣言はトークンとして比較するので、整形だけの変更は数えません。

```text
$ smates deps 'internal/api#TestPlaceOrder'
go.mod	static,recorded
internal/api/	static,recorded
internal/api/order.go	recorded
internal/api/order.go#decls	static,recorded
internal/api/user.go#decls	static,recorded
internal/api/user_test.go	static,recorded
smates.yml	static,recorded
```

## テストバイナリの再実行 { #re-executions-of-the-test-binary }

ヘルパープロセスのパターンでは、テストの一部を同じバイナリの子プロセスで実行します。

```go
cmd := exec.Command(os.Args[0], "-test.run=^TestHelperProcess$")
cmd.Env = append(os.Environ(), "WANT_HELPER=1")
```

このような子プロセスが実行したコードも、それを起動したテストの依存になります。記録中、smates は `-overlay` を使ってパッケージに小さなテストファイルを追加します（自分で指定した `-overlay` とはマージされ、パッケージに何かを書き込むことはありません）。これが各再実行に関数のカバレッジディレクトリを渡すので、子プロセスのカバレッジが親のものに合流します。

カバレッジを書き出さない子プロセス（panic したり kill されたりして終了フックが動かなかったもの）があると、その関数は到達しうるすべての Go ファイルに依存します。子プロセスが読んだファイルは、その子プロセス自身のテストログから記録します。`os.Exit` で終わる子プロセスではこのログが失われるため、関数はその Go モジュールのすべてのファイルに依存します（精度を保つには、ヘルパーで exit せずに return してください）。独自の環境（`os.Environ()` を含まない `cmd.Env`）で起動された子プロセスや、バイナリのコピーから起動された子プロセスは見えません。Go のテストが起動するそれ以外の子プロセスについては [Go のテストの子プロセス](runners.md#child-processes-of-go-tests) を参照してください。

## コンパイルチェック { #compile-checks }

`go test` は、テストがどの本体を実行するかに関係なく、コンパイルできないパッケージを失敗させます。どのテストも実行しない本体の中の編集（型が合わなくなった未使用のヘルパー、未使用の import、vet の失敗）は、そのままでは何も選択しません。そこで、テスト関数を持つパッケージには必ずコンパイルチェックのユニット `internal/api#(build)` も作ります。

- パッケージの静的な閉包全体の内容に依存します。そのパッケージの Go ファイルとテストファイル、import しているリポジトリ内のパッケージのファイル、埋め込みファイル、`go.mod` です。
- テストバイナリをビルドして vet まで行い、実行はせずに `go test -run '^$'` を実行します（smates がその `-exec` プログラムになり、`TestMain` は動きません）。スイートのコマンドのビルドフラグと `-vet` の設定を使います。
- ビルドできないパッケージは、コンパイラの出力とともにチェックが失敗します。

```text
$ smates run --explain
smates: selected 1 of 3 test unit(s) across 1 suite(s) (67% skipped)
  api (go): 1/3
    + internal/api#(build)  [changed: internal/api/legacy.go]
smates: api: running 1 test unit(s) with go
── smates: compile check of ./internal/api (exit 1, 75ms)
# example.com/shop/internal/api [example.com/shop/internal/api.test]
internal/api/legacy.go:4:9: cannot use "1" (untyped string constant) as int value in return statement
FAIL	example.com/shop/internal/api [build failed]
```

したがって、通常の本体の編集では、それを実行した関数と、変更したパッケージおよびそれを import するパッケージの軽いチェックが選択されます。ビルドの失敗はチェックが引き受け、関数は記録された結果を保つので、修正後はチェックだけが再実行されます。

## 選択した関数の実行方法 { #how-selected-functions-run }

記録しない実行では、選択した関数をパッケージのまとまりごとに 1 回の `go test` で実行します。

```bash
go test -run '^(TestA|TestB)$' ./internal/api ./internal/store
```

パターンは 16 KiB に収まるだけのパッケージをまとめます。まとめた別のパッケージに同じ名前の関数があれば、それも実行されます。1 つのパッケージだけで上限を超える場合は、そのパッケージ全体を実行します。結果は go test の `--- FAIL`/`--- PASS` の行から読み取ります。記録中は、前述のとおり関数ごとに専用のプロセスで実行します。実行されるコマンドは `smates run --dry-run` で確認できます。

## パッケージ全体にフォールバックする場合 { #when-smates-falls-back-to-the-whole-package }

次のパッケージは関数単位で記録できません。

- テストバイナリがカバレッジ付きでビルドできない、または `go list` が失敗する
- スイートのコマンドが素の `go test` ではない
- コマンド（または `GOFLAGS`、`smates run --` の後の引数）が `-run`、`-skip`、`-bench`、`-fuzz`、`-list`、`-c`、`-o` を渡している

このようなパッケージは全体として記録され、その旨が表示されます。

```text
smates: api: internal/api: recorded at package granularity (the suite command passes -run): each of its test functions depends on everything the package does
```

その場合、各関数はパッケージの閉包にあるすべての Go ファイルに依存します。関数の名前を使ったパッケージ単位の選択になるということです。選択は粗くなりますが、正しさは保たれます。

## 記録のコスト { #what-recording-costs }

記録では、テスト関数ごとに 1 プロセス、加えてパッケージごとに 1 回のカバレッジ付きビルドが必要です。デフォルトブランチでのフル実行は遅くなりますが、選択的な実行では選択した関数だけを起動します。

- 1 つのパッケージの関数は、`go test` と同じく 1 つずつ実行します。テスト同士が状態を共有しないなら、`record_jobs`（`record_jobs: 4` など）で同時に実行する数を増やせます。
- パッケージは `-p`（コマンドか `GOFLAGS` で指定。既定は GOMAXPROCS）が許す数だけ同時に実行します。パッケージ同士がデータベースを共有するために `go test -p 1` で実行しているスイートは、記録中も同じく分けて実行されます。

記録中、関数同士はプロセスを共有しません。同じパッケージの別のテストがメモリ上に残した状態に頼るテストは、`go test` のときとは異なる振る舞いになります。

## テストフラグ { #test-flags }

`-timeout`、`-short`、`-count`、`-parallel`、`-v` などのテストフラグは、コマンドにあっても `GOFLAGS` にあっても、各関数のプロセスに渡されます。両方にある場合はコマンドの値が優先されます。`-timeout` がなければ、go test の既定の 10 分がプロセスごとに適用されます。

```yaml
  - name: api
    runner: go
    granularity: func
    command: "go test -race -tags integration -timeout 2m"
    record_jobs: 4
```

## 関連項目 { #see-also }

- [言語とランナー](runners.md#go): パッケージ単位の選択
- [スイートの設定](configuration.md)
- [制限事項](../reference/limitations.md)
- [設計、第 2 節](../DESIGN.md#2-tracing-per-ecosystem): 内部の仕組み

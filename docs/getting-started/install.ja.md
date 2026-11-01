# smates のインストール { #install-smates }

smates は Linux、macOS、Windows（amd64 と arm64）向けの単一の静的バイナリです。このページでは
インストール方法、インストールの確認、アップグレードの手順を説明します。

## インストールスクリプト（Linux、macOS） { #install-script-linux-macos }

```bash
curl -fsSL https://raw.githubusercontent.com/dotneet/smates/main/install.sh | sh
```

スクリプトは GitHub から最新リリースのアーカイブをダウンロードし、リリースの
`checksums.txt`（SHA-256）で検証してから、`smates` を `/usr/local/bin`（書き込めない場合は
`~/.local/bin`）にインストールします。そのディレクトリが `PATH` に含まれていなければ、その旨を
表示します。`curl` または `wget`、`tar`、そして `sha256sum`・`shasum`・`openssl` のいずれかが
必要です。

オプションは `sh -s --` の後に渡します。

```bash
# v1.2.3 を ~/bin にインストールする
curl -fsSL https://raw.githubusercontent.com/dotneet/smates/main/install.sh | sh -s -- -b ~/bin v1.2.3
```

| オプション / 環境変数 | 効果 |
|---|---|
| `-b DIR` または `SMATES_INSTALL_DIR=DIR` | インストール先ディレクトリ。 |
| 引数 `VERSION` または `SMATES_VERSION` | インストールするリリース（`v1.2.3` または `1.2.3`）。既定は最新リリース。 |
| `SMATES_VERIFY=attestation` | アーカイブのビルド来歴（attestation）も `gh` CLI で検証します（下記参照）。 |

??? note "詳細: ビルド来歴の検証"
    `SMATES_VERIFY=attestation` を指定すると、スクリプトはアーカイブに対して
    `gh attestation verify` を実行します。アーカイブは、smates のリリースワークフローが
    そのリリースのタグから、GitHub ホストのランナー上でビルドしたものでなければなりません。
    ログイン済み（または `GH_TOKEN` を設定済み）で、`--source-ref` に対応したバージョンの
    [gh CLI](https://cli.github.com) が必要です。attestation の導入より前に公開された
    リリースには attestation がないので、`SMATES_VERIFY` なしでインストールしてください。

    ```bash
    curl -fsSL https://raw.githubusercontent.com/dotneet/smates/main/install.sh | SMATES_VERIFY=attestation sh
    ```

## リリースからダウンロードする { #release-downloads }

各 [GitHub リリース](https://github.com/dotneet/smates/releases) には、プラットフォームごとの
アーカイブと `checksums.txt` があります。

| プラットフォーム | アーカイブ |
|---|---|
| Linux | `smates_linux_amd64.tar.gz`、`smates_linux_arm64.tar.gz` |
| macOS | `smates_darwin_amd64.tar.gz`、`smates_darwin_arm64.tar.gz` |
| Windows | `smates_windows_amd64.zip`、`smates_windows_arm64.zip` |

`smates`（Windows では `smates.exe`）を取り出して `PATH` の通った場所に置きます。Windows でも
CLI はビルド・実行できますが、トレースの動作確認は Linux と macOS でのみ行っています。

## go install { #go-install }

Go 1.26 以降があれば次のコマンドでインストールできます。

```bash
go install github.com/dotneet/smates/cmd/smates@latest
```

バイナリは `$(go env GOPATH)/bin` に置かれます。この方法でビルドしたものはバージョンが
`dev` と表示されます。

## GitHub Actions { #github-actions }

GitHub Actions ではアクションが smates をインストールします: `uses: dotneet/smates@v1`。
詳しくは [GitHub Actions](../ci/github-actions.md) を参照してください。

## インストールを確認する { #check-the-install }

```text
$ smates version
smates 1.2.3
```

`smates --version` でも同じ結果になります。リポジトリの中で `smates doctor` を実行すると、
検出されたスイート、ランナーのツールチェーン、記録済みのステート、エージェントの認証情報
といったセットアップを確認できます。

## アップグレード { #upgrade }

インストールスクリプトをもう一度実行すると、バイナリをその場で置き換えます（smates の実行中でも
安全です）。特定のバージョンに留めたい場合は明示的に指定します（`sh -s -- v1.2.3`）。
`go install` の場合は同じコマンドを再実行します。GitHub Actions では `uses:` の ref で
バージョンが決まります（`@v1` は v1.x の最新リリース、`@v1.2.3` はそのリリースそのもの）。

## 次のステップ { #next-steps }

- [クイックスタート](quickstart.md): 一度記録して、選択的な実行を体験します。
- [選択の仕組み](how-it-works.md): テストが実行されるか、スキップされるかを決めるもの。

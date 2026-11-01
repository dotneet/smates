# Install smates

smates is a single static binary for Linux, macOS and Windows (amd64 and arm64). This page
shows the ways to install it, how to check the install and how to upgrade.

## Install script (Linux, macOS)

```bash
curl -fsSL https://raw.githubusercontent.com/dotneet/smates/main/install.sh | sh
```

The script downloads the latest release archive from GitHub, checks it against the
release's `checksums.txt` (SHA-256) and installs `smates` into `/usr/local/bin` when
that directory is writable, else into `~/.local/bin`. It prints a note when the directory
is not on your `PATH`. It needs `curl` or `wget`, `tar`, and one of `sha256sum`, `shasum`
or `openssl`.

Pass options after `sh -s --`:

```bash
# install v1.2.3 into ~/bin
curl -fsSL https://raw.githubusercontent.com/dotneet/smates/main/install.sh | sh -s -- -b ~/bin v1.2.3
```

| Option / variable | Effect |
|---|---|
| `-b DIR` or `SMATES_INSTALL_DIR=DIR` | Install directory. |
| `VERSION` argument or `SMATES_VERSION` | Release to install (`v1.2.3` or `1.2.3`). Default: the latest release. |
| `SMATES_VERIFY=attestation` | Also verify the archive's build provenance attestation with the `gh` CLI (see below). |

??? note "Details: verifying the build provenance"
    With `SMATES_VERIFY=attestation` the script runs `gh attestation verify` on the
    archive: it must have been built by smates' release workflow from the release's tag,
    on a GitHub-hosted runner. This needs the [gh CLI](https://cli.github.com), logged in
    or with `GH_TOKEN` set, in a version that supports `--source-ref`. Releases published
    before attestations were introduced have none; install those without
    `SMATES_VERIFY`.

    ```bash
    curl -fsSL https://raw.githubusercontent.com/dotneet/smates/main/install.sh | SMATES_VERIFY=attestation sh
    ```

## Release downloads

Every [GitHub release](https://github.com/dotneet/smates/releases) has one archive per
platform plus `checksums.txt`:

| Platform | Archive |
|---|---|
| Linux | `smates_linux_amd64.tar.gz`, `smates_linux_arm64.tar.gz` |
| macOS | `smates_darwin_amd64.tar.gz`, `smates_darwin_arm64.tar.gz` |
| Windows | `smates_windows_amd64.zip`, `smates_windows_arm64.zip` |

Extract `smates` (`smates.exe` on Windows) and put it on your `PATH`. On Windows the CLI
builds and runs, but tracing has only been verified on Linux and macOS.

## go install

With Go 1.26 or newer:

```bash
go install github.com/dotneet/smates/cmd/smates@latest
```

The binary lands in `$(go env GOPATH)/bin` and reports the module version it was built
from (`@v1.2.3` prints `smates 1.2.3`).

## GitHub Actions

The action installs smates for you: `uses: dotneet/smates@v1`. See
[GitHub Actions](../ci/github-actions.md).

## Check the install

```text
$ smates version
smates 1.2.3
```

`smates --version` prints the same. Inside a repository, `smates doctor` checks the setup:
the detected suites, the runner toolchains, the recorded state and the agent's
credentials.

## Upgrade

Run the install script again; it replaces the binary in place (safe even while smates is
running). To stay on a known version, pass it explicitly (`sh -s -- v1.2.3`). With
`go install`, rerun the same command. In GitHub Actions, the version follows the
`uses:` ref (`@v1` is the newest v1.x release, `@v1.2.3` exactly that one).

## Next steps

- [Quick start](quickstart.md): record once and see a selective run.
- [How smates decides](how-it-works.md): what makes a test run or skip.

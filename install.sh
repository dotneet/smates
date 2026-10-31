#!/bin/sh
# smates installer.
#
#   curl -fsSL https://raw.githubusercontent.com/dotneet/smates/main/install.sh | sh
#   curl -fsSL https://raw.githubusercontent.com/dotneet/smates/main/install.sh | sh -s -- -b /usr/local/bin v1.2.3
#
# Downloads a release archive from GitHub, verifies it against the release's
# checksums.txt (SHA-256) and installs the smates binary. Needs curl or wget, tar
# (unzip on Windows) and one of sha256sum, shasum or openssl. No API token is used.
#
# Options:
#   -b DIR   install directory (default: $SMATES_INSTALL_DIR, else /usr/local/bin when
#            writable, else ~/.local/bin)
#   -h       show this help
# Arguments:
#   VERSION  release to install, e.g. v1.2.3 or 1.2.3 (default: $SMATES_VERSION, else latest)

set -eu

REPO=dotneet/smates
GITHUB=https://github.com

log() {
	printf 'smates-install: %s\n' "$*" >&2
}

die() {
	log "error: $*"
	exit 1
}

usage() {
	cat <<'EOF'
usage: install.sh [-b bindir] [version]

  -b DIR   install directory (default: $SMATES_INSTALL_DIR, else /usr/local/bin
           when writable, else ~/.local/bin)
  -h       show this help
  version  release to install, e.g. v1.2.3 (default: $SMATES_VERSION, else latest)
EOF
}

has() {
	command -v "$1" >/dev/null 2>&1
}

detect_os() {
	case "$(uname -s)" in
	Linux) echo linux ;;
	Darwin) echo darwin ;;
	MINGW* | MSYS* | CYGWIN* | Windows_NT) echo windows ;;
	*) die "unsupported OS: $(uname -s)" ;;
	esac
}

detect_arch() {
	arch=$(uname -m)
	# A shell running under Rosetta reports x86_64 on Apple silicon; prefer native.
	if [ "$arch" = x86_64 ] && [ "$(uname -s)" = Darwin ] &&
		[ "$(sysctl -n sysctl.proc_translated 2>/dev/null || echo 0)" = 1 ]; then
		arch=arm64
	fi
	case "$arch" in
	x86_64 | amd64) echo amd64 ;;
	aarch64 | arm64 | armv8*) echo arm64 ;;
	*) die "unsupported architecture: $arch (build from source: go install github.com/$REPO/cmd/smates@latest)" ;;
	esac
}

# fetch URL FILE downloads URL to FILE.
fetch() {
	if has curl; then
		curl -fsSL --retry 3 --connect-timeout 20 -o "$2" "$1"
	elif has wget; then
		wget -q -O "$2" "$1"
	else
		die "curl or wget is required"
	fi
}

# latest_tag prints the newest release tag by following the releases/latest redirect.
latest_tag() {
	url=$GITHUB/$REPO/releases/latest
	if has curl; then
		effective=$(curl -fsSLI --retry 3 --connect-timeout 20 -o /dev/null -w '%{url_effective}' "$url") || return 1
	else
		effective=$(wget -S --spider "$url" 2>&1 | sed -n 's/^ *[Ll]ocation: *//p' | tail -n 1 | tr -d '\r')
	fi
	tag=${effective##*/releases/tag/}
	if [ -z "$effective" ] || [ "$tag" = "$effective" ] || [ -z "$tag" ]; then
		return 1
	fi
	echo "$tag"
}

sha256() {
	if has sha256sum; then
		sha256sum "$1" | awk '{print $1}'
	elif has shasum; then
		shasum -a 256 "$1" | awk '{print $1}'
	elif has openssl; then
		openssl dgst -sha256 -r "$1" | awk '{print $1}'
	else
		die "sha256sum, shasum or openssl is required to verify the download"
	fi
}

default_bindir() {
	if [ -n "${SMATES_INSTALL_DIR:-}" ]; then
		echo "$SMATES_INSTALL_DIR"
	elif [ -d /usr/local/bin ] && [ -w /usr/local/bin ]; then
		echo /usr/local/bin
	else
		echo "${HOME:?HOME is not set}/.local/bin"
	fi
}

main() {
	bindir=
	while getopts 'b:h' opt; do
		case "$opt" in
		b) bindir=$OPTARG ;;
		h)
			usage
			exit 0
			;;
		*)
			usage >&2
			exit 2
			;;
		esac
	done
	shift $((OPTIND - 1))
	[ $# -le 1 ] || {
		usage >&2
		exit 2
	}
	version=${1:-${SMATES_VERSION:-latest}}
	bindir=${bindir:-$(default_bindir)}

	os=$(detect_os)
	arch=$(detect_arch)
	exe=
	ext=tar.gz
	if [ "$os" = windows ]; then
		exe=.exe
		ext=zip
	fi

	case "$version" in
	latest) version=$(latest_tag) || die "could not resolve the latest release; pass a version such as v1.2.3" ;;
	v*) ;;
	[0-9]*) version=v$version ;;
	*) die "invalid version: $version" ;;
	esac

	mkdir -p "$bindir" || die "cannot create $bindir"
	[ -w "$bindir" ] || die "$bindir is not writable (choose another directory with -b, or run with sudo)"

	archive=smates_${os}_${arch}.$ext
	base=$GITHUB/$REPO/releases/download/$version

	tmp=$(mktemp -d 2>/dev/null || mktemp -d -t smates)
	trap 'rm -rf "$tmp"' EXIT
	trap 'exit 130' INT TERM

	log "downloading smates $version ($os/$arch)"
	fetch "$base/$archive" "$tmp/$archive" || die "download failed: $base/$archive"
	fetch "$base/checksums.txt" "$tmp/checksums.txt" || die "download failed: $base/checksums.txt"

	expected=$(awk -v f="$archive" '$2 == f || $2 == "*" f { print $1; exit }' "$tmp/checksums.txt")
	[ -n "$expected" ] || die "$archive is not listed in checksums.txt"
	actual=$(sha256 "$tmp/$archive")
	[ "$expected" = "$actual" ] || die "checksum mismatch for $archive: expected $expected, got $actual"

	case "$ext" in
	tar.gz) tar -xzf "$tmp/$archive" -C "$tmp" "smates$exe" ;;
	zip)
		has unzip || die "unzip is required"
		unzip -oq "$tmp/$archive" "smates$exe" -d "$tmp"
		;;
	esac

	# Copy next to the target, then rename: atomic, and safe while smates is running.
	cp "$tmp/smates$exe" "$bindir/.smates$exe.tmp.$$"
	chmod 755 "$bindir/.smates$exe.tmp.$$"
	mv -f "$bindir/.smates$exe.tmp.$$" "$bindir/smates$exe"
	log "installed $bindir/smates$exe"

	case ":$PATH:" in
	*":$bindir:"*) ;;
	*) log "note: $bindir is not on PATH; add it, e.g. export PATH=\"$bindir:\$PATH\"" ;;
	esac
}

main "$@"

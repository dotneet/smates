#!/usr/bin/env bash
# Tests the "Install smates" step of action.yml against a fake release: fake curl and
# gh on PATH, no network. Run: bash action/install_test.sh
set -uo pipefail

here=$(cd "$(dirname "$0")" && pwd)
root=$(dirname "$here")
# shellcheck source=action/lib.sh
. "$here/lib.sh"
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

# The step's script: the `run: |` block after `id: install`, de-indented.
awk '
  /^      id: install$/ { found = 1; next }
  found && /^      run: \|$/ { inrun = 1; next }
  inrun && /^ {0,7}[^ ]/ { exit }
  inrun { sub(/^        /, ""); print }
' "$root/action.yml" >"$work/install.sh"
grep -q 'install_release' "$work/install.sh" || {
  echo "FAIL: could not extract the install step from action.yml"
  exit 1
}

failures=0
fail() {
  echo "FAIL: $*"
  failures=$((failures + 1))
}

archive=smates_test_amd64.tar.gz
release=$work/release
mkdir -p "$release/pkg" "$work/fakebin" "$work/runner-temp" "$work/tmpdir"
printf '#!/bin/sh\necho "smates fake v1.0.0"\n' >"$release/pkg/smates"
chmod +x "$release/pkg/smates"
tar -czf "$release/$archive" -C "$release/pkg" smates
(cd "$release" && printf '%s  %s\n' "$(smates_sha256 "$archive")" "$archive" >checksums.txt)

cat >"$work/fakebin/curl" <<EOF
#!/bin/sh
# fake curl: copies \$release/<basename of URL> to the -o file.
while [ \$# -gt 0 ]; do
  case "\$1" in
    -o) out=\$2; shift 2 ;;
    -*) shift ;;
    *) url=\$1; shift ;;
  esac
done
echo "\$url" >>"$work/curl.log"
cp "$release/\${url##*/}" "\$out"
EOF
cat >"$work/fakebin/gh" <<'EOF'
#!/bin/sh
if [ "$3" = --help ]; then echo "--source-ref --source-digest --signer-workflow --deny-self-hosted-runners"; exit 0; fi
case "$1" in
  api) if [ "${FAKE_API:-1}" = 404 ]; then echo "HTTP 404"; exit 1; fi; echo "${FAKE_API:-1}" ;;
  attestation) exit "${FAKE_VERIFY:-0}" ;;
esac
EOF
chmod +x "$work/fakebin/curl" "$work/fakebin/gh"

tool_dir=$work/toolcache/smates/v1.0.0/linux-amd64
cache_dir=$work/runner-temp/smates-archive-cache/v1.0.0-linux-amd64

# run NAME WANT_STATUS PRESENT CACHE_HIT VERIFY REQUIRED runs the step; sets $out, $path_dir.
run() {
  local name=$1 want=$2 status=0
  out=$work/output.txt
  : >"$out"
  : >"$work/path.txt"
  : >"$work/curl.log"
  env -i PATH="$work/fakebin:/usr/bin:/bin:/usr/sbin:/sbin" HOME="${HOME:-/tmp}" \
    TMPDIR="$work/tmpdir" RUNNER_TEMP="$work/runner-temp" GITHUB_OUTPUT="$out" GITHUB_PATH="$work/path.txt" \
    GH_TOKEN=token SMATES_REPO=dotneet/smates SMATES_VERSION=v1.0.0 SMATES_ARCHIVE=$archive SMATES_EXE= \
    SMATES_TOOL_DIR="$tool_dir" SMATES_CACHE_DIR="$cache_dir" SMATES_PRESENT="$3" SMATES_CACHE_HIT="$4" \
    SMATES_VERIFY="$5" SMATES_ATTESTATION_REQUIRED="$6" SMATES_SOURCE_REF=refs/tags/v1.0.0 \
    SMATES_SOURCE_DIGEST=1111111111111111111111111111111111111111 \
    SMATES_ACTION_PATH="$root" SMATES_ACTION_REF=v1.0.0 \
    FAKE_API="${FAKE_API:-1}" FAKE_VERIFY="${FAKE_VERIFY:-0}" \
    bash "$work/install.sh" >"$work/log.txt" 2>&1 || status=$?
  path_dir=$(cat "$work/path.txt")
  # The step's scratch directory is removed on every exit path.
  if [ -n "$(ls -A "$work/tmpdir")" ]; then
    fail "$name: scratch files left behind: $(ls -A "$work/tmpdir")"
    rm -rf "${work:?}/tmpdir/"*
  fi
  if [ "$status" != "$want" ]; then
    fail "$name: exit $status, want $want; log:"
    sed 's/^/    /' "$work/log.txt"
  fi
}
output() { sed -n "s/^$1=//p" "$out"; }
# The PATH entry must hold the smates binary and nothing else.
check_path_dir() {
  local listing
  listing=$(ls -A "$path_dir" 2>/dev/null)
  [ "$listing" = smates ] || fail "$1: PATH entry '$path_dir' holds '$listing', want only smates"
  case "$path_dir" in
    "$tool_dir"* | "$cache_dir"*) fail "$1: PATH entry is a cache directory: $path_dir" ;;
  esac
}

run "fresh download" 0 false false auto true
[ "$(output method)" = download ] || fail "fresh download: method $(output method)"
[ "$(output verified)" = attestation ] || fail "fresh download: verified $(output verified)"
check_path_dir "fresh download"
[ -f "$tool_dir/$archive" ] || fail "fresh download: archive not kept in the tool cache"
[ "$(ls -A "$cache_dir")" = "$archive" ] || fail "fresh download: cache staging dir holds $(ls -A "$cache_dir")"

# A poisoned Actions cache entry: extra executables next to a genuine archive.
rm -rf "$tool_dir"
for tool in git go smates; do
  printf '#!/bin/sh\necho planted\n' >"$cache_dir/$tool"
  chmod +x "$cache_dir/$tool"
done
run "poisoned cache, genuine archive" 0 false true auto true
[ "$(output method)" = cached ] || fail "poisoned cache: method $(output method)"
[ "$(wc -l <"$work/curl.log" | tr -d ' ')" = 1 ] || fail "poisoned cache: want only checksums.txt fetched"
check_path_dir "poisoned cache"
"$path_dir/smates" | grep -q 'smates fake' || fail "poisoned cache: planted smates installed"

# The same in a persistent tool cache (self-hosted runners).
mkdir -p "$tool_dir"
cp "$release/$archive" "$tool_dir/"
printf '#!/bin/sh\necho planted\n' >"$tool_dir/git"
chmod +x "$tool_dir/git"
run "poisoned tool cache" 0 true false auto true
[ "$(output method)" = cached ] || fail "poisoned tool cache: method $(output method)"
check_path_dir "poisoned tool cache"

# A tampered cached archive fails, without falling back to a download.
echo garbage >>"$cache_dir/$archive"
rm -rf "$tool_dir"
run "tampered cached archive" 1 false true checksum false
[ -z "$path_dir" ] || fail "tampered cached archive: PATH changed"

FAKE_API=404 run "no attestation, required" 1 false false auto true
FAKE_API=404 run "no attestation, not required" 0 false false auto false
[ "$(output verified)" = checksum ] || fail "no attestation: verified $(output verified)"
FAKE_VERIFY=1 run "attestation does not verify" 1 false false auto false
run "checksum mode" 0 false false checksum false
[ "$(output verified)" = checksum ] || fail "checksum mode: verified $(output verified)"

if [ "$failures" -gt 0 ]; then
  echo "$failures failure(s)"
  exit 1
fi
echo "ok"

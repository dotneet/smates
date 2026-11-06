#!/usr/bin/env bash
# Tests for action/lib.sh: bash action/lib_test.sh (no network, no gh needed).
set -uo pipefail

here=$(cd "$(dirname "$0")" && pwd)
# shellcheck source=action/lib.sh
. "$here/lib.sh"

failures=0
fail() {
  echo "FAIL: $*"
  failures=$((failures + 1))
}

# expect NAME WANT_STATUS WANT_OUTPUT CMD... runs CMD and compares status and stdout.
expect() {
  local name=$1 want_status=$2 want_out=$3 out status=0
  shift 3
  out=$("$@") || status=$?
  if [ "$status" != "$want_status" ] || [ "$out" != "$want_out" ]; then
    fail "$name: got status $status, output '$out'; want status $want_status, output '$want_out'"
  fi
}

fixture=$here/testdata/ls-remote.txt
tags_for() { smates_tags_for_commit "$1" <"$fixture"; }

# --- smates_is_release_tag
for t in v1.2.3 v0.0.1 v1.2.3-rc.1 v10.20.30+build.5; do
  smates_is_release_tag "$t" || fail "is_release_tag $t"
done
for t in v1 v1.2 1.2.3 v1.2.3.4 release-1.2.3 'v1.2.3 ' ''; do
  ! smates_is_release_tag "$t" || fail "is_release_tag '$t' should be false"
done

# --- smates_tags_for_commit against `git ls-remote --tags` output
expect "annotated tag (peeled), moving v1 ignored" 0 v1.0.0 tags_for 1111111111111111111111111111111111111111
expect "tag object id is not the commit" 0 '' tags_for aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa1
expect "lightweight tag, moving v1.1 ignored" 0 v1.1.0 tags_for 2222222222222222222222222222222222222222
expect "two release tags" 0 $'v1.2.0\nv1.2.0-rc.1' tags_for 3333333333333333333333333333333333333333
expect "only non-release tags" 0 '' tags_for 4444444444444444444444444444444444444444
expect "non-v tag ignored" 0 '' tags_for 5555555555555555555555555555555555555555
expect "annotated tag" 0 v1.3.0 tags_for 6666666666666666666666666666666666666666
expect "upper-case sha" 0 v1.1.0 tags_for 2222222222222222222222222222222222222222
expect "upper-case listing" 0 v1.0.0 smates_tags_for_commit 1111111111111111111111111111111111111111 \
  < <(awk 'BEGIN { OFS = "\t" } { $1 = toupper($1); print }' "$fixture")
expect "unknown sha" 0 '' tags_for 9999999999999999999999999999999999999999
expect "empty listing" 0 '' smates_tags_for_commit 1111111111111111111111111111111111111111 </dev/null
# `git show-ref --tags -d` separates with a space instead of a tab.
expect "show-ref format" 0 v1.3.0 smates_tags_for_commit 6666666666666666666666666666666666666666 \
  < <(tr '\t' ' ' <"$fixture")

# --- smates_release_for_commit with a fake remote (git ls-remote)
git() {
  if [ "$1" = ls-remote ]; then
    if [ "${FAKE_REMOTE_FAIL:-}" = 1 ]; then
      return 128
    fi
    cat "$fixture"
    return 0
  fi
  command git "$@"
}
expect "remote: one release" 0 v1.0.0 smates_release_for_commit 1111111111111111111111111111111111111111 '' remote
expect "remote: none" 1 '' smates_release_for_commit 4444444444444444444444444444444444444444 /nonexistent remote
expect "remote: several" 2 $'v1.2.0\nv1.2.0-rc.1' smates_release_for_commit 3333333333333333333333333333333333333333 '' remote
FAKE_REMOTE_FAIL=1 expect "remote: unreachable" 3 '' smates_release_for_commit 1111111111111111111111111111111111111111 '' remote

# A local git checkout of the action: its own tags win, the remote is not needed.
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
(
  cd "$tmp" &&
    command git init -q &&
    command git -c user.name=t -c user.email=t@t commit -q --allow-empty -m one &&
    command git tag v9.9.9 &&
    command git -c user.name=t -c user.email=t@t tag -a -m annotated v9.9.8 &&
    command git tag v9 &&
    command git -c user.name=t -c user.email=t@t commit -q --allow-empty -m two &&
    command git tag v9.10.0
) || fail "creating the test repository"
head=$(command git -C "$tmp" rev-parse HEAD)
first=$(command git -C "$tmp" rev-parse HEAD~1)
FAKE_REMOTE_FAIL=1 expect "local: HEAD tag" 0 v9.10.0 smates_release_for_commit "$head" "$tmp" remote
# The checkout is not at the pinned commit: its tags are not trusted, the remote is asked.
FAKE_REMOTE_FAIL=1 expect "local: other commit" 3 '' smates_release_for_commit "$first" "$tmp" remote
command git -C "$tmp" checkout -q "$first"
FAKE_REMOTE_FAIL=1 expect "local: annotated + lightweight" 2 $'v9.9.8\nv9.9.9' smates_release_for_commit "$first" "$tmp" remote
unset -f git

# --- smates_verify_attestation with a fake gh
file=$tmp/smates_linux_amd64.tar.gz
echo archive >"$file"
digest=$(smates_sha256 "$file")
[ ${#digest} = 64 ] || fail "sha256: '$digest'"
gh_calls=$tmp/gh.calls
gh() {
  echo "$*" >>"$gh_calls"
  case "$1 $2" in
    "attestation verify")
      if [ "${3:-}" = --help ]; then
        echo "${FAKE_GH_HELP:---source-digest --source-ref --signer-workflow --deny-self-hosted-runners}"
        return 0
      fi
      return "${FAKE_GH_VERIFY:-0}"
      ;;
    "api repos/dotneet/smates/attestations/sha256:$digest")
      case "${FAKE_GH_API:-1}" in
        404)
          echo "gh: Not Found (HTTP 404)"
          return 1
          ;;
        error)
          echo "gh: connection refused"
          return 1
          ;;
        *) echo "${FAKE_GH_API:-1}" ;;
      esac
      ;;
    *) return 9 ;;
  esac
}
sleep() { :; }
verify() { smates_verify_attestation "$file" dotneet/smates refs/tags/v1.2.3 "$1" "$2" "$3" >/dev/null 2>&1; }
status_of() {
  local s=0
  "$@" || s=$?
  echo "$s"
}

export GH_TOKEN=token
: >"$gh_calls"
[ "$(status_of verify 3333333333333333333333333333333333333333 auto false)" = 0 ] || fail "verify: attested"
grep -q -- "attestation verify $file --repo dotneet/smates --signer-workflow dotneet/smates/.github/workflows/release.yml --deny-self-hosted-runners --source-ref refs/tags/v1.2.3 --source-digest 3333333333333333333333333333333333333333" "$gh_calls" ||
  fail "verify: policy flags not passed: $(cat "$gh_calls")"
: >"$gh_calls"
[ "$(status_of verify '' attestation false)" = 0 ] || fail "verify: attested, no digest"
grep -q -- "--source-digest" "$gh_calls" && fail "verify: empty digest passed"
[ "$(FAKE_GH_VERIFY=1 status_of verify '' auto false)" = 2 ] || fail "verify: failing attestation must fail in auto"
[ "$(FAKE_GH_VERIFY=1 status_of verify '' attestation false)" = 2 ] || fail "verify: failing attestation must fail"
[ "$(FAKE_GH_API=404 status_of verify '' auto false)" = 1 ] || fail "verify: none, auto falls back"
[ "$(FAKE_GH_API=0 status_of verify '' auto false)" = 1 ] || fail "verify: zero, auto falls back"
[ "$(FAKE_GH_API=404 status_of verify '' auto true)" = 2 ] || fail "verify: none but required"
[ "$(FAKE_GH_API=404 status_of verify '' attestation false)" = 2 ] || fail "verify: none, attestation mode"
[ "$(FAKE_GH_API=error status_of verify '' auto false)" = 1 ] || fail "verify: api error, auto falls back"
[ "$(FAKE_GH_API=error status_of verify '' auto true)" = 2 ] || fail "verify: api error, required"
[ "$(FAKE_GH_HELP=old status_of verify '' auto true)" = 1 ] || fail "verify: old gh, auto falls back"
[ "$(FAKE_GH_HELP=old status_of verify '' attestation false)" = 2 ] || fail "verify: old gh, attestation mode"
[ "$(GH_TOKEN='' GITHUB_TOKEN='' status_of verify '' attestation false)" = 2 ] || fail "verify: no token"
[ "$(GH_TOKEN='' GITHUB_TOKEN='' status_of verify '' auto false)" = 1 ] || fail "verify: no token, auto falls back"

# --- smates_restore_base
p=smates-state-Linux-
expect "base differs" 0 "${p}release-" smates_restore_base "$p" feature release main
expect "no base (push, schedule)" 0 "${p}main-" smates_restore_base "$p" main '' main
expect "no base, other branch" 0 "${p}topic-" smates_restore_base "$p" topic '' main
expect "base is the default" 0 "${p}feature-" smates_restore_base "$p" feature main main
expect "base is the branch" 0 "${p}release-" smates_restore_base "$p" release release main
expect "comma in base" 0 "${p}rel_1-" smates_restore_base "$p" feature 'rel,1' main
expect "comma in base equal to sanitized branch" 0 "${p}a_b-" smates_restore_base "$p" a_b 'a,b' main
expect "with cache-key" 0 "${p}unit-release-" smates_restore_base "${p}unit-" feature release main

if [ "$failures" -gt 0 ]; then
  echo "$failures failure(s)"
  exit 1
fi
echo "ok"

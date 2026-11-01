# shellcheck shell=bash
# Helpers for action.yml, sourced by its steps (and by action/lib_test.sh).
# Bash, but no bash-4-only features: macOS runners ship bash 3.2.

# smates_is_release_tag NAME succeeds when NAME is a release tag (v1.2.3, v1.2.3-rc.1),
# not a moving major/minor tag (v1, v1.2).
smates_is_release_tag() {
  [[ $1 =~ ^v[0-9]+\.[0-9]+\.[0-9]+([-+][0-9A-Za-z.+-]*)?$ ]]
}

# smates_tags_for_commit SHA reads a tag listing on stdin, in the format of
# `git ls-remote --tags` (<sha><TAB>refs/tags/<name>[^{}]) or `git show-ref --tags -d`
# (<sha> refs/tags/<name>[^{}]), and prints the release tags whose commit is SHA, one
# per line, sorted. An annotated tag's commit is its peeled (^{}) entry; a lightweight
# tag's is its own entry.
smates_tags_for_commit() {
  local sha
  sha=$(printf '%s' "$1" | tr 'A-F' 'a-f')
  awk -v sha="$sha" '
    $2 ~ /^refs\/tags\// {
      name = substr($2, 11)
      id = tolower($1)
      if (name ~ /\^\{\}$/) {
        peeled[substr(name, 1, length(name) - 3)] = id
      } else {
        direct[name] = id
      }
    }
    END {
      for (name in direct) {
        commit = (name in peeled) ? peeled[name] : direct[name]
        if (commit == sha) print name
      }
    }
  ' | while IFS= read -r name; do
    if smates_is_release_tag "$name"; then
      printf '%s\n' "$name"
    fi
  done | LC_ALL=C sort
}

# smates_release_for_commit SHA ACTION_PATH REMOTE prints the release tag that points
# at commit SHA. It looks at the tags of the action's own checkout first (when it is a
# git checkout of SHA), then at the remote's tags (`git ls-remote`, no API token).
# Exit status: 0 with the tag printed when exactly one release tag matches, 1 when
# none does, 2 when several do (the names are printed, one per line), 3 when the
# remote could not be listed.
smates_release_for_commit() {
  local sha=$1 path=$2 remote=$3 tags='' listing head
  if [ -n "$path" ] && [ -e "$path/.git" ]; then
    head=$(git -C "$path" rev-parse HEAD 2>/dev/null || true)
    if [ "$(printf '%s' "$head" | tr 'A-F' 'a-f')" = "$(printf '%s' "$sha" | tr 'A-F' 'a-f')" ]; then
      tags=$(git -C "$path" show-ref --tags -d 2>/dev/null | smates_tags_for_commit "$sha")
    fi
  fi
  if [ -z "$tags" ]; then
    # Local tags can be missing (checkouts without tags): ask the remote.
    if ! listing=$(GIT_TERMINAL_PROMPT=0 git ls-remote --tags "$remote" 2>/dev/null); then
      return 3
    fi
    tags=$(printf '%s\n' "$listing" | smates_tags_for_commit "$sha")
  fi
  if [ -z "$tags" ]; then
    return 1
  fi
  printf '%s\n' "$tags"
  if [ "$(printf '%s\n' "$tags" | wc -l | tr -d ' ')" != 1 ]; then
    return 2
  fi
}

smates_sha256() {
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$1" | awk '{print $1}'
  elif command -v shasum >/dev/null 2>&1; then
    shasum -a 256 "$1" | awk '{print $1}'
  else
    openssl dgst -sha256 -r "$1" | awk '{print $1}'
  fi
}

# smates_gh_can_attest succeeds when a gh CLI that can verify attestations with the
# policy flags used below is on PATH and a token is set.
smates_gh_can_attest() {
  local help flag
  command -v gh >/dev/null 2>&1 || return 1
  [ -n "${GH_TOKEN:-}${GITHUB_TOKEN:-}" ] || return 1
  help=$(gh attestation verify --help 2>&1) || return 1
  for flag in --source-ref --source-digest --signer-workflow --deny-self-hosted-runners; do
    case "$help" in
      *"$flag"*) ;;
      *) return 1 ;;
    esac
  done
}

# smates_attestation_count REPO DIGEST prints how many attestations REPO has for the
# sha256 DIGEST (0 when the API answers 404). Fails when the API cannot be reached.
smates_attestation_count() {
  local out status try
  for try in 1 2 3; do
    status=0
    out=$(gh api "repos/$1/attestations/sha256:$2" --jq '.attestations | length' 2>&1) || status=$?
    if [ "$status" -eq 0 ]; then
      printf '%s\n' "${out:-0}"
      return 0
    fi
    case "$out" in
      *"HTTP 404"*)
        echo 0
        return 0
        ;;
    esac
    if [ "$try" -lt 3 ]; then
      sleep 2
    fi
  done
  printf '%s\n' "$out" >&2
  return 1
}

# smates_verify_attestation FILE REPO SOURCE_REF SOURCE_DIGEST MODE REQUIRED checks the
# build provenance attestation of FILE, a release asset of REPO:
#   MODE      "attestation": an attestation must exist and verify; "auto": verify it
#             when gh can and the asset has one.
#   REQUIRED  "true" when the release is known to carry attestations (auto then fails
#             instead of falling back when there is none).
#   SOURCE_REF / SOURCE_DIGEST  the git ref (refs/tags/vX.Y.Z) and the commit the
#             attestation must name; empty to skip that check.
# Exit status: 0 verified; 1 not verified, but the checksum alone is acceptable (a
# notice or warning says why); 2 verification failed, or is impossible where it is
# required. The caller must never fall back to anything on 2.
smates_verify_attestation() {
  local file=$1 repo=$2 source_ref=$3 source_digest=$4 mode=$5 required=$6
  local name digest count
  local -a args
  name=$(basename "$file")
  if ! smates_gh_can_attest; then
    if [ "$mode" = attestation ]; then
      echo "::error::smates: verify: attestation needs a gh CLI with 'gh attestation verify --source-digest' (GitHub-hosted runners have one) and a token (the github-token input)"
      return 2
    fi
    echo "::notice title=smates::no gh CLI with attestation support on this runner; $name verified by checksum only"
    return 1
  fi
  digest=$(smates_sha256 "$file")
  if ! count=$(smates_attestation_count "$repo" "$digest"); then
    if [ "$mode" = attestation ] || [ "$required" = true ]; then
      echo "::error::smates: could not look up the attestations of $name (sha256:$digest) in $repo"
      return 2
    fi
    echo "::warning::smates: could not look up the attestations of $name; verified by checksum only"
    return 1
  fi
  if [ "$count" = 0 ]; then
    if [ "$mode" = attestation ]; then
      echo "::error::smates: $name (sha256:$digest) has no build provenance attestation in $repo (releases published before attestations were introduced need verify: checksum)"
      return 2
    fi
    if [ "$required" = true ]; then
      echo "::error::smates: $name (sha256:$digest) has no build provenance attestation, but this release is published with one: the asset is not what the release workflow built"
      return 2
    fi
    echo "::notice title=smates::$name has no build provenance attestation (a release published before attestations were introduced); verified by checksum only"
    return 1
  fi
  args=(attestation verify "$file" --repo "$repo"
    --signer-workflow "$repo/.github/workflows/release.yml"
    --deny-self-hosted-runners)
  if [ -n "$source_ref" ]; then
    args+=(--source-ref "$source_ref")
  fi
  if [ -n "$source_digest" ]; then
    args+=(--source-digest "$source_digest")
  fi
  echo "Verifying the build provenance of $name (built by $repo's release workflow${source_ref:+ from $source_ref}${source_digest:+ at $source_digest})"
  if ! gh "${args[@]}"; then
    echo "::error::smates: the build provenance attestation of $name does not verify (expected $repo's release workflow${source_ref:+, $source_ref}${source_digest:+, commit $source_digest})"
    return 2
  fi
}

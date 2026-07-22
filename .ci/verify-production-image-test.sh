#!/usr/bin/env bash
set -Eeuo pipefail

if [[ ${CLIPROXY_TEST_FAKE_DOCKER:-false} == true && ${0##*/} == docker ]]; then
  if [[ ${1:-} == pull && $# == 2 ]]; then
    exit 0
  fi
  if [[ ${1:-} == image && ${2:-} == inspect && ${3:-} == --format && $# == 5 ]]; then
    case $4 in
      *org.opencontainers.image.revision*)
        printf '%s\n' "${CLIPROXY_TEST_OCI_REVISION:-}"
        ;;
      '{{ .Id }}')
        printf '%s\n' 'sha256:test-image-id'
        ;;
      '{{ join .RepoDigests "," }}')
        printf '%s\n' 'ghcr.io/quqi1599/cliproxyapi@sha256:test-repo-digest'
        ;;
      *)
        printf 'unexpected docker inspect format: %s\n' "$4" >&2
        exit 2
        ;;
    esac
    exit 0
  fi
  printf 'unexpected docker invocation:' >&2
  printf ' %q' "$@" >&2
  printf '\n' >&2
  exit 2
fi

script_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
repository_root=$(cd -- "$script_dir/.." && pwd)
verifier="$repository_root/scripts/verify-production-image.sh"
fake_bin=$(mktemp -d)
trap 'rm -rf -- "${fake_bin:?}"' EXIT
ln -s "$script_dir/verify-production-image-test.sh" "$fake_bin/docker"

fail() {
  printf 'verify-production-image test failed: %s\n' "$*" >&2
  exit 1
}

expect_success() {
  description=$1
  shift
  if ! "$@" >/dev/null 2>&1; then
    fail "$description unexpectedly failed"
  fi
}

expect_failure() {
  description=$1
  expected=$2
  shift 2
  if output=$("$@" 2>&1); then
    fail "$description unexpectedly succeeded"
  fi
  [[ $output == *"$expected"* ]] || \
    fail "$description failed for an unexpected reason: $output"
}

dockerfile=$(<"$repository_root/Dockerfile")
compose=$(<"$repository_root/docker-compose.prod.yml")
[[ $dockerfile == *'org.opencontainers.image.revision="${COMMIT}"'* ]] || \
  fail "Dockerfile does not publish the COMMIT OCI revision"
[[ $compose == *'image: ghcr.io/quqi1599/cliproxyapi:sha-${CLIPROXY_COMMIT12:?'* ]] || \
  fail "production Compose does not pin CLIPROXY_COMMIT12"
[[ $compose != *CLIPROXY_IMAGE* ]] || \
  fail "production Compose still accepts CLIPROXY_IMAGE"
for workflow in docker-image.yml docker-image-debug.yml; do
  workflow_content=$(<"$repository_root/.github/workflows/$workflow")
  [[ $workflow_content == *'COMMIT="$(git rev-parse HEAD)"'* ]] || \
    fail "$workflow does not pass the full commit revision"
  [[ $workflow_content != *'git rev-parse --short HEAD'* ]] || \
    fail "$workflow still truncates the commit revision"
done

valid_tag=ghcr.io/quqi1599/cliproxyapi:sha-0123456789ab
matching_revision=0123456789ab0000000000000000000000000000
mismatched_revision=ffffffffffffffffffffffffffffffffffffffff

expect_success "immutable reference" \
  env PATH="$fake_bin:$PATH" CLIPROXY_TEST_FAKE_DOCKER=true \
  CLIPROXY_TEST_OCI_REVISION="$matching_revision" "$verifier" "$valid_tag"
expect_success "Compose commit variable" \
  env PATH="$fake_bin:$PATH" CLIPROXY_TEST_FAKE_DOCKER=true \
  CLIPROXY_TEST_OCI_REVISION="$matching_revision" CLIPROXY_COMMIT12=0123456789ab "$verifier"
expect_failure "mutable latest reference" \
  "image must use sha-<12> or an immutable sha256 digest" \
  env PATH="$fake_bin:$PATH" CLIPROXY_TEST_FAKE_DOCKER=true \
  CLIPROXY_TEST_OCI_REVISION="$matching_revision" \
  "$verifier" ghcr.io/quqi1599/cliproxyapi:latest
expect_failure "non-12-character SHA tag" \
  "image must use sha-<12> or an immutable sha256 digest" \
  env PATH="$fake_bin:$PATH" CLIPROXY_TEST_FAKE_DOCKER=true \
  CLIPROXY_TEST_OCI_REVISION="$matching_revision" CLIPROXY_COMMIT12=0123456789a \
  "$verifier"
expect_failure "non-hexadecimal Compose revision" \
  "image must use sha-<12> or an immutable sha256 digest" \
  env PATH="$fake_bin:$PATH" CLIPROXY_TEST_FAKE_DOCKER=true \
  CLIPROXY_TEST_OCI_REVISION="$matching_revision" CLIPROXY_COMMIT12=0123456789ag \
  "$verifier"
expect_failure "argument and Compose revision mismatch" \
  "image argument does not match CLIPROXY_COMMIT12" \
  env PATH="$fake_bin:$PATH" CLIPROXY_TEST_FAKE_DOCKER=true \
  CLIPROXY_TEST_OCI_REVISION="$matching_revision" CLIPROXY_COMMIT12=fedcba987654 \
  "$verifier" "$valid_tag"
expect_failure "non-40-character OCI revision" \
  "OCI revision must be the full 40-character commit SHA" \
  env PATH="$fake_bin:$PATH" CLIPROXY_TEST_FAKE_DOCKER=true \
  CLIPROXY_TEST_OCI_REVISION=0123456789ab "$verifier" "$valid_tag"
expect_failure "tag and OCI revision mismatch" \
  "does not match OCI revision" \
  env PATH="$fake_bin:$PATH" CLIPROXY_TEST_FAKE_DOCKER=true \
  CLIPROXY_TEST_OCI_REVISION="$mismatched_revision" "$verifier" "$valid_tag"

printf 'verify-production-image tests passed\n'

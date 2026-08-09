#!/usr/bin/env bash

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
DEPLOY_SCRIPT="${SCRIPT_DIR}/deploy-ghcr-release.sh"

TEST_DIR="$(mktemp -d)"
cleanup() {
  rm -rf "$TEST_DIR"
}
trap cleanup EXIT

# Load only the function under test. Sourcing the complete deployment script
# would execute its production deployment flow.
FUNCTION_FILE="${TEST_DIR}/rewrite-compose-image.sh"
sed -n '/^rewrite_compose_image_if_needed()/,/^}/p' "$DEPLOY_SCRIPT" >"$FUNCTION_FILE"
source "$FUNCTION_FILE"

assert_equal() {
  local want="$1"
  local got="$2"
  local label="$3"
  if [[ "$got" != "$want" ]]; then
    printf 'FAIL %s\nwant: %s\n got: %s\n' "$label" "$want" "$got" >&2
    exit 1
  fi
}

write_compose_fixture() {
  local target="$1"
  local image_line="$2"
  printf 'services:\n  cli-proxy-api:\n    %s\n' "$image_line" >"$target"
}

read_compose_image() {
  local target="$1"
  awk '/^[[:space:]]*image:/ {sub(/^[[:space:]]*image:[[:space:]]*/, ""); print; exit}' "$target"
}

digest_ref='ghcr.io/quqi1599/cliproxyapi@sha256:61629b4537e9f6e3910f7f46559026d1d2ee04098ee5cde31160acfb961d1f61'
COMPOSE_FILE="${TEST_DIR}/digest.yml"
write_compose_fixture "$COMPOSE_FILE" 'image: old.example/app:tag'
rewrite_compose_image_if_needed "$digest_ref"
assert_equal "$digest_ref" "$(read_compose_image "$COMPOSE_FILE")" 'digest reference remains literal'

tag_ref='ghcr.io/quqi1599/cliproxyapi:fork-v7.10.139'
COMPOSE_FILE="${TEST_DIR}/tag.yml"
write_compose_fixture "$COMPOSE_FILE" 'image: old.example/app:tag'
rewrite_compose_image_if_needed "$tag_ref"
assert_equal "$tag_ref" "$(read_compose_image "$COMPOSE_FILE")" 'tag reference remains literal'

COMPOSE_FILE="${TEST_DIR}/environment.yml"
write_compose_fixture "$COMPOSE_FILE" 'image: ${CLI_PROXY_IMAGE:-old.example/app:tag}'
original_compose="$(<"$COMPOSE_FILE")"
unset CLI_PROXY_IMAGE || true
rewrite_compose_image_if_needed "$digest_ref"
assert_equal "$digest_ref" "${CLI_PROXY_IMAGE:-}" 'environment-backed compose exports the image'
assert_equal "$original_compose" "$(<"$COMPOSE_FILE")" 'environment-backed compose remains unchanged'

printf 'PASS deploy-ghcr-release image rewrite tests\n'

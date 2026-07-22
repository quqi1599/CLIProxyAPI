#!/usr/bin/env bash
set -Eeuo pipefail

repo_root=$(git rev-parse --show-toplevel)
cd "$repo_root"

fixture_dir=$(mktemp -d)
trap 'rm -rf "$fixture_dir"' EXIT

assert_rejected() {
  local name=$1
  local source=$2
  local fixture="$fixture_dir/$name.go"
  printf '%s\n' "$source" > "$fixture"
  if bash .ci/payload-clone-scan.sh "$fixture" >/dev/null 2>&1; then
    echo "payload clone scan accepted forbidden fixture: $name" >&2
    exit 1
  fi
}

assert_allowed() {
  local name=$1
  local source=$2
  local fixture="$fixture_dir/$name.go"
  printf '%s\n' "$source" > "$fixture"
  if ! bash .ci/payload-clone-scan.sh "$fixture" >/dev/null 2>&1; then
    echo "payload clone scan rejected allowed fixture: $name" >&2
    exit 1
  fi
}

assert_rejected bytes_clone 'package fixture
import "bytes"
func clone(input []byte) []byte { return bytes.Clone(input) }'

assert_rejected slices_clone 'package fixture
import "slices"
func clone(input []byte) []byte { return slices.Clone(input) }'

assert_rejected append_clone 'package fixture
func clone(input []byte) []byte { return append([]byte(nil), input...) }'

assert_rejected append_empty_clone 'package fixture
func clone(input []byte) []byte { return append([]byte{}, input...) }'

assert_rejected make_copy_clone 'package fixture
func clone(input []byte) []byte {
	out := make([]byte, len(input))
	copy(out, input)
	return out
}'

assert_allowed centralized_clone 'package fixture
import internalpayload "github.com/router-for-me/CLIProxyAPI/v7/internal/payload"
func clone(input []byte) []byte { return internalpayload.CloneBytes(input) }'

assert_allowed reasoned_exception 'package fixture
import "slices"
func clone(input []string) []string { return slices.Clone(input) } //nolint:payload-clone reason=non_payload_slice'

if ! bash .ci/payload-clone-scan.sh "$fixture_dir/deleted.go" >/dev/null 2>&1; then
  echo "payload clone scan rejected a deleted source path" >&2
  exit 1
fi

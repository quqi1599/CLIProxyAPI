#!/usr/bin/env bash
set -Eeuo pipefail

if ! command -v go >/dev/null 2>&1; then
  exec docker run --rm \
    --volume "$PWD:/workspace:ro" \
    --workdir /workspace \
    golang:1.26-bookworm \
    bash -lc 'git config --global --add safe.directory /workspace && .ci/ci.sh'
fi

unformatted=$(git ls-files -z '*.go' | xargs -0 gofmt -l)
if [[ -n $unformatted ]]; then
  echo "gofmt required for:" >&2
  echo "$unformatted" >&2
  exit 1
fi

go test ./internal/payload
go test ./...
go vet ./...

build_output=$(mktemp)
trap 'rm -f "$build_output"' EXIT
go build -o "$build_output" ./cmd/server

go test ./internal/runtime/executor \
  -run '^$' \
  -bench '^BenchmarkPatchCodexCompletedOutput$' \
  -benchmem \
  -benchtime=100ms

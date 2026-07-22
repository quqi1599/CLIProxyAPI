#!/usr/bin/env bash
set -Eeuo pipefail

if ! command -v go >/dev/null 2>&1; then
  docker_bin=$(command -v docker)
  exec "$docker_bin" run --rm \
    --user "$(id -u):$(id -g)" \
    --env HOME=/tmp/ci-home \
    --env GOCACHE=/tmp/go-build \
    --env GOMODCACHE=/tmp/go-mod \
    --volume "$PWD:/workspace" \
    --workdir /workspace \
    golang:1.26-bookworm \
    bash -c 'mkdir -p "$HOME" && git config --global --add safe.directory /workspace && .ci/ci.sh'
fi

go_files=()
while IFS= read -r -d '' file; do
  if [[ -f $file ]]; then
    go_files+=("$file")
  fi
done < <(git ls-files -z -co --exclude-standard -- '*.go')

unformatted=
if ((${#go_files[@]} > 0)); then
  unformatted=$(gofmt -l "${go_files[@]}")
fi
if [[ -n $unformatted ]]; then
  echo "gofmt required for:" >&2
  echo "$unformatted" >&2
  exit 1
fi

go run ./cmd/payload-growth -test=false ./...
bash .ci/payload-clone-scan-test.sh
bash .ci/payload-clone-scan.sh
bash .ci/verify-production-image-test.sh
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

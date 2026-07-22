#!/usr/bin/env bash
set -Eeuo pipefail

repo_root=$(git rev-parse --show-toplevel)
cd "$repo_root"

files=()
while IFS= read -r -d '' file; do
  files+=("$file")
done < <(git ls-files -z -co --exclude-standard -- \
  '*.go' \
  ':!**/*_test.go' \
  ':!examples/**' \
  ':!cmd/payload-soak/**' \
  ':!internal/payload/growthlint/testdata/**')

if ((${#files[@]} == 0)); then
  exit 0
fi

direct_clones=$(grep -nHE 'bytes[.]Clone[[:space:]]*[(]' "${files[@]}" || true)
violations=$(printf '%s\n' "$direct_clones" | grep -vE '//nolint:payload-clone reason=[[:alnum:]_-]+' || true)
if [[ -n $violations ]]; then
  echo 'direct bytes.Clone calls must use internal/payload.CloneBytes or a reasoned small-value exception:' >&2
  echo "$violations" >&2
  exit 1
fi

clone_helpers=$(grep -nHE 'func[[:space:]]+cloneBytes[[:space:]]*[(]' "${files[@]}" || true)
unexpected_helpers=$(printf '%s\n' "$clone_helpers" | grep -v '^sdk/api/handlers/handlers.go:' || true)
if [[ -n $unexpected_helpers ]]; then
  echo 'local cloneBytes helpers bypass the centralized payload clone contract:' >&2
  echo "$unexpected_helpers" >&2
  exit 1
fi

if [[ -n $clone_helpers ]] && ! awk '
  /func cloneBytes\(src \[\]byte\) \[\]byte/ {
    getline
    if ($0 ~ /return internalpayload[.]CloneBytes[(]src[)]/) {
      delegated = 1
    }
  }
  END { exit delegated ? 0 : 1 }
' sdk/api/handlers/handlers.go; then
  echo 'sdk/api/handlers cloneBytes must delegate to internal/payload.CloneBytes' >&2
  exit 1
fi

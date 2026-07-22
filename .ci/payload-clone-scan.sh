#!/usr/bin/env bash
set -Eeuo pipefail

repo_root=$(git rev-parse --show-toplevel)
cd "$repo_root"

requested_files=("$@")
files=()
if ((${#requested_files[@]} == 0)); then
  while IFS= read -r -d '' file; do
    if [[ -f $file ]]; then
      files+=("$file")
    fi
  done < <(git ls-files -z -co --exclude-standard -- \
    '*.go' \
    ':!**/*_test.go' \
    ':!examples/**' \
    ':!cmd/payload-soak/**' \
    ':!internal/payload/clone.go' \
    ':!internal/payload/growthlint/testdata/**')
else
  for file in "${requested_files[@]}"; do
    if [[ -f $file ]]; then
      files+=("$file")
    fi
  done
fi

if ((${#files[@]} == 0)); then
  exit 0
fi

direct_clones=$(grep -nHE \
  -e 'bytes[.]Clone[[:space:]]*[(]' \
  -e 'slices[.]Clone[[:space:]]*[(]' \
  -e 'append[[:space:]]*[(][[:space:]]*\[\]byte[[:space:]]*[(][[:space:]]*nil[[:space:]]*[)][[:space:]]*,' \
  -e 'append[[:space:]]*[(][[:space:]]*\[\]byte[[:space:]]*[{][[:space:]]*[}][[:space:]]*,' \
  "${files[@]}" || true)
violations=$(printf '%s\n' "$direct_clones" | grep -vE '//nolint:payload-clone reason=[[:alnum:]_-]+' || true)
if [[ -n $violations ]]; then
  echo 'payload byte clones must use internal/payload clone helpers or a reasoned non-payload exception:' >&2
  echo "$violations" >&2
  exit 1
fi

make_copy_clones=$(awk '
  FNR == 1 {
    make_line = 0
    make_text = ""
  }
  /make[[:space:]]*[(][[:space:]]*\[\]byte[[:space:]]*,[[:space:]]*len[[:space:]]*[(]/ {
    make_line = FNR
    make_text = $0
    next
  }
  make_line > 0 && FNR <= make_line + 4 && /copy[[:space:]]*[(]/ {
    print FILENAME ":" make_line ":" make_text
    make_line = 0
    make_text = ""
    next
  }
  make_line > 0 && FNR > make_line + 4 {
    make_line = 0
    make_text = ""
  }
' "${files[@]}")
make_copy_violations=$(printf '%s\n' "$make_copy_clones" | grep -vE '//nolint:payload-clone reason=[[:alnum:]_-]+' || true)
if [[ -n $make_copy_violations ]]; then
  echo 'make+copy byte clones must use internal/payload clone helpers or a reasoned non-payload exception:' >&2
  echo "$make_copy_violations" >&2
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

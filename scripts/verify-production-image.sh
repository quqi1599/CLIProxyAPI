#!/usr/bin/env bash
set -Eeuo pipefail

image=${1:-}

fail() {
  echo "production image verification failed: $*" >&2
  exit 1
}

if [[ -n ${CLIPROXY_COMMIT12:-} ]]; then
  compose_image="ghcr.io/quqi1599/cliproxyapi:sha-${CLIPROXY_COMMIT12}"
  if [[ -n $image && $image != "$compose_image" ]]; then
    fail "image argument does not match CLIPROXY_COMMIT12 used by docker-compose.prod.yml"
  fi
  image=$compose_image
fi

if [[ -z $image ]]; then
  fail "provide an image argument or CLIPROXY_COMMIT12"
fi

tag_commit=""
if [[ $image =~ ^ghcr\.io/quqi1599/cliproxyapi:sha-([0-9a-f]{12})$ ]]; then
  tag_commit=${BASH_REMATCH[1]}
elif [[ ! $image =~ ^ghcr\.io/quqi1599/cliproxyapi@sha256:[0-9a-f]{64}$ ]]; then
  fail "image must use sha-<12> or an immutable sha256 digest"
fi

command -v docker >/dev/null 2>&1 || fail "docker is required"
docker pull "$image" >/dev/null

revision=$(docker image inspect --format '{{ index .Config.Labels "org.opencontainers.image.revision" }}' "$image")
if [[ ! $revision =~ ^[0-9a-f]{40}$ ]]; then
  fail "OCI revision must be the full 40-character commit SHA"
fi
if [[ -n $tag_commit && ${revision:0:12} != "$tag_commit" ]]; then
  fail "sha tag $tag_commit does not match OCI revision $revision"
fi

image_id=$(docker image inspect --format '{{ .Id }}' "$image")
repo_digests=$(docker image inspect --format '{{ join .RepoDigests "," }}' "$image")
printf 'image=%s\nrevision=%s\nimage_id=%s\nrepo_digests=%s\n' \
  "$image" "$revision" "$image_id" "$repo_digests"

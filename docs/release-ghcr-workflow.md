# Release and GHCR Deployment Workflow

This fork is intended to use a pull-based production deployment flow:

1. Merge validated code into `main`
2. Create and push a Git tag like `fork/v7.10.43`
3. GitHub Actions builds release binaries and an amd64 Docker image
4. Production servers pull the published GHCR image
5. Restart the service without building on the production host

## Workflows

- `.github/workflows/docker-image.yml`
  - Triggered by pushing `fork/v*` tags
  - Publishes `linux/amd64` images to `ghcr.io/quqi1599/cliproxyapi`
  - Also supports manual reruns through `workflow_dispatch`
  - Manual reruns can optionally refresh the `latest` image tag
- `.github/workflows/release.yaml`
  - Triggered by the same `fork/v*` tags
  - Publishes GitHub Release artifacts for all supported platforms

## Published Image Tags

For a release tag like `fork/v7.10.43`, the Docker workflow publishes:

- `ghcr.io/quqi1599/cliproxyapi:fork-v7.10.43`
- `ghcr.io/quqi1599/cliproxyapi:v7.10.43`
- `ghcr.io/quqi1599/cliproxyapi:7.10.43`
- `ghcr.io/quqi1599/cliproxyapi:7.10`
- `ghcr.io/quqi1599/cliproxyapi:7`
- `ghcr.io/quqi1599/cliproxyapi:latest` on tag push

These release tags remain available for compatibility. The default
`docker-compose.yml` keeps its local build and mutable image fallback for
development only; production must use the SHA-pinned compose file below.

For compatibility with existing tag-based deployments, the legacy helper
continues to use the development compose file and report readiness timestamps:

```bash
./scripts/deploy-ghcr-release.sh fork/v7.10.43
```

The script prints:

- `old_container_stop_at`
- `new_container_start_event_at`
- `port_8317_listen_at`
- `healthz_ok_at`
- `first_success_request_at`
- `first_failed_request_at`
- `last_failed_request_at`
- `connection_refused_request_count`

## Production Deployment

The current production host is `x86_64`, so the release workflow only builds `linux/amd64`.
If you later add ARM servers, you can reintroduce `linux/arm64` to the Docker workflow.

Use the `sha-<12>` image produced by `ci-builder`. `CLIPROXY_COMMIT12` is
required and must contain exactly 12 lowercase hexadecimal characters, so
Compose fails before deployment when no candidate revision is supplied:

```bash
cd /opt/cliproxy
export CLIPROXY_COMMIT12=7822c9e37aed
./scripts/verify-production-image.sh
docker compose -f docker-compose.prod.yml config --quiet
docker compose -f docker-compose.prod.yml config --images
docker compose -f docker-compose.prod.yml pull
docker compose -f docker-compose.prod.yml up -d --no-build
docker compose -f docker-compose.prod.yml ps
docker compose -f docker-compose.prod.yml logs --tail 20 cli-proxy-api
curl -fsS http://127.0.0.1:8317/livez
```

The preflight rejects mutable tags, pulls the exact candidate, requires a full
40-character `org.opencontainers.image.revision` label, and verifies that a
`sha-<12>` tag matches that revision. Before opening traffic, run the 12-hour
release gate with the same full revision; it continuously verifies
`/healthz/details.build.commit` and fails on a restart or revision change.

The production defaults are an 8 GiB memory hard limit, a 6 GiB
`GOMEMLIMIT`, 4 CPUs with `GOMAXPROCS=4`, 1024 PIDs, no container swap, and a
60-second stop grace period. If the memory limit changes, keep `GOMEMLIMIT` at
approximately 70%-80% of it and keep the memory and memory-plus-swap limits
equal to leave swap disabled.

The corresponding overrides are `CLIPROXY_MEMORY_LIMIT`,
`CLIPROXY_MEMORY_SWAP_LIMIT`, `CLIPROXY_GOMEMLIMIT`, `CLIPROXY_CPUS`,
`CLIPROXY_GOMAXPROCS`, and `CLIPROXY_PIDS_LIMIT`.

## Rollback

Rollback is the same process with an older image tag:

```bash
export CLIPROXY_COMMIT12=0123456789ab # replace with the previous revision
./scripts/verify-production-image.sh
docker compose -f docker-compose.prod.yml pull
docker compose -f docker-compose.prod.yml up -d --no-build
```

## Debug Image And pprof

The regular release image stays stripped for normal production use. When you need
symbolized CPU or heap profiles, trigger the manual workflow:

- Workflow: `.github/workflows/docker-image-debug.yml`
- Input tag: `fork/v7.10.43`
- Published tag: `ghcr.io/quqi1599/cliproxyapi:fork-v7.10.43-debug`

`pprof` accepts only `localhost` or a loopback IP. Each enable or re-apply starts
a fixed 30-minute diagnostic window; the listener then shuts down automatically.
Enable it in `config.yaml`:

```yaml
pprof:
  enable: true
  addr: 127.0.0.1:8316
```

After the configuration reloads, verify that only the loopback listener and
the expected endpoint are available:

```bash
ss -ltnp | grep '127.0.0.1:8316'
curl -fsS http://127.0.0.1:8316/debug/pprof/ >/dev/null
```

Collect the bounded profiles you need before the 30-minute TTL expires:

```bash
go tool pprof -o /tmp/cliproxy-heap.pb.gz http://127.0.0.1:8316/debug/pprof/heap
go tool pprof -o /tmp/cliproxy-allocs.pb.gz http://127.0.0.1:8316/debug/pprof/allocs
go tool pprof -o /tmp/cliproxy-cpu.pb.gz 'http://127.0.0.1:8316/debug/pprof/profile?seconds=30'
curl -fsS 'http://127.0.0.1:8316/debug/pprof/goroutine?debug=1' \
  -o /tmp/cliproxy-goroutine.txt
```

Disable `pprof` immediately after collection rather than waiting for the TTL:

```yaml
pprof:
  enable: false
  addr: 127.0.0.1:8316
```

After the reload, both checks must show that the listener is gone:

```bash
! ss -ltn | grep -q '127.0.0.1:8316'
! curl -fsS --max-time 2 http://127.0.0.1:8316/debug/pprof/
```

## Local Developer Builds

Use local builds only for development verification:

```bash
./docker-build.sh
```

Then choose `2) Build from Source and Run (For Developers)`.

If you want to run a published release locally, keep the default option and set:

```bash
export CLI_PROXY_IMAGE=ghcr.io/quqi1599/cliproxyapi:fork-v7.10.43
./docker-build.sh
```

# Payload Soak Release Gate

`cmd/payload-soak` drives the required mixed payload profile against an isolated staging CLIProxyAPI instance. It sends 90% small, 9% medium, and 1% large requests containing generated message history, tool output, reasoning, and inline-image data. Medium and large requests set `reasoning_effort=high`, and the release gate requires the `thinking_history.synthetic_budget` policy counter to increase after preflight. This prevents a run from passing without exercising the July 21 thinking-history normalization path. No customer payload is used or written to disk.

Start the deterministic fake upstream in a separate process:

```bash
go run ./cmd/payload-soak-upstream \
  -listen 127.0.0.1:18317 \
  -api-key payload-soak-upstream
```

Use this isolated staging configuration for the candidate. Do not add real providers or credentials to this instance:

```yaml
host: "127.0.0.1"
port: 8317
remote-management:
  allow-remote: false
  secret-key: "payload-soak-management"
auth-dir: "./payload-soak-auth"
api-keys:
  - "payload-soak-client"
local-model: true
request-retry: 0
max-retry-credentials: 1
disable-cooling: true
request-guards:
  global-admission:
    enabled: true
    capacity: 128
    max-queue: 64
    max-wait-seconds: 30
openai-compatibility:
  - name: "payload-soak"
    base-url: "http://127.0.0.1:18317/v1"
    disable-cooling: true
    api-key-entries:
      - api-key: "payload-soak-upstream"
    models:
      - name: "payload-soak-model"
        alias: "payload-soak-model"
```

Run the exact release-candidate image with that config, then run the gate for at least 12 hours:

```bash
export CPA_SOAK_API_KEY='payload-soak-client'
export CPA_SOAK_MANAGEMENT_KEY='payload-soak-management'
export CPA_SOAK_MODEL='payload-soak-model'
export CPA_SOAK_EXPECTED_COMMIT='<full 40-character commit SHA>'
export CPA_SOAK_BASE_URL='http://127.0.0.1:8317'
.ci/payload-soak.sh
```

The generated large requests exercise proxy transforms and may not contain a semantically valid image for a production model. The fake upstream never forwards requests and stores no payloads.

The script always enables release-gate, chaos, and Responses WebSocket modes. It rejects durations shorter than 12 hours and requires the full expected commit SHA. The preflight and every later authenticated `/healthz/details` sample must report that exact commit, so a wrong image or an in-place revision change fails the run. The complete matrix runs before load to catch a bad staging setup quickly and again after load; the same resource-recovery gate runs immediately after each matrix.

| Scenario | Required observation |
| --- | --- |
| `slow_upstream` | 2xx marker response; response headers arrive after at least 200 ms |
| `http_half_close` | Controlled downstream 500/502 error envelope |
| `http_reset` | Controlled downstream 500/502 error envelope |
| `rate_limit_429` | Downstream 429 error envelope; this expected non-2xx does not fail the workload counter |
| `upstream_5xx` | Downstream 503 error envelope; this expected non-2xx does not fail the workload counter |
| `bad_gzip` | Downstream 502 decode error |
| `malformed_sse` | Terminal HTTP 200 stream error or bootstrap HTTP 502 error envelope; never a completion-only stream |
| `downstream_stream` | Successful downstream SSE path with data plus `[DONE]` or `response.completed` |
| `client_mid_stream_cancel` | First downstream stream bytes arrive, then the test client closes the body |
| `responses_ws_connect` | `/v1/responses` upgrades with HTTP 101 and accepts a clean close |
| `responses_ws_idle` | An idle connection remains usable and completes a later request |
| `responses_ws_frame` | At least two JSON frames arrive and terminate with `response.completed` |
| `responses_ws_cancel` | At least one frame arrives before the test client closes the WebSocket |

Every scenario reports its expected outcome, attempts, successes, failures, last status/category, and header/end-to-end latency. Release mode requires all 13 scenarios in both matrix runs; a missing scenario is a hard failure.

The command exits nonzero on early SIGINT/SIGTERM, when any normal workload request exceeds its two-minute client deadline, when an unexpected request is non-2xx, when a bounded response exceeds 16 MiB, or when `/livez` or `/readyz` fails its five-second probe deadline. Scenario-specific non-2xx responses pass only when they match the table above. A valid run must complete the configured duration and successfully exercise every small, medium, and large profile. Every 30 seconds it also samples the authenticated `/healthz/details?gc=1` endpoint so heap readings represent a fresh post-GC snapshot; a missing sample, process start/revision change, or details failure fails the gate. After chaos/load stops, it closes idle workload connections, then probes for up to two minutes and requires admission to drain plus goroutines, live heap, and every platform-available RSS/file-descriptor/socket metric to return to its preflight baseline with bounded headroom. The JSON report includes first/last/max resources, per-resource trends after a short warm-up, and exact recovery limits without exposing either key. Workload `average_latency_ms` and `maximum_latency_ms` are end-to-end values measured after the response body is read and closed; separate `*_headers_latency_ms` fields retain time-to-headers.

For a local smoke test only, invoke the command directly with `-release-gate=false` and a shorter `-duration`. Chaos and Responses WebSocket checks remain enabled by default; they may be disabled explicitly only in non-release mode. This path is not release evidence.

The release passes only when:

- there are no OOMs or restarts;
- post-GC heap has no sustained positive slope after warm-up;
- post-GC heap, goroutines, and available RSS/file-descriptor/socket counts independently avoid sustained growth and return within their reported recovery limits;
- readiness and liveness remain healthy;
- the running image uses the exact `sha-<12>` candidate built by `ci-builder`, and `/healthz/details` reports its matching full 40-character commit SHA.

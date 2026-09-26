# Independent request admission dimensions

Global admission remains an explicit opt-in. This change is local code; enabling
it on a production server requires a separately reviewed configuration rollout.

## Resource lifetimes

| Dimension | Unit and default | Acquire / release |
| --- | --- | --- |
| Body | Decoded input bytes; 512 MiB total | Known identity bodies reserve before reading; unknown/encoded bodies transfer to measured bytes before JSON inspection. Retained until execution and nested children finish. No queue while holding unaccounted input. SDK and WebSocket execution entry points also reserve bytes. |
| Read | Existing conservative weighted read budget; inherits `capacity` unless `read-capacity` is set | Unknown/encoded ingress only; rejects immediately on saturation, releases after decoding/parsing, independent of body residency and upstream slots. |
| Transform | 16 preparation units | Before decode/inspection, provider request planning, guarded legacy translation, or compatibility/SDK pipelines; releases before upstream network waiting. Nested synchronous helpers share one scope. |
| Execution | 128 slots | Only execution entry points acquire slots. Bodies at or below 16 MiB cost 1; larger bodies cost `4 * ceil(bytes / 32 MiB)`. Held through the response/stream; nested execution shares a lease, concurrent siblings add their weights. |
| Queue | Bounded waiter count and elapsed wait budget | Execution and transform have separate queues, counters, and maxima. A request shares a cumulative queue-wait budget across sequential stages/retries. Waiting never installs an upstream context deadline. |

Byte admission accounts for logical input residency, not all temporary copies,
compressed input, transformed output, or total process RSS. Unknown/encoded input
allocations are initially protected by the read pool and the physical body
ceiling. A measured body that cannot fit the byte pool is rejected without
waiting. Multipart accounting is conservative and includes file bytes even when
files spill to disk. A body larger than the configured byte capacity is rejected,
never silently clamped to the capacity. Existing leases survive configuration
reload; reducing a limit does not revoke already-running work.

Preparation uses size buckets: <=1 MiB costs 1 unit, <=16 MiB costs 2, >16 MiB
costs 8. Each bucket keeps a bounded elapsed-time EWMA. If it exceeds one second,
subsequent work in that bucket costs twice as much. The estimate excludes admission
waiting and upstream execution. It measures **wall time**, including local
preparation dependencies, not per-request OS CPU time. A transform is synchronous;
this mechanism limits concurrent expensive work rather than preempting Go code.
Scopes larger than the configured transform capacity run exclusively. Read and
execution weights likewise saturate at their pool capacity for tiny deployments.

The independent pools and cumulative queue budgets do not guarantee a global FIFO
order. Each weighted queue retains the existing light/heavy aging policy. A local
transform rejection is a request-scoped 503; it is not an upstream health failure
and must not trigger automatic provider retries. Request cancellation is preserved.

## Configuration

```yaml
request-guards:
  global-admission:
    enabled: true
    capacity: 128
    body-capacity-bytes: 536870912
    read-capacity: 128
    transform-capacity: 16
    max-queue: 64
    max-wait-seconds: 30
    transform-max-queue: 64
    transform-max-wait-milliseconds: 1000
    saturation-grace-seconds: 5
```

The meaning of `capacity` changes from a mixed complexity score to execution slots.
Review existing enabled configurations before rolling out. Zero/omitted new values
use the defaults above; `read-capacity` inherits `capacity`, and
`transform-max-queue` inherits `max-queue`. Admission defaults disabled, preserving
existing server opt-in behavior.

## Amplification and scan invariants

The existing amplification contract remains:

`output <= input + max(256 KiB, ceil(input * 25%))`

Thus a default-bound rejection requires both excessive absolute growth and
excessive proportional growth. Tiny requests can receive a small fixed protocol
preamble without a ratio-only false positive. Named policy overrides and
observe/enforce mode retain their existing semantics. No blanket 2x rejection was
introduced. ASCII and normalized-text candidate scanning in content audit remains
unchanged; no-hit text avoids full segmentation and candidate scans remain zero-copy.

## Observability

The authenticated `/healthz/details` response exports:

- `admission.body`, `.read`, `.transform`, `.execution`: independent capacity,
  active usage, queue depth, queue-wait buckets and rejection counters. Body/read
  acquisition is immediate; their queue depth stays zero.
- `transforms.slow_reports_over_one_second`: one count for a completed request
  report whose recorded transform work exceeds one second, including failed work.
- `transforms.preparation.over_one_second` and `.large_over_one_second`: actual
  outer preparation scopes exceeding one second, excluding queue time and nested
  helper double counting. These count scopes, not unique requests.
- `transforms.preparation.duration_buckets`, `.nanoseconds`, `.scopes`.
- `payload_transform_summary.transform_over_one_second`: a searchable slow-report
  flag separate from total request/stream latency.

Readiness considers sustained saturation in every pool. Liveness and health remain
independent. These admission counters contain no prompt, body, user or credential
labels. The subsequent remediation adds a separate bounded route/model/provider
metric collection; see [server remediation](server-remediation-20260927.md).

## Verification

Behavior tests cover known/unknown ingress without execution occupancy, strict
body-byte rejection before reading, >16 MiB weight transitions, elapsed feedback
isolated by size, nested/fanout lifetime, cancellation, queue bounds/expiration,
configuration shrink/disable, concurrent byte accounting, and actual provider
execution releasing transform capacity before upstream IO. The slow-report and
scope counters have exact one-second boundary and exactly-once tests.

The admission benchmark compares enabled/disabled control overhead with 8 KiB,
16 MiB, 17 MiB and 64 MiB admission estimates at `-cpu=1,2`. It does not allocate or
transform those payloads and must not be described as an end-to-end speedup or
production load test. Candidate-scan benchmarks exercise real ASCII/normalized
text and report allocations. Use synthetic payloads, not production prompts, for
any later end-to-end load test.

## Local benchmark evidence (2026-09-27)

Host: darwin/arm64, Apple M5; two repetitions, 200 ms per case.
The admission measurements below are scope/control overhead only.

| Case | 1 worker (ns/op) | 2 workers (ns/op) | B/op | allocs/op |
| --- | ---: | ---: | ---: | ---: |
| 8 KiB estimate, disabled | 157-160 | 162-164 | 192 | 6 |
| 8 KiB estimate, enabled | 213-220 | 320-321 | 240 | 8 |
| 64 MiB estimate, disabled | 169-172 | 165-184 | 192 | 6 |
| 64 MiB estimate, enabled | 239-245 | 340-358 | 240 | 8 |

Real candidate scans remained zero-allocation: ASCII 22.0-22.3 us/op
(351-355 MB/s), normalized text 46.3-46.6 us/op (116-117 MB/s).
Their `-cpu=1,2` cases are serial scans with different scheduler settings;
the admission benchmark uses `RunParallel` for actual 1/2-worker contention.

Reproduce:

```sh
go test ./sdk/api/handlers -run '^$' -bench '^BenchmarkAdmissionDimensions$' -benchmem -benchtime=200ms -count=2 -cpu=1,2
go test ./internal/contentaudit -run 'TestMatcherCandidateFastPathsAvoidNormalizationAllocation|TestMatcherIsSafeForConcurrentRequests' -bench '^BenchmarkMatcherCandidateScanFastPaths$' -benchmem -benchtime=200ms -count=2 -cpu=1,2
```

# CPA remediation and measurements — 2026-09-27

## Production evidence and changes

The inspected production container is still revision
`c00e6adc5269ad7eb88ae7e35481244ef9dc5fec`, image digest
`sha256:4ef10a851f5f1b2cb3093213d65678e3770fe0e5000061fe5b4a65bc2f2b7b79`.
Local code changes below have not been released. No production load test was run.
All active upstream tests used tiny synthetic operator prompts, sequentially.

The expired Codex OAuth credential was matched to the failing runtime auth index,
backed up privately under `/opt/cliproxy/backups/codex-oauth-quarantine-20260926T194016Z/`,
then disabled through its watched JSON file. Its tokens and other fields were not
modified. This quarantines the repeated 401 candidate; it does not mint a new OAuth
grant. Restoring that grant requires the account owner's interactive login.

All three real CPA `POST /v1/responses` probes returned HTTP 200 with
`status=completed` and nonempty output, through the existing configured API-key
routes. Logs confirmed the selected route and successful attempt for each probe.

| Model | Before quarantine | After quarantine |
| --- | ---: | ---: |
| gpt-5.5 | 2.236 s | 10.845 s |
| gpt-5.6-luna | 22.064 s | 11.879 s |
| gpt-6-luna | 2.672 s | 1.471 s |

These single-request timings prove availability at those instants, not a latency
improvement or SLO. No arbitrary alias to a different model was introduced.
The post-change file readback confirms `disabled=true`, and each of the three
post-change requests had exactly one ready candidate in the runtime selection log.

The complete retained main-log set (126 files, none lost during reading) covers
2026-09-26 03:46:05 through 2026-09-27 04:00:37 as recorded by the server. There are
413,607 HTTP log entries: 406,388 status 200; 2,513 status 503; 1,722 status 422;
911 status 502; 794 status 499; 274 status 500; 143 status 429; and 135 status 403.
This is approximately 24 hours, not 32 hours. The duration parser includes ns/us/ms,
seconds, minutes and hours; p50 is 3.594 s, p95 28.672 s and max 1,548 s across all
logged routes. Management/probe traffic is included, so this is not an inference
business-success-rate denominator. Earlier estimates using only second-formatted
durations understated errors and long requests.

## Provider failures

- The prominent `provider=claude`, `glm-5.3-flash` 429s originate at the Zhipu
  Anthropic-compatible endpoint. The executor's protocol name does not identify
  Anthropic as the vendor. A current direct GLM request succeeded (HTTP 200).
- Kimi historical 403 credentials were retested with the configured endpoint and
  headers: K2.7 text/tools, K2.6 text and K3 text succeeded. There is no demonstrated
  persistent entitlement failure on these sampled credentials now. Kimi documents
  concurrent and time-window usage limits as possible 403 causes; without the
  historical error text those remain hypotheses, not confirmed causes.
- The same DeepSeek credential appearing in 422 logs passed text, tool-definition,
  and streaming requests with 32,768/65,536 output limits. A real CPA Responses
  request for `deepseek-v4.1-flash` also completed with HTTP 200. Historical 422
  metadata mostly shows `max_tokens,messages,model,stream,stream_options`, including
  both chat and Messages ingress. Error bodies are deliberately stored as hashes
  and sizes, so the exact invalid parameter cannot be recovered from those logs.
  No speculative model disablement or silent request rewrite was applied.

Reference: [Kimi Code error reference](https://www.kimi.com/code/docs/en/kimi-code/error-reference.html).
DeepSeek's [thinking contract](https://api-docs.deepseek.com/guides/thinking_mode/)
requires preserving reasoning content in tool history. Our basic tool-definition
probe does not establish compatibility of every multi-turn tool history.

## Local implementation

- An unresolved provider route now returns HTTP 404, with an explicit missing-route
  message and OpenAI `model_not_found` classification. The exact Haiku model and
  all three protocols are covered for streaming and non-streaming requests.
  Configured routes with unavailable credentials retain their existing semantics.
- `/healthz/details` now includes `route_metrics`, protected by the existing
  management authentication. Labels are endpoint, requested model and executor
  provider. There are at most 512 ordinary series plus one overflow series, bounded
  labels and a fixed stage set; no bodies, user IDs or credentials are labels.
- Request HTTP outcomes and upstream attempt outcomes are separate counters,
  including 503/429/403/499. A downstream cancellation is counted as request 499;
  a retry's provider error is not confused with the final client-visible outcome.
  `upstream_http_by_status` additionally preserves raw HTTP exchange status before
  executor error normalization (for example raw 403 versus normalized 429).
- Fixed histograms collect outer transform scope time, synchronous audit evidence
  write time (including encryption/compression/SQLite), selection, upstream HTTP
  headers, first semantic streamed token and total request duration. Histogram
  counts are non-cumulative; bounds are exported in milliseconds, sums in ns.
  Shared ingress/retry preparation costs are attributed to the final provider;
  attempt outcomes remain attributed to the provider of each attempt.
- First-token time starts at ingress and ignores role envelopes and heartbeats.
  It recognizes content/reasoning/tool-argument deltas for the three supported
  protocols. Non-streaming JSON has no fabricated token timing. A 64 KiB bounded
  SSE line scanner skips oversized lines rather than retaining unbounded output.
- Upstream-header time includes connection, request upload and server wait. It
  excludes streaming body consumption and is not pure provider compute time.
  HTTP transport instrumentation covers proxy and cached uTLS clients without
  mutating shared clients or adding upstream timeouts. WebSocket network timing
  is not part of this HTTP metric surface.
- Body bytes, unknown-body reading, transformation and execution admission remain
  independent. See [admission dimensions](admission-dimensions.md). Production has
  not enabled this local implementation.
- Spread shutdown now waits for an in-flight snapshot, then persists the latest
  state. Previously a concurrent snapshot could cause the final flush to be skipped.

## Reproduction and interpretation

```sh
go test ./internal/api -run '^$' -bench BenchmarkRoutePipelineMatrix -benchtime=2x -cpu=1,2
go test ./sdk/cliproxy/auth -run '^$' -bench BenchmarkSpreadRouteState -benchtime=100ms -cpu=1
```

The route matrix covers 8 KiB / 1 MiB / 16 MiB / 64 MiB, all three routes, and
Codex / Claude / DeepSeek / MiniMax provider preparation. It executes real ingress,
audit policy extraction, encrypted SQLite persistence, Spread selection, executor
conversion and response translation. Only the upstream is simulated on loopback,
with a 2 ms delay. One and two Go workers are compared with two measured operations
per cell; these are minimal diagnostic samples, not capacity certification.

Payloads are synthetic ASCII single-user messages with an audit marker at both
ends. Audit extraction retains bounded evidence (200,000 runes per string), so
audit write time does not represent persisting the entire 64 MiB input. The CSV
records actual body bytes, which include the JSON envelope. `B/op` is cumulative
allocation, not live heap or peak RSS. `ns/op` is benchmark wall time per operation;
`total-ns/op` is mean request latency and can be about twice `ns/op` at two workers.

### Measured matrix

All 96 cells completed successfully, with 192 measured requests and zero 499/503.
Warmup requests are excluded from those 192 observations. The 8 KiB cells were
rerun separately after other local checks stopped. Host: Apple M5, darwin/arm64.
Full results: [96-cell CSV](route-pipeline-matrix-20260927.csv).

| Payload | Transform (ms) | Audit write (ms) | Selection (ms) | Total latency (ms) | Allocated GiB/request |
| --- | ---: | ---: | ---: | ---: | ---: |
| 8 KiB | 0.20–0.63 | 0.18–0.76 | 0.014–0.048 | 7.18–10.10 | 0.005–0.055 |
| 1 MiB | 22.47–86.50 | 1.15–7.33 | 0.49–6.93 | 163.28–256.97 | 0.107–0.185 |
| 16 MiB | 351.38–1171.60 | 10.76–22.17 | 7.62–31.69 | 879.10–1921.17 | 0.472–1.474 |
| 64 MiB | 1402.44–4756.02 | 41.37–98.32 | 30.49–134.25 | 2991.30–7376.74 | 1.722–5.660 |

Ranges include all routes/providers and one/two workers. This is a real pipeline
comparison against a simulated upstream, not evidence of production throughput or
provider latency. The warmup initializes some process-wide components; allocation
profiles include initialization and setup, while benchmark B/op excludes setup.

A focused 64 MiB Codex Responses profile identified repeated `gjson.getBytes`
(1,152 MB) and `sjson.set`/`appendRawPaths` (1,472 MB cumulative), URL parsing/escaping
(432 MB), and full `[]rune` allocation in audit extraction (256 MB in each of two
call sites). CPU samples concentrate in JSON string scanning. These profile totals
include setup and must not be substituted for per-operation figures in the table.

Priority follow-ups supported by this evidence:

1. Reuse parsed spans and batch JSON field mutations, avoiding repeated full-body
   GJSON copies and SJSON rebuilds. Preserve protocol/tool-history semantics.
2. In audit extraction, detect a URL scheme before calling `url.Parse` on arbitrary
   megabyte text. Locate UTF-8 prefix/suffix boundaries without constructing a full
   rune array solely to retain the final 200,000 runes.
3. Cache request-shape inspection across route selection; the full selection stage
   grows with payload size despite having a single benchmark credential.
4. Validate memory peaks under the production memory limit before choosing body and
   transform capacity. Logical body admission is not a guarantee on temporary RSS.

### Spread decision

Recent live selection logs show common route populations of 2–31 candidates, not
hundreds per route. The current weighted-deficit selector measured 2.736 us at 8
candidates, 24.497 us at 64, 94.179 us at 256 and 401.805 us at 1,024. Copying a
snapshot of 16 routes measured 4.377/35.116/129.321/496.520 us at those sizes;
these snapshot-copy numbers exclude serialization, fsync and disk write latency.

A prebuilt static Fenwick lookup measured 2.37–8.44 ns, but that microbenchmark
does not include eligibility, health/load recomputation, updates, locking or
weighted-deficit fairness. It is not an equivalent selector and does not establish
an end-to-end speedup. Existing dynamic weights require scanning candidates; adding
a tree without changing that contract leaves the main O(N) work intact.

Keep route-scoped weighted deficit with five-second, request-triggered asynchronous
snapshots and a correct final shutdown flush. A timer-driven dirty flush could make
idle-route durability stricter if that is required. Reconsider Fenwick only if
per-route populations and measured selection CPU become material, with eligibility
and weight updates made incremental and fairness tested against the current policy.

## Database maintenance boundary

The current SQLite file is 23,279,435,776 bytes, with roughly 17.75 GB (16.5 GiB)
of free pages. Deleting retained rows alone cannot shrink it with `auto_vacuum=0`.
The prepared [maintenance script](../scripts/maintain-audit-db.py) defaults to a
read-only inventory. Its explicit apply mode verifies the image and bind mount,
stops CPA, checkpoints WAL, uses `VACUUM INTO`, verifies integrity and every table's
row values by hash, retains the original through a hard link, atomically replaces
the database, and restarts CPA even if compaction fails. A readiness check is
followed by scoped business probes. It never deletes backups.

The original database retained for rollback still occupies disk space: reducing
the active file is not the same as immediate net disk reclamation. The maintenance
window and rollback-retention policy must be explicit. SQLite documents
[VACUUM INTO](https://www.sqlite.org/lang_vacuum.html) as a consistent compact copy;
the service must be stopped during final replacement to avoid losing newer writes.

There is no verified backup expiration policy. The two large historical backups
are the 2026-08-23 file (7,816,011,776 bytes) and 2026-09-05 file
(20,591,996,928 bytes). A specific maintenance-window and deletion choice has been
requested; neither compaction nor backup deletion has run as of this report.

## Validation and delivery state

- `go test ./...` passed. Targeted race checks passed for route metrics, HTTP
  middleware, API routes, content audit, payload admission, executor transports,
  API handlers and Spread snapshot lifecycle.
- Server entrypoint build, the repository payload-growth checker and
  `git diff --check` passed. The final raw-status metric addition received another
  targeted race check and entrypoint build.
- An offline SQLite fixture verified smaller output, identical logical content
  and an intact original database for rollback. The production maintenance script
  was run only in its read-only planning mode.
- Existing working-tree changes were preserved. No commit, push, CI run, image
  publication or application code deployment was performed during this task.
  Local checks are not production release proof.
- Raw sanitized evidence and profiles are under
  `/tmp/cpa-remediation-20260927/`. The report and CSV are the durable repository
  deliverables; production credentials and customer payloads are not included.

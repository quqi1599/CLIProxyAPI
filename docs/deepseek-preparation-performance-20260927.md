# DeepSeek request preparation: bounded field views

Date: 2026-09-27. Baseline: `327093ff91541acb854014b8851fc70a689f3619`.
This document records implementation-stage validation. Deployment evidence is
tracked separately in the release record.

## Evidence and compatibility constraints

The earlier production audit traced request reference `9e742916c4c0` to
`DeepSeek-官方` / `api.deepseek.com`: requested `deepseek-v4.1-flash`, actual
model `deepseek-flash`, `/v1/chat/completions`, 19,070,361 input bytes, 4,719 ms transformation time.
A read-only SSH log scan during this task confirmed that route and the same trace.
At pre-release verification, the live container ran the baseline SHA and was healthy.
Pre-quirk cleanup accounted for 1,310.254 ms and the DeepSeek policy for
2,195.074 ms. These are previously collected server measurements, not a replay
of the customer's request or a measurement of the new implementation in production.

GBrain's development log and September 16 compatibility record describe earlier
endpoint-specific repairs. The current code and regression tests remain the
behavioral authority: preserve explicit thinking-off, forced tool selection,
namespace round trips, real reasoning history, native Responses capabilities,
beta strict schemas, request-scoped errors, and validation after payload overrides.
The optimization does not introduce retries, fabricate reasoning, or change the
canonical thinking representation and provider applier architecture.

An initial 16 MiB Responses CPU/allocation profile reproduced the local cost:
1.242 s/request and 1,456.8 MiB allocated/request. `newPayloadJSONValue` accounted
for about 51.5% of sampled allocated space; whole-body JSON parsing, validation,
and sequential sjson mutations dominated CPU. The sample used synthetic data.

## Design

`helps.JSONFieldView` indexes top-level fields once per view, constructs
a small document containing a rule group's declared read/write fields, and merges
the result into the original body with at most one full output allocation.
The existing compatibility rules operate on this small document. The index is
request-local; there is no cross-request body cache.

The implementation applies to:

- DeepSeek forced-tool/thinking normalization after the canonical thinking applier.
- The DeepSeek provider policy's control fields on Chat, Responses and compact.
- DeepSeek capability and tool-schema cleanup, including Chat.
- DeepSeek Responses/compact pre-quirk cleanup and post-override revalidation.
- DeepSeek Chat tool normalization when function names do not change.

Chat history processing still sees its complete message history. Tool normalization
first processes tools and tool choice in a small view; a function-name change
falls back to the complete legacy traversal so every historical reference is
rewritten. A schema-only edit preserves messages verbatim. The diagnostic
`modified_fields` consequently stops reporting incidental message key reordering
as a message change; a regression test now asserts this behavior.
Responses `input`, instructions and other unrelated values stay opaque to
control-only rules. Mixed requests that also supply `messages` retain the
existing message cleanup. Tool-name changes retain the legacy downgrade report.

No-op merges return the original slice. Unrelated raw values preserve exact
numbers, escapes, nested formatting and field order. Duplicate top-level keys,
invalid/non-object JSON, or undeclared writes fall back to the original path.
Malformed string escapes are repaired on the complete body before pre-quirk
projection, so they cannot be hidden by the view. The helper uses a safe gjson
string copy; it is not an unsafe zero-copy parser.

Phase reporting and amplification/admission checks remain outside the view and
receive complete body lengths. Views are discarded after their phase, so a
payload override cannot leave a stale index behind.

## Alternatives measured

The isolated DeepSeek policy benchmark compares the old full-body sequence,
a decode-once `map[string]json.RawMessage` candidate, and the selected field view.
Three measured iterations, one worker, synthetic Responses input:

| Content | Strategy | ms/op | MiB allocated/op |
| --- | --- | ---: | ---: |
| 1 MiB | Legacy sequence | 13.218 | 27.231 |
| 1 MiB | RawMessage map | 5.665 | 2.365 |
| 1 MiB | Field view | 1.013 | 2.028 |
| 16 MiB | Legacy sequence | 190.260 | 432.231 |
| 16 MiB | RawMessage map | 93.712 | 37.366 |
| 16 MiB | Field view | 15.984 | 32.028 |

The RawMessage candidate decodes/copies the whole prompt and re-marshals the
entire map, sorting fields. The field view preserves unrelated raw bytes and
performed better in this comparison. These policy-only numbers are not complete
request timings.

## Complete preparation benchmark

Environment: Go 1.26.0, darwin/arm64, Apple M5, `GOMAXPROCS=2` via `-cpu=2`.
Inputs contain a large synthetic content field before controls, a function
schema with `required:null`, forced tool selection and reasoning controls.
Sizes below describe content; complete bodies are slightly larger.

The old and new binaries ran sequentially with no concurrent build/test process.
Each cell has two measured operations plus Go's warm-up; this is a minimal
comparison, not a capacity estimate or statistical latency distribution.
For two workers, ns/op is aggregate wall time divided by completed operations,
not the latency of each concurrent request.

| Route | Content | Workers | Before ms/op | After ms/op | Before MiB/op | After MiB/op |
| --- | --- | ---: | ---: | ---: | ---: | ---: |
| Responses | 8 KiB | 1 | 0.690 | 0.489 | 0.89 | 0.20 |
| Responses | 8 KiB | 2 | 0.408 | 0.224 | 0.90 | 0.20 |
| Responses | 1 MiB | 1 | 60.021 | 29.912 | 91.78 | 16.18 |
| Responses | 1 MiB | 2 | 33.761 | 17.448 | 91.78 | 16.18 |
| Responses | 16 MiB | 1 | 1,033.517 | 480.728 | 1,456.78 | 256.18 |
| Responses | 16 MiB | 2 | 514.726 | 257.647 | 1,456.78 | 256.18 |
| Responses | 64 MiB | 1 | 4,086.790 | 2,111.156 | 5,888.79 | 1,088.19 |
| Responses | 64 MiB | 2 | 2,115.234 | 1,200.427 | 5,888.79 | 1,088.19 |
| Chat | 8 KiB | 1 | 0.638 | 0.406 | 0.71 | 0.31 |
| Chat | 8 KiB | 2 | 0.655 | 0.391 | 0.72 | 0.31 |
| Chat | 1 MiB | 1 | 70.010 | 41.891 | 71.61 | 28.27 |
| Chat | 1 MiB | 2 | 37.386 | 22.525 | 71.61 | 28.27 |
| Chat | 16 MiB | 1 | 1,042.992 | 646.989 | 1,136.61 | 448.27 |
| Chat | 16 MiB | 2 | 533.928 | 338.683 | 1,136.61 | 448.27 |
| Chat | 64 MiB | 1 | 4,264.200 | 2,699.923 | 4,608.62 | 1,856.27 |
| Chat | 64 MiB | 2 | 2,264.335 | 1,777.461 | 4,608.62 | 1,856.27 |

Raw numeric results: [CSV](deepseek-preparation-matrix-20260927.csv).
Process peak RSS across the identical complete matrix, measured with macOS
`/usr/bin/time -l`, was 2,088.8 MiB before and 1,061.6 MiB after. This includes
fixtures, both routes and runtime retention; it is not a per-request RSS number.
Allocated bytes are cumulative allocations, not simultaneously resident memory.

No upstream traffic, audit writes or HTTP handling are included. Consequently
upstream waiting, audit latency, 499/503 rates and production throughput are not
measured by this benchmark. No production pressure traffic was generated.

## Validation

- 1,944 differential policy cases across four DeepSeek model names, three
  endpoints, stable/beta URLs, thinking controls, aliases and tool shapes.
- Pre-quirk and post-config comparisons, malformed escape repair and duplicate
  field fallback; complete body sizes remain visible to phase accounting.
- Exact preservation of opaque raw values, no-op storage reuse, undeclared-write
  rejection, and concurrent Chat/Responses isolation over shared source bytes.
- 30-second field-view fuzz run: 511,214 executions, no failures.
- `go test ./...` passed (87 packages with tests).
- `go test -race ./internal/runtime/executor/helps ./internal/runtime/executor` passed.
- `go run ./cmd/payload-growth ./internal/runtime/executor/...` passed.
- `go build -buildvcs=false -o <temporary-directory>/server ./cmd/server` passed.

These are local checks. A future release still needs the repository's trusted
CI, immutable image publication, deployment and real provider acceptance checks.

## Reproduction and remaining costs

For the baseline, use a detached checkout of the baseline SHA and copy only
`internal/runtime/executor/deepseek_preparation_benchmark_test.go` from this
change into it. That file is self-contained against the baseline. Build separate
test binaries with `go test -c ./internal/runtime/executor -o <version>.test`.
Run each binary sequentially:

```sh
./before.test -test.run='^$' -test.bench='^BenchmarkDeepSeekPreparation$' -test.benchtime=2x -test.cpu=2 -test.count=1
./after.test -test.run='^$' -test.bench='^BenchmarkDeepSeekPreparation$' -test.benchtime=2x -test.cpu=2 -test.count=1
go test ./internal/runtime/executor -run '^$' -bench '^BenchmarkDeepSeekPolicyStrategies$' -benchtime=3x -cpu=2
```

Unprofiled matrix results above are the main comparison. Separate first-pass profiled
16 MiB runs measured 1.242 s/1,456.8 MiB before and 0.506 s/272.2 MiB after;
profiling and run-to-run effects make their time values differ from the matrix.
Local raw logs/profiles are under `/tmp/cpa-deepseek-redesign-20260927/`.

The final Chat profile still shows repeated general validation/scanning (gjson
validation accounts for approximately 30.6% of sampled CPU) and diagnostic
top-level RawMessage copies. Chat also retains full message/history processing
and falls back to complete tool processing when a name changes. These are
separate follow-up candidates; this change preserves the
compatibility phase boundaries rather than bypassing those checks. Namespace
flattening, native Claude preparation, routing and admission configuration were
not redesigned in this batch.

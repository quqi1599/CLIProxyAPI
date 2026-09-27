# Content audit bounded scans and pending recovery index

Measured on 2026-09-27. Scope: the first three requested optimizations only. This report records pre-release local validation; release status is tracked separately. No production data or upstream traffic was used in these benchmarks.

## Implementation and preserved behavior

- Evidence prefix/suffix extraction now scans a bounded UTF-8 window and copies only the retained fragment. Evidence display retains the prefix; current enforcement text and continuation references retain the suffix. The limits remain 200,000 and 4,096 runes respectively. Short byte strings return immediately; a bounded standard-library rune count prevents a short-Unicode regression found in the first experiment. Truncated fragments are cloned so they cannot retain a large backing string. Invalid UTF-8 keeps the exact previous per-invalid-byte replacement behavior, and untruncated strings remain byte-for-byte unchanged.
- URL classification checks the five-byte case-insensitive `data:` prefix, then a valid ASCII scheme and `://`, before calling `net/url.Parse`. Arbitrary valid schemes, host validation, Unicode hosts, and malformed-URL rejection remain supported. Ordinary text no longer undergoes whole-string lowercasing or URL parsing. The byte search remains O(N) in the worst case; this is not a constant-time classifier.
- A partial B-tree index on `created_at` contains only `model_review_mode='shadow' AND model_review_fallback='shadow_pending'`. It is created after legacy review-column migrations. The existing recovery UPDATE and cutoff semantics are unchanged. `EXPLAIN QUERY PLAN` verifies that it selects the pending-only index. Selection visits eligible pending records instead of all historical records; terminal review writes remove their entries automatically.

For a retention limit K and input length N, evidence scanning is bounded by O(min(N, 4K)) bytes, plus copying retained runes. The short-text precheck does not scan arbitrarily large inputs. Whole-request JSON decoding, evidence serialization, and other audit processing remain proportional to the full input.

## Measurement method

- Apple M5, 32 GiB RAM, macOS 27.0; Go 1.26.0 darwin/arm64; modernc.org/sqlite v1.44.3.
- Parent revision: `5f2aa9e7ab08ade4f7e47ec9de54aa3a0e941439`. The complete extraction control was compiled from `git archive` of that revision with the identical public-entrypoint benchmark fixture added.
- All timing suites ran sequentially after test/build work finished. No stress traffic was sent to production. Reported values are medians of three runs, not statistical confidence bounds or predicted production speedups.
- Function scans: `-benchtime=100ms -count=3 -cpu=1,2`; ASCII and mixed UTF-8, nominal 8 KiB / 1 MiB / 16 MiB / 64 MiB. The legacy suffix microbenchmark omits the old extra repeated rune count, making it a conservative control. Short valid URL and data-URI controls retain fixed short inputs under each size group; only ordinary-text fixtures scale with those labels.
- Full audit extraction: `/v1/chat/completions`, one synthetic user message repeating `Plain TEXT `; `-benchtime=5x -count=3 -cpu=1,2`. Sizes refer to nominal prompt bytes, with small JSON-envelope overhead. Fixtures are shared and immutable. Each worker independently extracts and serializes its result. Two-worker `ns/op` is aggregate time per completed operation, not individual request latency.
- Recovery: file-backed WAL SQLite, the real schema and `RecoverInterruptedShadowReviews`, 512-byte synthetic evidence per historical row, 0 or 32 pending rows, `-benchtime=5x -count=3 -cpu=1`. Fixture setup, checkpoint, and resetting pending rows are outside the timer. The timed call includes its commit. The old control drops only the new index. These are warm OS-cache measurements, not cold production-disk results. Recovery is startup-only and intentionally has no artificial concurrent-writer benchmark.
- Peak RSS: `/usr/bin/time -l` on separate benchmark-binary processes, five timed operations per process, three fresh processes per case. RSS includes runtime, fixture construction, pools, and GC retention; it is not per-request memory. Concurrent fixtures share input bytes, so these measurements cannot determine admission limits for distinct simultaneous uploads. `B/op` measures cumulative Go allocation, not peak RSS.

## Function-level comparison

Single worker; all timings below are microseconds. Allocation figures include amortized benchmark-worker overhead.

| Operation | Old µs/op | New µs/op | Old B/op | New B/op |
|---|---:|---:|---:|---:|
| 8KiB ASCII, keep first 200k runes | 2.068 | 0.002 | 0 | 0 |
| 1MiB ASCII, keep first 200k runes | 1817.411 | 123.002 | 4,399,106 | 204,800 |
| 16MiB ASCII, keep first 200k runes | 17634.820 | 112.057 | 67,313,690 | 204,800 |
| 64MiB ASCII, keep first 200k runes | 70419.646 | 107.387 | 268,640,336 | 204,800 |
| 8KiB UTF-8, keep last 4096 runes | 4.964 | 4.673 | 0 | 0 |
| 64MiB UTF-8, keep last 4096 runes | 120952.833 | 17.693 | 76,718,240 | 14,336 |
| 8KiB mixed-case ordinary text URL check | 46.382 | 0.107 | 32,912 | 0 |
| 1MiB mixed-case ordinary text URL check | 5298.204 | 10.504 | 3,915,927 | 0 |
| 16MiB mixed-case ordinary text URL check | 75185.854 | 183.685 | 62,537,944 | 0 |
| 64MiB mixed-case ordinary text URL check | 293027.000 | 929.329 | 250,134,816 | 1 |

## Complete audit extraction

These numbers include the existing JSON parser, prompt traversal, evidence sanitizer and serializer; they exclude routing, provider conversion, network waits, encryption, and audit database insertion.

| Prompt | Workers | Old ms/op | New ms/op | Old MiB allocated/op | New MiB allocated/op | Old → new allocations/op |
|---|---:|---:|---:|---:|---:|---:|
| 8KiB | 1 | 0.438 | 0.173 | 0.141 | 0.046 | 100 → 86 |
| 8KiB | 2 | 0.208 | 0.120 | 0.150 | 0.055 | 103 → 89 |
| 1MiB | 1 | 26.330 | 8.384 | 26.031 | 2.982 | 126 → 90 |
| 1MiB | 2 | 15.488 | 5.646 | 26.031 | 5.289 | 125 → 100 |
| 16MiB | 1 | 322.902 | 85.555 | 372.492 | 32.982 | 127 → 90 |
| 16MiB | 2 | 200.610 | 54.913 | 372.492 | 39.500 | 125 → 95 |
| 64MiB | 1 | 1258.859 | 310.790 | 1485.211 | 128.982 | 127 → 90 |
| 64MiB | 2 | 799.629 | 199.636 | 1485.211 | 180.417 | 125 → 94 |

## Shadow recovery and migration cost

| Historical rows | Pending rows | Old µs/op | Indexed µs/op | Old B/op | Indexed B/op |
|---:|---:|---:|---:|---:|---:|
| 1,000 | 0 | 300.650 | 21.825 | 360 | 360 |
| 1,000 | 32 | 427.866 | 140.758 | 456 | 456 |
| 100,000 | 0 | 37800.459 | 23.150 | 360 | 360 |
| 100,000 | 32 | 37898.692 | 103.633 | 360 | 360 |

On the 100,000-history / 32-pending fixture, rebuilding the index took **20.83 ms**, with **4,096 bytes** of index pages (`dbstat`). This first creation scans existing rows once; its cost is not included in the recovery timings. Production database size and storage differ, so the next release must allow for this one-time startup migration. Existing databases are not modified by this local task. The migration is idempotent and compatible with absent legacy review columns.

## Peak resident memory

MiB; median followed by the full range of three fresh-process observations. All extraction rows use 64 MiB prompts.

| Case | Old peak RSS | New peak RSS |
|---|---:|---:|
| Prefix extraction, one worker | 855.7 (855.7–855.8) | 87.4 (87.4–87.9) |
| Ordinary-text URL check, one worker | 366.8 (366.2–366.9) | 86.5 (86.4–86.6) |
| 100k history / 32 pending recovery | 29.3 (29.2–29.4) | 29.4 (29.3–30.2) |
| Complete audit extraction, one worker | 1096.0 (1095.3–1096.1) | 551.7 (551.6–552.7) |
| Complete audit extraction, two workers | 1502.8 (1205.7–1526.8) | 680.2 (538.6–1194.0) |

The recovery index primarily removes scan/IO work; its RSS remains essentially unchanged. Two-worker extraction RSS varies considerably with scheduling, JSON buffer reuse, and GC. Use the measured range, not just the median; this small experiment does not establish production capacity or justify enabling admission automatically.

## Validation and reproduction

Passed:

- `go test ./...` on the final production implementation.
- `go test -race ./internal/contentaudit` (72.410 s on the final implementation).
- Differential UTF-8 fuzzing: 117,615 executions initially, then 72,460 after the short-text optimization. Differential URL fuzzing: 50,629 executions. All match the previous behavior; finite fuzz runs are not exhaustive proofs.
- Deterministic invalid-UTF-8, multibyte, combining-character, exact-boundary, random-byte and URL grammar cases.
- Large prefix/current-tail/continuation-reference assertions for Chat Completions, Responses and Messages. The same boundary test passes against the parent revision.
- Index query plan, legacy review-column migration, idempotent migration, cutoff inclusivity, future and terminal record preservation, repeated recovery and unchanged encrypted evidence.
- Server build to a task-local temporary directory and `git diff --check`.

Re-run the checked-in benchmarks:

```bash
go test ./internal/contentaudit -run '^$' -bench '^Benchmark(EvidenceWindows|URLPrefilter)$' -benchmem -benchtime=100ms -count=3 -cpu=1,2
go test ./internal/contentaudit -run '^$' -bench '^BenchmarkAuditExtraction$' -benchmem -benchtime=5x -count=3 -cpu=1,2
go test ./internal/contentaudit -run '^$' -bench '^BenchmarkShadowRecovery$' -benchmem -benchtime=5x -count=3 -cpu=1
go test ./internal/contentaudit -run '^$' -bench '^BenchmarkShadowPendingIndexBuild$' -benchmem -benchtime=3x -count=3 -cpu=1
```

For the full-extraction baseline, archive the parent revision into a scratch directory and add only `auditBenchmarkSizes` and `BenchmarkAuditExtraction` from `extract_benchmark_test.go`, with their imports. This harness uses only the unchanged public entrypoint. Compile both with `go test -c`; invoke them sequentially with identical `-test.bench`, `-test.benchtime`, `-test.count` and `-test.cpu` flags. For RSS, prefix each separate invocation with `/usr/bin/time -l`.

Raw benchmark output, aggregate CSV/JSON, process-RSS samples, test logs and the sequential driver are retained under `/tmp/cpa-audit-optimization-20260927-aqduJm/`. These local temporary artifacts may expire; the checked-in benchmark fixtures and this report are the durable reproduction record.

## Scope of the next batch

Request-local metadata reuse / combined JSON rewrites, avoiding doomed provider retries, and isolated admission calibration remain separate follow-up optimizations. This batch changes neither provider selection nor quotas. Spread/Fenwick remains deferred; these measurements concern audit extraction and pending-review recovery only. Production release, cloud CI and live business verification require the release stage and are not claimed here.

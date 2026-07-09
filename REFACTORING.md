# Refactoring CLIProxyAPI Safely

This repository is a compatibility gateway. Reliability comes from preserving provider-specific behavior while keeping routing, retries, error classification, transport, and observability as separate concerns. A full rewrite would put those behaviors at risk; the codebase should be reduced through staged, contract-tested migrations.

## Baseline

Before Phase 0 on 2026-07-10:

- The repository contains 816 Go files across 103 packages.
- The fork is hundreds of commits ahead of its upstream merge base and carries roughly 65,000 added lines.
- The largest production files are `sdk/cliproxy/auth/conductor.go`, `internal/runtime/executor/claude_executor.go`, `internal/usage/monitor_queries.go`, and `internal/runtime/executor/openai_compat_profile.go`.
- The full test suite passes, but several concurrency and lifecycle paths have little or no direct coverage.

## Audit findings

| Priority | Finding | Direction |
| --- | --- | --- |
| P0 | Stream interceptors could mutate a returned header map concurrently with downstream readers. | Commit headers before returning the stream. |
| P0 | The websocket relay silently discarded chunks after an eight-message pending queue filled. | Isolate requests and terminate an overflowing request explicitly. |
| P0 | A closed data channel could win a `select` over a buffered upstream error, replacing a real 429 or 5xx with a local 408. | Treat data and error channels as one composite stream termination. |
| P1 | Built-in scheduling still scans and allocates once per credential on the request path. | Add a route-aware invalidated index, then differential-test it against current selection semantics. |
| P1 | Usage finalization starts untracked goroutines and plugin streaming copies request context for every chunk. | Add bounded workers and allocation benchmarks before changing copy semantics. |
| P1 | Request-body limits are inconsistent across compression and protocol paths. | Enforce one post-decompression limit in the shared body reader. |
| P1 | Failure meaning is repeatedly inferred from strings in executors, auth coordination, and API handlers. | Classify a typed failure once at the executor boundary. |
| P2 | `Manager`, Claude execution, monitoring queries, and OpenAI compatibility profiles are oversized orchestration units. | Split behind existing public APIs only after behavior matrices exist. |
| P2 | OAuth flow, provider identity, transport setup, and reasoning replay storage contain substantial duplication. | Share private infrastructure while preserving provider wrappers. |

## Phase 0 delivered

- Reduced the tree to 815 Go files and 102 packages.
- Removed more code than was added, including one direct dependency, a never-constructed usage mirror, obsolete logging builders, unused internal DTOs and packages, and unreachable executor helpers.
- Consolidated duplicate mixed-provider request and count execution loops without changing routing or retry policy.
- Fixed stream-header publication, websocket error ordering, relay overflow behavior, context ownership, and unsafe `sync.Map` test cleanup.
- Added direct websocket-relay coverage and made management and watcher tests race-safe.
- Preserved the external SDK, plugin ABI, translator architecture, retry budgets, provider routing, and thinking pipeline.

## Non-negotiable invariants

1. Preserve the `canonical ThinkingConfig -> provider applier` architecture in `internal/thinking`.
2. Do not change routing, retry budgets, cooldown scope, or credential eviction in a mechanical cleanup commit.
3. Do not log raw credentials, prompts, request bodies, or provider secrets to diagnose compatibility failures.
4. Once an upstream connection is established, do not add transport timeouts outside the documented exceptions in `AGENTS.md`.
5. Keep the legacy string classifiers until every migrated failure has a typed fallback contract.
6. Keep external SDK and plugin ABI compatibility unless a versioned migration is explicitly approved.

## Target ownership boundaries

| Area | Owns | Must not own |
| --- | --- | --- |
| API handler | Protocol parsing and downstream response writing | Provider retry and credential health decisions |
| Translator | Deterministic protocol conversion | Network calls, global cache mutation, retry policy |
| Executor | One provider exchange and provider-native failure classification | Cross-credential fallback policy |
| Auth coordinator | Credential/model selection, retry budget, cooldown and eviction | Provider payload rewriting and client-facing error prose |
| Usage and logging | Bounded persistence and redacted diagnostics | Request routing decisions |
| Management API | Input validation and service calls | Storage-specific aggregation logic |

## Highest-priority architecture work

### 1. Classify failures once

Introduce a typed executor failure contract with at least:

- `Kind`
- `Scope` (`request`, `model`, `credential`, or `provider`)
- `HTTPStatus`
- `ProviderCode`
- `RetryAfter`
- `Retryable`
- `Cause`

Executors classify provider responses once. The auth coordinator decides retry, cooldown, and eviction from `Kind + Scope`. API handlers only map the typed failure to a client response. During migration, unknown failures must fall back to the existing string classifiers.

### 2. Turn Manager into a facade

Keep the public `Manager` API, but move implementation into package-private components:

- `AttemptRunner`: one provider/auth/model attempt
- `RetryCoordinator`: the only cross-model and cross-credential state machine
- `ResultRecorder`: health, cooldown, breaker, and registry updates
- `AuthRepository`: CRUD and persistence
- `HomeDispatcher`: Home-specific session and credential dispatch

Execute, CountTokens, and Stream must share a behavior matrix before their remaining control flow is consolidated.

### 3. Keep one built-in scheduler

The scheduler and legacy selectors currently implement overlapping availability and strategy semantics. Add differential tests for priority, cooldown, mixed providers, pinned auth, aliases, model pools, routing groups, Sequential Fill, and websocket preference. Migrate one strategy at a time, then remove its legacy path.

### 4. Resolve provider identity once

Create a pure `ResolveProviderIdentity(Auth)` function that returns canonical provider, executor key, provider family, compatibility name, and compatibility kind. Service registration, auth selection, config synthesis, and model registration should consume the same result.

### 5. Bound asynchronous work

The following paths need explicit queue, backpressure, overflow, and close semantics:

- websocket relay pending-request mailboxes
- usage finalization and stream-summary writes
- plugin stream bridges
- request body reads and decompression

Silent chunk drops, unbounded goroutines, and writes to already returned maps are not acceptable failure modes.

## Compatibility change admission rule

Every new provider or model workaround must include all of the following:

1. An exact fixture: provider, model, protocol, endpoint, status, provider code, and redacted response shape.
2. A declared owner layer: translator, executor, auth policy, API response, or observability.
3. A declared failure scope: request, model, credential, or provider.
4. Tests for every affected execution mode: non-stream, count, stream, and websocket where applicable.
5. A retry statement: retryable or terminal, retry budget consumed or not, cooldown target, and eviction behavior.
6. Redacted diagnostics sufficient to distinguish the failure without raw request logging.
7. A removal condition or upstream capability that makes the workaround obsolete.

Provider-specific substring checks must not be added to `conductor.go` or generic API handlers unless they are a documented temporary fallback for a typed failure migration.

## Staged plan

### Phase 0: remove proven waste and restore guardrails

- Delete unreachable internal code and obsolete mirror paths.
- Remove duplicate native/dependency wrappers.
- Consolidate exact duplicate execution loops without changing semantics.
- Fix deterministic races and context-lifecycle leaks.
- Make `go test ./...`, `go vet ./...`, the required server build, and targeted race tests mandatory.

### Phase 1: establish contracts

- Add typed failures with legacy fallback.
- Add provider identity resolution.
- Split executor optional capabilities without breaking the existing composite interface.
- Add cross-mode failure and retry contract tests.

### Phase 2: reduce hot-path duplication

- Remove the scheduler pre-scan that allocates once per credential, after differential tests exist.
- Consolidate request/count/stream attempt handling behind the same retry coordinator.
- Extract Claude compatibility, tool-name, cloaking, and cache-control policy from transport code.
- Share Codex and Antigravity reasoning replay storage.

### Phase 3: simplify edges

- Share Claude and Codex OAuth flow infrastructure behind provider-specific wrappers.
- Introduce a storage-independent monitor reader.
- Move management orchestration out of Gin handlers.
- Gradually replace implicit global translator registration with explicit registries.

## Verification matrix

Every refactoring batch must run:

```bash
gofmt -w <changed-go-files>
go test ./...
go vet ./...
go build -o test-output ./cmd/server && rm test-output
```

Concurrency-sensitive changes must also run targeted `go test -race` loops. Routing changes require old/new differential tests and benchmark comparisons with logging disabled for the benchmark.

The Phase 0 batch passes the full ordinary test suite, `go vet`, the required server build, and race testing for every package except `internal/store`. The remaining `internal/store` race is inside the pinned go-git v6 pack transport: its handshake reads a shared stderr buffer before its copy goroutine finishes. Non-git store tests pass under the race detector. Upgrade or isolate that dependency in a separate compatibility-tested change rather than hiding the detector result here.

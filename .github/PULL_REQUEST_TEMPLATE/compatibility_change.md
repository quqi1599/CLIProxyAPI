---
name: Compatibility change
about: Add or update a provider or model compatibility policy
title: "[compat] "
labels: ""
assignees: ""
---

## Compatibility change

Do not include credentials, raw prompts, reasoning, or unredacted provider responses.

### Match

- Provider family:
- Compatibility kind:
- Models or pattern:
- Source format:
- Target format:
- Endpoint:
- Modes: non-stream / stream / count / compact / websocket

### Evidence and ownership

- Observed upstream behavior:
- Redacted fixture path:
- Owner layer: translator / executor / auth policy / API response / observability
- Policy ID:
- Policy phase:
- Owner:

### Failure and retry contract

- Failure kind:
- Failure scope: request / model / credential / provider
- Retryable:
- Consumes retry budget:
- Cooldown target:
- Credential eviction:
- Redacted diagnostic fields:

### Cost contract

- Fixture input bytes:
- Expected output bytes:
- Maximum expansion bytes or ratio:
- Complexity: O(1) / O(n) / O(bytes)
- May copy large fields:
- Benchmark result or reason not applicable:

### Lifecycle and release

- Removal condition:
- Review date or upstream version condition:
- Canary cohort:
- Rollback signals:

### Verification

- [ ] The policy has owner, cost, removal, fixture, evidence, retry, and review metadata.
- [ ] The redacted fixture covers every affected execution mode.
- [ ] The transform is idempotent and preserves unknown fields.
- [ ] Diagnostics contain no secrets, prompts, reasoning, or raw payloads.
- [ ] Targeted compatibility tests pass.
- [ ] Full tests, vet, and the required server build pass.

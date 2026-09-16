# Request reliability and terminal accounting

This change addresses repeated capacity failures, slow GPT fallback, known
DeepSeek route incompatibility, and misleading completion diagnostics observed
in production on September 16–17, 2026.

- Context-limit fallback for the existing Sonnet/GLM aliases skips credentials
  sharing the same upstream and model pool. Distinct model pools remain eligible.
  The request-wide context fallback budget is four attempts, including the first.
  Ordinary invalid requests and content-policy failures keep their existing rules.
- GPT spread selection defers routes with recent first-event failures when a
  healthy alternative exists. Existing two-minute observations expire naturally;
  an entirely degraded pool remains eligible for recovery. After 180 seconds of
  accumulated first-event wait, a new streaming attempt is not admitted. This
  check neither changes connection deadlines nor interrupts active output.
- Retry admission counters cover actual execution/first-event acquisition.
  Eligible failover remains immediate even when the observational permit limit
  is exceeded. `retry_permit_fail_open`, `retry_permit_queued`, and
  `retry_permit_rejected` distinguish admission decisions; congestion alone does
  not mean a request was throttled.
- Stream success is published after upstream and downstream completion.
  `stream_execution_summary` includes `summary_phase=terminal`, `http_status`,
  `final_status`, `final_success`, and `terminal_outcome`. An error after a stop
  marker overrides that marker. Output that has already started is never replayed.
- Successful transform logs retain compact totals and sample full stage details
  at one percent. Execution failures, incomplete reports, amplification anomalies,
  debug logging, and transformations taking at least 100 ms retain stage details.
- DeepSeek Responses filters known incompatible Coding routes before selection
  while preserving explicit route pins and actionable errors for a sole route.
  Forced tools retain their selection and use the provider's supported thinking
  controls; unsupported tools are not silently removed.
- Production Compose includes bounded Docker log rotation. When updating an
  existing production Compose file, preserve its network, ports, mounts and
  credentials, and apply the existing 8 GiB memory, 6 GiB Go memory target,
  four CPU and 1024 PID protection values to the target service only.

## Release verification scope

Use exact-SHA trusted CI and an immutable GHCR image. Targeted tests cover
31 credentials sharing two upstreams, a successful larger-capacity alternative,
attempt limits, route recovery, immediate failover under saturation, HTTP 200
followed by a stream error, and duplicate downstream completion. A concurrent
64-stream success/failure drain checks terminal accounting and stream tracking.
Run affected package and race checks before publication. These bounded existing
controller/lifecycle changes do not introduce a new long-lived worker or cache;
use the short production observation profile unless testing exposes a remaining
duration-dependent resource risk. A 12-hour soak is not represented as completed.

Save the previous Compose/image and protected configuration hashes, apply only
the intended service update, and verify revision, resources, health, auth,
bounded signed functional requests, terminal fields, and fresh production logs.
Provider errors and application regressions must remain separately classified.

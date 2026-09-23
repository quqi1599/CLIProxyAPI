# Long Responses tool-history routing contract

## Capability is not availability

`codex-api-key[].native-responses` is an optional boolean declaring that a route
has been verified to carry native Responses and complex, long tool histories.
This is independent of WebSocket support, priority, current health, and remote
compaction capabilities. Do not enable it merely to clear an error or because a
short probe returned HTTP 200.

- `true`: include the route in the compatible candidate pool.
- `false`: explicitly exclude the route from guarded generation, even for an
  official endpoint.
- Omitted: standard Codex/default or official HTTPS endpoints retain their native
  default; custom endpoints remain unverified.

Keep all verified candidates eligible for health-aware selection. A failed OAuth
credential must not monopolize the pool or remove verified third-party routes.
If no route has declared/default support, return a nonretryable
`request_feature_unsupported` response rather than dispatching an unverified route.
Ordinary short requests and long plain-text histories do not require this extra
tool-history capability. Local token counting is exempt.

Example, only after compatibility verification:

```yaml
codex-api-key:
  - api-key: "<configured-by-operator>"
    base-url: "https://verified-provider.example/v1"
    native-responses: true
```

Management GET, YAML save/load, runtime synthesis, and hot reload preserve absent,
true, and false values. PATCH `{"value":{"native-responses":null},"index":0}`
clears a declaration. Default merging PUT preserves an omitted field for older
management clients; `replace=true` is a complete replacement and must carry every
intended field. Never use that mode to edit just one capability.

## One request-shape policy

Routing and HTTP/WebSocket execution share
`RequiresNativeResponsesToolHistory` and `SupportsNativeResponses`. Risk detection
uses the WorkBuddy/Codex client profile plus at least 240 messages with at least
8 declared tools/tool interactions, or a complex tool surface and a 2 MiB body.
Metadata and actual body evidence are combined conservatively. Large-request
retry budgets are a separate concern, not evidence of protocol incompatibility.

Source messages are not Responses items. Before candidate selection, generation
measures the actual request-scoped translator output once outside the retry loop;
one source message may expand into multiple messages, calls and results. The
result only adds a restrictive capability requirement. Do not duplicate the
translator's expansion algorithm or let low-reported metadata override the body.
Plugin translators and existing untranslated fallbacks retain their registry
semantics. Execution still checks its final body as a defense-in-depth boundary.

SDK calls use the request payload when no original payload was provided. Count
selection is explicitly distinguished from generation. Built-in scheduler
delegation must not escape the candidate set that passed compatibility and health
filtering. Home-dispatched credentials must pass the same guard before execution;
the remote Home scheduler's own candidate policy remains its responsibility.

## Failure semantics

- Return 429 for a pool whose active blockers are all quota/rate related.
- Return 503 `auth_unavailable` for local selection exhaustion caused by
  authentication, transport/provider breakers, or mixed causes. Preserve the
  known recovery delay as `Retry-After`; do not claim an upstream request occurred.
- Preserve actual upstream failures separately from local selection failures.
- Do not replay deterministic 400 failures or responses whose output is committed.
- GPT and remote-compaction streaming retry budgets belong to Manager. The HTTP
  bootstrap wrapper must not start another Manager operation after that budget
  ends. Non-GPT legacy bootstrap still respects explicit terminal request errors
  and committed/after-output markers.
- Downstream NewAPI can try a different aggregate entry point, but an entry point
  reporting `auth_unavailable` must stay excluded for that client request across
  retry rounds. This is not a persistent/global channel disable.

## Required regression cases

1. Broken native OAuth plus multiple verified healthy third-party routes.
2. First compatible route returns a real 429 or 502; another succeeds.
3. All compatible routes have 401/403/5xx breakers versus all have true rate limits.
4. Explicit false, unknown custom, malformed capability, and configuration hot reload.
5. Long plain-text, short tools, token count, SDK body-only, mixed-provider and Home paths.
6. HTTP and WebSocket, streaming and non-streaming, with zero upstream calls for a
   pre-dispatch capability rejection.
7. Plugin delegation respects the filtered candidates.
8. A typed local selection failure retains its public code and `Retry-After`.
9. NewAPI: entry A returns `auth_unavailable`, B has a transient failure, and a
   later round may retry B but never resets A's request-scoped exclusion.
10. Configuring generic 400 retry must not make `request_feature_unsupported`
    retryable; committed output must never be replayed.
11. Exercise the real Handler-to-Manager path with bootstrap retries set to 0, 1
    and 3; total attempts must stay within the same request budget.
12. A source history below the item threshold that expands above it must select
    a compatible route before execution, including plugin-only translators.

## Release boundary

Local test success is not deployment proof. Use the existing trusted CI and
immutable-image release process only when push/deployment is authorized. Verify
the exact running revision/digest, then exercise the client route and inspect
candidate exclusions, attempt/fallback counts, and final stream outcome. Health
endpoints alone do not prove a usable provider pool. A failed log-inspection
command is an acceptance gap, never evidence of zero errors.

No production capability declaration is automatically added by this patch.
Verify each third-party route's tool-history behavior before enabling it, and
independently resolve invalid OAuth credentials. Clearing all guards or rolling
back unrelated usage/accounting fixes is not a substitute.

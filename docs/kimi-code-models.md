# Kimi Code model compatibility

Thinking compatibility checked against the [Kimi Code model documentation](https://www.kimi.com/code/docs/kimi-code/models.html) on 2026-11-17. Other model metadata was verified on 2026-10-24.

Use the exact upstream IDs. Display names and invented aliases can silently select a different model at Kimi. Client-facing aliases must map explicitly to one of these IDs.

| Upstream ID | Maximum context* | Proxy default effort | Thinking off |
| --- | ---: | --- | --- |
| `k3.1-pro` | 1,048,576 | `max` | Normalized to `low` |
| `k3.1-pro-highspeed` | 1,048,576 | `max` | Normalized to `low` |
| `k3` | 1,048,576 | `high` | Kimi routes to non-thinking K2.8 |
| `k3-256k` | 262,144 | `high` | Kimi routes to non-thinking K2.8 |
| `kimi-for-coding` | 1,048,576 | `max` | Kimi routes to non-thinking K2.8 |
| `kimi-for-coding-highspeed` | 262,144 | Enabled, not adjustable | Kimi routes to non-thinking K2.8 |

*These are model ceilings, not account entitlements. For example, Plus accounts have a 262,144-token limit on K3.1 Pro; 1M requires a qualifying plan. No undocumented maximum output limit is added. K3-256k accepts images but not video. See the source for current membership requirements.

## Protocols and configuration

For OpenAI Chat Completions and native Responses:

```yaml
openai-compatibility:
  - name: "kimi-code"
    kind: "kimi"
    base-url: "https://api.kimi.com/coding/v1"
    api-key-entries:
      - api-key: "YOUR_KIMI_CODE_KEY"
    models:
      - name: "k3.1-pro"
        alias: "k3.1-pro"
      - name: "k3.1-pro-highspeed"
        alias: "k3.1-pro-highspeed"
```

The overseas base is `https://api.kimi.ai/coding/v1`. Incoming `/v1/responses` requests retain their native `input`, reasoning items and tools rather than being converted to Chat Completions. This also applies to the dedicated Kimi executor for the published Coding Plan IDs, including streaming responses; its existing OAuth credentials and identity headers are retained.

For native Anthropic Messages, use a `claude-api-key` entry with `base-url: "https://api.kimi.com/coding"` and the same explicit model mappings. Keep credentials on the endpoint surface for which they were issued. The dedicated Kimi executor's existing Messages-to-Chat path remains a compatibility path; select `claude-api-key` to use native Messages.

For K3.1 Pro / Pro Highspeed, a `claude-api-key` entry on either official HTTPS Coding Plan host also sends incoming Chat Completions and Responses requests through that host's matching native API. The selected API key, auth identity, proxy and configured headers are retained; only the request-local base path changes to `/coding/v1`. Incoming Messages remain on `/v1/messages`. This adaptation applies only to the exact `/coding` and `/coding/v1` bases, not third-party gateways, Moonshot's separate platform, OAuth-only credentials, unknown model IDs or `/responses/compact`.

K3.1 Chat requests preserve message-level dynamic tool definitions, 128-character tool names, schema strictness and numeric literals, `response_format`, `stop` and sampling controls other than the fixed Chat sampling settings below. The older shared tool repair and schema/name normalization are bypassed so invalid definitions reach Kimi's validator instead of silently becoming a different request. Kimi Messages routes default to no Anthropic system prompt or synthetic user ID; explicit per-credential cloaking settings retain their precedence. Native K3.1 Messages tool names are also passed through for upstream validation. Upstream usage is reported as returned; no fixed token deduction is applied.

## Video input in Chat Completions

For `k3`, `k3.1-pro` and `k3.1-pro-highspeed` on the official Coding Plan, send a user message with a `video_url` content part. Put the MP4 in `video_url.url` as a `data:video/mp4;base64,...` URL or a public HTTPS link. K3 video requests using an official `claude-api-key` entry use native Chat Completions so the video survives forwarding. K3 text-only requests keep their configured protocol.

The proxy fetches up to four remote MP4s totaling 8 MiB per request. Each link and redirect must use public HTTPS on port 443. The media host receives a separate GET without the caller's credentials. If the connection closes with EOF before any HTTP response, the proxy retries that GET once. A failed download or invalid MP4 signature returns 400 before calling the model. Exceeding the file count or byte limit returns 413. Inline videos do not need a download. This adaptation applies to Chat Completions.

## Thinking controls

Chat uses `reasoning_effort`, Responses uses `reasoning.effort`, and Messages uses `output_config.effort`. Original client controls are read before protocol conversion. Model suffixes such as `k3.1-pro(low)` take precedence over body controls; final payload overrides are validated again.

Chat also accepts Kimi's `thinking.effort`, which takes precedence over root `reasoning_effort` when present and non-null. Explicit disabled-thinking switches and model suffixes retain their existing priority. Responses and Messages retain their own native effort precedence.

For adjustable models, `ultra/max/xhigh` map to `max`, `high/medium` to `high`, and `low/minimum/light` to `low`. The proxy also accepts its existing `minimal` level as `low`. Unknown effort values return HTTP 400. K2.7 Code Highspeed rejects explicit effort adjustment. For K3.1 Pro and Pro Highspeed (including `[1m]` IDs), disabling thinking through an effort value, boolean toggle, zero budget, model suffix or Messages thinking object is normalized to `low`, the minimum supported effort. This is proxy compatibility behavior: Kimi itself still rejects disabled thinking. The proxy removes conflicting root-level off controls before sending the enabled effort in the target protocol, including after payload overrides. Valid explicit efforts and suffix priority are preserved. Older Coding Plan models retain their supported disabled-thinking behavior.

The proxy retains its existing default `max` for K3.1 when no thinking control is supplied; the current upstream default documented by Kimi is `high`. This compatibility fix does not change the proxy default.

## Forced tool selection

Kimi rejects a named `tool_choice` while thinking is enabled. For K3.1 Pro and Pro Highspeed, the proxy preserves a named function constraint by sending only that function's root-level definition with `required` (Chat/Responses) or `any` (Messages). The requested thinking effort remains enabled, including on Messages routes that previously applied Anthropic's forced-tool thinking removal. With a nonempty tool list, existing `auto`, `none` and required/any controls retain their handling. When root tools are absent, null or empty and there are no dynamic definitions in history, orphan auto/none/required/any controls are removed so the request can produce text without calling tools. A named choice with no matching definition still returns a local 400.

Missing or duplicate named definitions, unsupported tool types, and dynamic tool definitions in history return a request-scoped HTTP 400 before sending. The proxy does not broaden a named choice to other tools, rewrite historical tool calls or invent tool results. Payload overrides receive the same validation. See Kimi's [tool-choice guide](https://platform.kimi.com/docs/guide/use-tool-choice) and [Messages reference](https://platform.kimi.com/docs/api/messages); the exact field value of the November 17 customer rejection was not retained, so local synthetic coverage does not establish recovery of that production request.

Only root-level thinking controls are normalized. Returned reasoning, encrypted reasoning items, tool history and schema numbers are retained. Legacy `kimi-k3` Moonshot behavior is unchanged; it is not the Coding Plan `k3` ID. Existing K2 sampling compatibility is retained.

On official Coding Plan Chat Completions, K3.1 Pro and Pro Highspeed omit numeric `temperature` (0–2), `top_p` (0–1), and `presence_penalty` / `frequency_penalty` (−2 to 2) after request overrides. Kimi then uses its own sampling settings. Clients that depend on adjustable sampling should account for this behavior. Out-of-range values, invalid types, tools and response formats retain native validation. This also applies to Chat sent through the official Claude API key configuration. Native Responses retain their sampling fields.

For thinking Chat Completions on Coding Plan `k3` (including `k3[1m]`), the proxy drops the optional root `top_p` after payload overrides so the upstream uses its default. The requested effort stays in effect.

Coding Plan `k3` also requires `temperature=1`. Explicit temperature values are normalized after payload overrides, while the requested effort and response format remain intact. A synthetic Coding Plan request confirmed that temperature 0 is rejected and temperature 1 completes successfully. This sampling rule does not change native Responses payloads.

Known Kimi request rejections now retain fixed reason codes and reviewed field names in failure metadata. Raw upstream prose, tool names, URLs, credentials and client content are not recorded. Unknown errors keep the existing sanitized diagnostics; these labels do not enable retries or credential rotation.

For Claude Code's documented 1M mode, map `k3.1-pro[1m]` explicitly if using that client ID; see the [official Claude Code setup](https://www.kimi.com/code/docs/third-party-tools/claude-code.html). This does not grant 1M account permission. Start a new session when changing model families, and avoid changing effort repeatedly within a session.

## Credential scheduling and client identity

Kimi `spread` routing groups protocol aliases by the exact underlying credential,
so adding an OpenAI/Anthropic entry for the same key does not increase its traffic
share. Active-request load is shared between those aliases within the selected
route/model. Different keys from one membership cannot be identified as the same
account without an explicit account mapping; this change does not infer one.

Configuration reloads of an unchanged Kimi key retain its active cooldown and
health state. A late success on a model alias cannot clear a current credential
quota cooldown. Key replacement and explicit cooling/disable controls retain
their existing lifecycle semantics.

At most 64 Kimi requests per Manager may wait for a retry cooldown concurrently.
The existing `request-retry` and `max-retry-interval` settings still decide whether
and how long to wait. Cancellation releases the waiting slot; a full queue
preserves the current failure. This is a bound on cooldown waiters, not an
upstream concurrency cap or a FIFO request scheduler. Long quota resets outside
the retry interval are not queued. Existing insufficient-quota fallback remains
bounded to two distinct credentials.

On the official HTTPS `/coding` endpoints, compatible OpenAI and Messages
requests forward the incoming User-Agent (or identify as `CLIProxyAPI` if none
is usable). Messages requests no longer acquire a synthesized Anthropic OAuth
device/session fingerprint. Explicit configured headers still take precedence;
operators should review fixed client-identity overrides separately. The native
Kimi OAuth executor retains its acquired identity. The [official integration
guide](https://www.kimi.com/code/docs/en/) requires the tool's real identity.

## Local verification

Tests cover the thinking source/target protocol matrix, effort aliases/defaults, suffix and `thinking.effort` priority, disabled-to-low normalization and rejection of unknown efforts before upstream calls (including payload overrides), exact model IDs, native Responses JSON/SSE, OAuth identity preservation, and unchanged tool/reasoning content and large integer schema literals. KVV-derived wire regressions cover Chat/Responses on official `claude-api-key` routes, dynamic tools, 128-character names, strict schemas, invalid-field forwarding, no default Messages cloaking, native reasoning/usage, payload overrides, and endpoint/credential isolation. Local fake-upstream tests do not verify account permissions, production routing or a new KVV score.

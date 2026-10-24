# Kimi Code model compatibility

Verified against the [Kimi Code model documentation](https://www.kimi.com/code/docs/kimi-code/models.html) on 2026-10-24.

Use the exact upstream IDs. Display names and invented aliases can silently select a different model at Kimi. Client-facing aliases must map explicitly to one of these IDs.

| Upstream ID | Maximum context* | Default effort | Thinking off |
| --- | ---: | --- | --- |
| `k3.1-pro` | 1,048,576 | `max` | Rejected locally with HTTP 400 |
| `k3.1-pro-highspeed` | 1,048,576 | `max` | Rejected locally with HTTP 400 |
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

## Thinking controls

Chat uses `reasoning_effort`, Responses uses `reasoning.effort`, and Messages uses `output_config.effort`. Original client controls are read before protocol conversion. Model suffixes such as `k3.1-pro(low)` take precedence over body controls; final payload overrides are validated again.

For adjustable models, `ultra/max/xhigh` map to `max`, `high/medium` to `high`, and `low/minimum/light` to `low`. The proxy also accepts its existing `minimal` level as `low`. Unknown effort values return HTTP 400. K2.7 Code Highspeed rejects explicit effort adjustment. Disabling K3.1 through an effort value, boolean toggle, zero budget or Messages thinking object returns HTTP 400 before contacting the upstream.

Only root-level thinking controls are normalized. Returned reasoning, encrypted reasoning items, tool history and schema numbers are retained. Legacy `kimi-k3` Moonshot behavior is unchanged; it is not the Coding Plan `k3` ID. Existing K2 sampling compatibility is retained.

For Claude Code's documented 1M mode, map `k3.1-pro[1m]` explicitly if using that client ID; see the [official Claude Code setup](https://www.kimi.com/code/docs/third-party-tools/claude-code.html). This does not grant 1M account permission. Start a new session when changing model families, and avoid changing effort repeatedly within a session.

## Local verification

Tests cover the thinking source/target protocol matrix, effort aliases/defaults, suffix priority, strict rejection before upstream calls (including payload overrides), exact model IDs, native Responses JSON/SSE, OAuth identity preservation, and unchanged tool/reasoning content and large integer schema literals. Local fake-upstream tests do not verify account permissions or production routing.

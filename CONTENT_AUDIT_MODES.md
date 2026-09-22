# Content audit modes

The management page exposes three request-time modes. Switching modes does not
change the stored keyword policy, ban users, revoke tokens, or clear sessions.

| Mode | Behavior |
| --- | --- |
| `strict` | Apply the complete current policy and existing model-review settings. |
| `simple` | Retain the rule IDs listed in `internal/contentaudit/mode.go` as eligible for blocking. Other matches remain observation-only, including model and cached verdicts. |
| `off` | Bypass request-time audit, model review and new hit recording. Keep historical evidence and existing retention controls. Internal identity headers are still stripped. |

Simple mode still blocks the selected explicit-danger rules; it is not an
allowlist for arbitrary content. Custom/new rule IDs are observation-only unless
included in the simple-mode set. Retained rules still honor the policy's
disabled/action/context settings. Matching scope and quotation, truncation,
history and document-material safeguards are unchanged. Effective actions are
ranked **before** choosing the winning rule, so an observation match cannot hide
a separate retained hard-block match.

## Legacy settings

- `enabled: false` or `mode: off` disables request-time audit.
- An absent/unknown mode with `enabled: true` defaults to `strict`.
- `audit-only: true` remains a separate legacy observation override. It does not
  become simple enforcement. The UI labels this explicitly rather than selecting
  one of the three enforcement buttons.
- Choosing any mode via `PATCH /v0/management/content-audit/mode` with
  `{"value":"strict|simple|off"}` sets `enabled` consistently and clears
  `audit-only`. Selecting strict/simple from legacy observation enables blocking.
- The legacy enabled endpoint remains supported. Enabling from off chooses
  strict; an already-enabled simple mode is preserved.

## Save and activation

Management writes clone configuration under the management lock, persist it and
schedule the existing hot-reload hook. Failed saves do not change shared runtime
configuration; version conflicts preserve the newly loaded disk configuration.
`GET /content-audit/mode` reports configuration, while `GET /content-audit/status`
reports the request-time snapshot. A save response alone is not activation proof.
The UI reads runtime status before showing success and warns if activation cannot
be confirmed. Requests already in flight retain their original immutable snapshot.

Local regression coverage includes competing rules, legacy observation, mode
round trips, hot policy updates, off-mode evidence preservation, failed writes,
version conflicts and concurrent management calls. Production deployment remains
a separate step requiring CI and runtime verification.

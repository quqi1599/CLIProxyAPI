# MiniMax M3.1 forwarding

Verified against MiniMax's official documentation on 2026-09-30:

- [Model invocation](https://platform.minimax.cn/docs/guides/text-generation)
- [Messages API](https://platform.minimax.cn/docs/api-reference/text-chat-anthropic)
- [Chat Completions API](https://platform.minimax.cn/docs/api-reference/text-chat-openai)
- [Tool use and interleaved thinking](https://platform.minimax.cn/docs/guides/text-m3-function-call)

The published model ID is `MiniMax-M3.1-Flash-Preview`, currently available through M Plan and MiniMax Code. `MiniMax-M3.1` and `MiniMax-M3.1-flash` are recognized as configured compatibility aliases; CPA does not automatically map them to the preview or advertise them as separate official models. Configure the upstream ID explicitly.

M3.1 requires adaptive thinking. Supported effort levels are `low`, `medium`, `high`, `xhigh`, and `max`; omission retains the upstream default (`max`). Use `low` to reduce reasoning latency. Explicit disable requests return a local HTTP 400 instead of being silently changed. Effort survives Chat/Responses/Messages translation, and source controls prevent `max` from becoming `xhigh` through intermediate budget conversion.

M3.1 preserves returned reasoning history, image/video content, supported sampling controls, and streaming usage requests. Mixed model pools retain M3.1 for multimodal/long-context requests. Existing legacy MiniMax policies remain model-scoped. Native Responses routing is unchanged.

Validation uses local request-plan and mock HTTP/SSE regression tests; it does not establish subscription access, real upstream behavior, CI publication, or production deployment.

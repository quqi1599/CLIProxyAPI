# MiniMax M3.1 双协议兼容修复（2026-10-06）

同一真实上游凭据、low 和 8192 输出预算下，官方 Chat 使用 MiniMax-M3.1 返回 unknown model / 2013，只改为 MiniMax-M3.1-Flash-Preview 则返回 OK。旧渠道 1 的两条关联请求仍向上游发送短名；官方 Anthropic 对短名返回的身份是 M3。

## 行为

- 仅已知官方 HTTPS SDK 端点，把 M3.1 与 M3.1-flash 短名规范为官方 Preview ID；第三方、其他型号保持原映射。普通、SSE、原始 HTTP 与操作者参数覆盖后的请求都经过规范。
- 五档 effort、模型后缀优先规则、原始工具和思考历史保留。单独 SDK effort 拼写和显式关闭控制跨协议一致；M3.1 显式关闭继续本地拒绝。
- Sonnet 聚合明确关闭思考时，在执行前排除必须思考的 MiniMax 候选，允许已有兼容候选接手；不把 off 静默改为 low，不增加上游尝试。
- 已识别的 MiniMax 模型、思考、effort、reasoning_split 拒绝返回固定诊断码，保留 request 作用域与禁止重放；上游任意正文、密钥和客户内容不输出。2013 单独出现不用于猜测参数错误。

[官方 Chat 文档](https://platform.minimax.io/docs/api-reference/text-chat-openai)与[官方 Anthropic 文档](https://platform.minimax.io/docs/api-reference/text-anthropic-api)当前公开的 M3.1 ID 为 MiniMax-M3.1-Flash-Preview。本补丁不声明另一个独立正式 M3.1 型号。

## 验证与发布

普通/SSE、Chat/Messages/Responses、五档 effort、原始 HTTP、端点边界、固定拒绝诊断和 Sonnet 候选筛选均有回归。最终发布按 trusted-ci → 固定 GHCR digest → 目标服务切换 → 健康与业务探针执行，发布后证据另行记录。

本次在隔离工作区实施。原开发目录已有的 off 自动转 low 候选改动保持原样，未合入本发布。

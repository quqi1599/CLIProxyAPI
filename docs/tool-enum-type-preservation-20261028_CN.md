# 工具枚举类型保留

2026-10-28。Chat 转 Claude Messages 时，普通工具的 Strict Schema 清理曾复用 Gemini 的枚举字符串化规则，把 `integer / enum: [7]` 改为 `string / enum: ["7"]`。最终发给上游的请求也包含这个变化；原生 Claude 对照不经过此清理路径。

本次仅区分枚举处理模式：Gemini/Antigravity 保留原有字符串化行为，Strict 入口保留枚举及 const 值的数字、布尔、字符串和 null，并保持精确数值。响应校验不接收错误类型，内部重试和发布停机行为不变。

原有组合规则、引用、nullable union、required 和 additionalProperties 处理保持，不宣称完整 JSON Schema 保真。回归覆盖 helper、两种工具参数字段、流式/非流式翻译及最终出站请求；旧版九个反例与原生 Claude 对照分别保存，修复后重新验证。

本次镜像发布另为每个 run/attempt 隔离 Docker 登录目录，保持原账号、GITHUB_TOKEN 和权限，避免并发任务相互覆盖认证状态。正式 CI、固定镜像和生产读回分别确认。

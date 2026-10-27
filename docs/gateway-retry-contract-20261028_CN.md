# 旧 CPA 向 NewAPI 报告已执行与禁止重放

2026-10-28；本次补丁基线 `bc67ec54affa117367b38fdfa0a5da02d3af0ef2`。提交、CI、镜像和生产状态以本次发布记录为准。

## 触发条件与依据

同一 NewAPI 客户请求在渠道 9 三次进入旧 CPA；三个内部请求分别进行了 5、4、4 次尝试，最终仍失败。线上旧 CPA 不输出新 CPA 已有的 `X-CPA-Retry-Scope` / `X-CPA-Retry-Attempts`。NewAPI `4bd9b01` 已识别这两个字段，缺字段时对普通 429/5xx 保留外层重试，导致同一池被重复访问。

本轮只读证据和跨服务报告位于 `/Users/qinxian/code/cpa-new/docs/multiclient-errors-20261028_CN.md`；查询输出在 `/Users/qinxian/.local/share/cpa-new/incident-20261028/`，无凭据或客户正文。

## 修改

HTTP 处理层为一条请求建立有界、加锁的重试证据对象。模型 HTTP 客户端通过 `UsageReporter.TrackHTTPClient` 包装 `RoundTripper`，每次进入模型传输调用增加一次计数，并组合已有 `httptrace` 回调，观察获取连接、获得连接、写请求头和写请求的阶段。这个计数是模型传输调用次数，不是 key 数量、选路次数或成功接受次数；连接前失败也占一次传输尝试。认证、准备和纯本地校验没有调用模型传输时，不增加计数。

Codex 与复用同一发送函数的 xAI WebSocket 在写模型消息前计数；连接握手与空连接不计为模型请求。写失败可能只发送了部分消息，保守保留结果未知状态。补丁不修改内部选择、重试、队列或终态策略。

通用及 Claude 错误输出在未提交响应前生成控制头：

- 明确收到上游 4xx/5xx 拒绝，或由连接阶段/类型化 DNS、dial 错误确认发生在连接前，且最终状态是 429/5xx：输出 `X-CPA-Retry-Scope: gateway`，让 NewAPI 排除同一聚合服务，并保留独立备用。
- 任何一次传输收到 2xx、写入后没有明确拒绝、已获得连接后结果不明，或自定义传输没有足以确认连接前失败的信号：输出 `request` 和 `accepted_outcome_unknown`，禁止外层重放。不按 EOF、dial 等错误文字猜测接受状态。
- 最终类型化错误明确记录已输出时使用 `upstream_output_observed`。早期接受或不确定证据不会被后续明确拒绝覆盖；传输内部观察到多次写请求时也保留不确定状态。

零传输调用、客户端取消、成功响应、已提交流及越界计数不生成控制头。无传输证据的路径保持原有外层处理。客户端请求头和供应商 Addon 不能填写计数；同名 Addon 头在终态错误响应中删除后根据本地证据重建。原 HTTP 状态、错误正文和 Retry-After 保留，不新增客户内容采集。

采用请求级对象而非全局表，避免串请求和无界状态；不通过修改 NewAPI 的普通供应商错误规则猜测聚合池身份。当前渠道 9 的可信 CPA 设置已只读确认启用。

## 已有内部重试边界

GPT 请求走 `attempt_runner.go` 的专用预算及 `shouldRetryGPTRound`，不等同于通用 `request-retry` 配置。`beginGPTRound` 清空本轮失败集合，下一轮可再次选择同 key；压力评估后 `gptFirstEventRetryLimits` 将轮数限制为两轮。因此设置 `request-retry: 0` 和单个 key，也不保证只调用一次上游。相同 `routingChannelBaseKey` 的 key 在本轮会被一起标记为已尝试。

本补丁报告实际观察到的传输调用，并阻止 NewAPI 再次进入该聚合服务；不修改上述内部行为。上游已接受但未交付输出的请求仍可能被旧 CPA 原有内部策略重试，这一限制与外层重放控制分别记录。

## 本机验证

本次 transport 版本的实际结果：

- `sdk/cliproxy/auth`、`sdk/api/handlers`、`sdk/api/handlers/claude`、`internal/runtime/executor/helps`、`internal/runtime/executor` 五包完整回归通过（`legacy-transport-regression.log`）。
- 四包相关测试在 race 下通过（`legacy-transport-race.log`），上述五包 `go vet` 通过（`legacy-transport-vet.log`）。
- 服务器入口构建成功，二进制为 `../legacy-retry-server`。

新增验证涵盖真实 HTTP 429/503、2xx 后截断、发送正文后断开、连接拒绝，类型化 DNS/dial 与伪造错误文字，自定义/嵌套传输、httptrace 组合、早期接受证据保留、并发快照、计数上界，以及无传输的选路/结果汇报不会冒充尝试。HTTP 控制头测试覆盖普通与已接受错误、伪造 Addon/请求头、取消、零尝试和已提交响应。实际 Codex WebSocket 成功发送计为一次，握手失败计为零；模型 HTTP 客户端的集成测试确认包装器实际生效。

跨进程 NewAPI→旧 CPA→合成上游验证由跨服务报告记录。本地测试不等于 CI 或生产恢复；生产未改。发布需按原私有仓库、自建 CI、固定 SHA 镜像和目标服务切换流程执行；回滚仅恢复旧镜像，本补丁没有新增存储格式。

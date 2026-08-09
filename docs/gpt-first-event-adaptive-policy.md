# GPT 首事件自适应策略

这套策略只作用于 GPT/Codex 流式请求的“首个下游可交付事件”，不限制首事件之后的长任务执行时间。

## 决策口径

- 只统计每个请求的第一个上游尝试，避免同一请求的重复失败放大全局故障率。
- 排除参数错误、权限错误和其他请求级失败。
- 每个具体模型独立使用 5 分钟滑动窗口，不会用 `*` 的混合模型数据覆盖冷启动或低流量模型。
- 保护性升档最少需要 40 个有效样本；降档恢复最少需要 100 个有效样本。每分钟最多评估一次。
- 冷启动或低流量模型在 10–39 个样本时，如果本地 deadline 超时率连续 3 个评估窗口不低于 80%，只保护性地从 `normal` 升到 `slow_30s`，不会在低样本下继续放大到 40/50 秒。
- 连续 3 个评估窗口满足条件才升级；状态最少保持 5 分钟。
- 恢复需下一档时限内成功率连续 10 个窗口不低于 95%。
- 样本数不足时保留该模型已学习的状态，不回落到全局 `normal/25s`。

## 状态与兜底

| 状态 | 首事件时限 | 每轮渠道 | 最多轮数 | 首事件累计等待预算 |
| --- | ---: | ---: | ---: | ---: |
| `normal` | 25 秒 | 8 | 3 | 300 秒 |
| `slow_30s` | 30 秒 | 6 | 2 | 300 秒 |
| `slow_40s` | 40 秒 | 4 | 2 | 300 秒 |
| `slow_50s` | 50 秒 | 3 | 2 | 300 秒 |
| `outage` | 25 秒 | 3 | 1 | 75 秒 |

`normal` 在 25 秒内成功率低于 90%，且 CPA 本地首事件 deadline 超时不低于 10% 时进入 `slow_30s`；单纯的明确 5xx 不会伪装成慢响应并放大 deadline。从 30 秒继续升到 40/50 秒时，当前档成功率需低于 50%，并且满足以下两个延时证据之一：至少 5% 的“额外等待后成功”，或至少 50% 的本地 deadline 超时。后者会把集体慢响应逐档放宽，而不是误判为上游宕机。

当当前时限内成功率低于 10%，且明确的上游 5xx 或网络失败合计不低于 90% 时，才判定为 `outage`。此时不会把每个渠道放宽到 50 秒，而是快速尝试 3 个不同渠道后停止，防止一个请求长时间占用连接和并发。
如果 `outage` 期间明确硬失败消失，而主要现象变为本地 deadline 超时，策略会经连续窗口确认后回到 `slow_30s`，再按模型实际延时逐档调整。
为避免低流量模型永久停在 `outage`，进入该状态 5 分钟后，如果最新 5 分钟窗口不再同时具备至少 40 个样本和 90% 明确硬失败，下一次策略快照会自动退让到 `slow_30s`。

`gpt_first_event_timeout` 是 CPA 在输出前触发的本地 deadline。它仅作为当前模型的换路和慢档证据，不计入 route/model hard breaker；真实 provider 5xx、协议错误和已分类的 provider/model 故障仍按原有 breaker 规则累计。

每个请求在开始时固定一份策略快照，中途状态变更只影响下一个请求。

## 零可用路由恢复

当同一具体模型的候选路由都处于可恢复的 cooldown 或 breaker 状态时，系统按“模型 + 候选路由集合”建立单飞半开探测：

- 同一时刻只有 1 个 owner 请求实际访问半开路由，最多 8 个同范围请求等待该探测结果。
- 等待时间与当前模型 25–50 秒的首事件时限对齐，多 2 秒容差，且不低于 15 秒、不高于 55 秒，同时受客户请求上下文约束。
- 等待探测不消耗新的 GPT 上游轮数；超出等待上限、排队上限或客户取消时有界结束，不会回退为无限量 cooldown 休眠。
- owner 成功、失败或被客户取消时，都先清理本次半开 active 标记再唤醒等待者，防止等待者集体穿透到同一条失败路由。

## 状态持久化

精确模型的当前档位保存到凭据目录下独立的 `.runtime/gpt-first-event-policy.json`，不与凭据 `.cds` cooldown 文件混用。

- 只保存当前策略状态、来源和更新时间，不保存请求样本或客户数据。
- 状态记录 24 小时过期；重启恢复时不会把旧 `outage` 原样带回，而是降级为 `slow_30s`。
- 档位变化立即异步持久化；慢档仍有延时证据时最多每小时 checkpoint 一次。单次写入限时 2 秒，并串行导出和写入，不会因为异步写入乱序覆盖较新状态。

## 日志与监控

结构化日志事件：

- `gpt_first_event_observation`：记录首尝试结果、延迟、窗口成功率和故障率。
- `gpt_first_event_policy_transition`：记录状态切换、原因、新时限和新重试预算。
- `gpt_first_event_policy_checkpoint`：记录慢档仍有支持证据时的持久化 checkpoint。
- `request_execution_summary`：记录该请求实际使用的策略快照、累计等待、超时次数以及预算是否耗尽。
- `auth_selection_failed`：输出 `candidate_route_count`、`eligible_route_count`、`blocked_route_count`、`breaker_open_count`、`blocked_reasons`、`breaker_statuses`、`breaker_reasons` 和 `earliest_recovery_ms`，用于区分真正零路由、cooldown 和 breaker 阻断。

同时，文本主日志放行 `normalized_status`、`outer_status`、`failure_kind`、`failure_scope`、`semantic_type`、`semantic_code`、`stream_phase`、`output_committed` 和 `hard_failure_rate`，使错误分类、重试、cooldown 和最终摘要能使用同一份 canonical failure 证据。

管理接口：

```text
GET /v0/management/custom/monitor/gpt-first-event-policy?model=*&days=7
```

`current` 返回当前 5 分钟窗口和生效状态，`daily` 返回最近 1–31 天的进程内按日聚合。接口明确返回 `runtime_scoped=true`：进程重启后样本窗口和内存日聚合会重新累计，但上述当前模型档位会按 24 小时 TTL 恢复。跨重启的详细复盘仍以持久化结构日志为准。

## 配置边界

`gpt-first-event-timeout: 25` 启用自适应策略。配置为其他正数时视为人工固定时限，不做自动放宽；负数禁用首事件时限。

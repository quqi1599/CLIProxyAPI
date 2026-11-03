# 网页体验审计身份 v2

兼容补丁基于 `ae5f05428628f8392b5194de955461576b1bb59b`；验证与发布结果分别记录。用于兼容 NewAPI 网页体验的已登录用户，避免请求回退旧 CPA 后因临时 Token ID 为零而返回身份错误。

## 协议边界

正式 API Token 和管理员渠道测试继续使用 v1，签名内容不变。网页体验使用 v2：

- `X-CPA-Audit-Version: 2`
- `X-CPA-Audit-Identity-Kind: playground`
- 已登录用户的正数 `user_id`、非空 `request_id`
- `token_id=0`、`token_name=__playground__`
- `channel_test=0`（省略时等价为零），不得冒充管理员测试

v2 在原签名字段中加入 `identity_kind`，仍绑定模型、方法、实际路径和时间。未知版本、缺失类型、形态冲突、过期、篡改与降级请求均拒绝。客户端身份头在审计后剥离；网页体验仍执行正常内容审计。

审计事件保留真实用户归属，用现有 `token_name` 标记网页体验，无数据库迁移。模型复核采用独立 `verified-playground:<user_id>` 范围；匹配规则和零命中 shadow 抽样都使用这一范围，不与其他用户或正式 Token 共享。受信教育文档名单仍要求真实 Token ID。

## 验证与发布顺序

`playground_identity_test.go` 覆盖 NewAPI 的独立固定签名、23 类错误形态/篡改、用户范围隔离、原 v1 兼容，以及通过真实审计中间件的放行、观察、阻断和用户归属。全部请求与内容均为合成样本。

发布时先升级所有可能接收网页体验的 CPA 验证端（包括新版与本旧版后备），再升级 NewAPI 签名端。未升级的旧 CPA 会拒绝 v2，不能以新版 CPA 测试通过替代后备验证。回滚时先恢复 NewAPI 发送端，再回退验证端。生产渠道、账户额度和签名密钥不由本补丁修改。

## 本地检查记录

- 身份/v1 兼容/网页体验/中间件定向测试通过。
- `go test -race ./internal/contentaudit -run 'Test(VerifyIdentity|VerifyPlayground|Playground|StripIdentityHeaders|CachedBlock|ZeroHit)' -count=1` 通过（8.283 秒）。
- `go build ./cmd/server` 通过，临时构建产物已清理；`git diff --check` 通过。
- 整个 `contentaudit` 包的 race 运行未通过：221 秒后出现本机临时目录 `no space left on device`，多个测试无法创建 TempDir；工具输出曾截断，未把该次全包运行报告为通过。进程退出后只读核对可用空间约 10 GiB，随后仅补跑上述定向 race 并通过，未重复整个包，也未清理其他任务数据。

此文档在旧仓库现有 `docs/*` 忽略规则内；后续获准提交时需要显式加入该单文件，不需要修改全局忽略规则。

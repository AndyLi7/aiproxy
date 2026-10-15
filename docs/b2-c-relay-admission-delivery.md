# C：实际 relay 入口持久准入

基于主控已验收的 native R1 `1c69834`。本批接通 relay 的实际执行入口，不代表完整 C、使用关闭或安全停用完成。应用 C2A-R1 `4fc46cf` 已另行由主控验收，工作区保持独立。

## 实现与范围

- `relay/controller/DoHelper` 在 `ConvertRequest` 前调用持久准入，覆盖上层 public relay/retry/admin channel-test 和直接 `Handle`（例如 web-search rewrite 的内部模型调用）。不能只在上层 handler 检查，否则插件内部模型调用可以绕过。普通/流式/generic async 在执行上游前已有 use，async usage/outbox 未落库、进程退出或接受情况未知均保留该记录。
- `model.AdmitRelayAttempt` 复用 control → channel → family → deployment proof 的事务协议。身份来自服务端实际选中的完整 channel；每次执行创建新的随机 server nonce。客户 request ID 重复、旧 use ID、probe header 都不能重放已封口身份的授权。ResourceKey 绑定服务端 billing operation 和调用身份；每次 attempt 有独立 use。同 key 的精确 successor 可接新调用，旧身份及其他别名拒绝。
- 内部/admin 请求的 group/token 可能为空/零。准入用独立 actor hash 命名空间，没有改写客户账单所有权。激活后 ID=0 预览、disabled shadow、错误/过时身份仍拒绝；受限 probe 未实现，不能用通用试用豁免。
- 准入分类不复用日志 `IsReadMode`：AlphaSearch 和文件上传会产生新工作，必须准入；已有任务的 poll/download/cancel/delete 可继续通过各自冻结身份/权限检查。未知新增 mode 默认需要准入。该豁免只跳过新工作门槛，不绕过原有读取鉴权。
- 从未安装/从未激活 C 保持旧行为；部分存储、DB/LogDB 分离、证明被撤销或身份漂移拒绝。没有生产 proof 签发方、激活入口或 schema 变更。

## 验证

- `/tmp/b2-c-relay-regression.jsonl`：5 个相关 Go 包，56 根/含子测试 140 项 race 回归，0 fail / 0 skip；包括准入、native、relay、failover、共享 Handle。
- `/tmp/b2-c-relay-final-targeted.jsonl`：补齐旧存储兼容后 7 根/33 项 race 定向测试，0 fail / 0 skip。
- 实际 `relayHandler` / shared `Handle` + OpenAI adaptor + loopback HTTP 合成上游验证：send 时另一查询已读到 open use；普通、stream、内部/admin、直接内部 Handle、503 未知接受；sealed/stale-key/stale-host/revoked/partial/split/disabled/preview 在 send 前拒绝，客户伪造 header/use 无效；读取不产生新 use，同 key successor 可以发送。fixture 故意不建 async/task 表，证明不是事后依赖 outbox 补记。
- model 测试覆盖相同 RequestID/BillingOperationID 多 attempt 的不同 generation、封口后拒绝、未知 use 保留，以及未安装/未激活存储与临时 admin preview 的旧行为兼容。
- `go -C core build ./...` exit 0；`/tmp/b2-c-relay-build.log`。本批未新增 SQL 锁算法，也未重跑 PostgreSQL；已有准入/native PostgreSQL 证据不称为本批实测。
- 提交前 security-scan worktree/staged + 人工完整差异审查；精确 staged 日志 `/tmp/b2-c-relay-security-staged.log`。仅合成 key、隔离 SQLite 和 loopback HTTP，没有真实供应商请求、付费探测或生产读写。

## 后续及限制

所有本批 relay use 暂时保留 open，成功 HTTP 响应或失败响应都不能独自证明账单完成、无后台任务或无活跃执行者。尚待补齐已证明未接受/终态与账单/worker 使用关闭，以及持久恢复、最终 safe-disable。该保守状态不作为完整 drain 可用声明。

独立 image task 预付费/重试入口尚未接入本批 DoHelper，需单独与预留代次、钱包和换渠道 attempt 串行化；native 已有前批保护。插件外部搜索引擎不使用 channel 凭据；若未来新增绕过 shared Handle 的模型调用，必须纳入入口证明。全部 executor、真实部署 issuer、恢复水位仍未闭合，生产 C 激活继续受阻。实际 gateway CAS adapter/受限 probe、应用 prepare/live 取证/API/UI/drain/revert 继续后续实现。

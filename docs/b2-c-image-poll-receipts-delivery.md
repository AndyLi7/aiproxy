# POLL-RECEIPT-1：图片轮询动作及结果回执保护

基线：主控已验收组合 `5606008ad6d719a723ede0d2680accd31b2501da`。独立分支 `codex/b2-image-poll-receipts`，仅本地开发。本批没有新 schema、TTL、报价、参数映射或供应商接入；既有三分钟 ProcessingToken 和合法续租期限不变。

## 变化

- 实际 `processOneImageUsage` 起步校验原 usage ID / ProcessingToken，provider-neutral poll 调用前再次校验。通过仅服务端可安装的 context guard，fal 每个 status/fallback/result HTTP、图片下载/上传/metadata HTTP 各自检查当前合法 claim；已有未安装 guard 的调用保持原接口兼容。
- `SetClaimedImageTaskResult`、`SaveClaimedArchivedImage`、`CompleteClaimedImageArchive`、超大结果失败和非法已存图片数量失败使用 task → usage → log 锁序。锁后读取数据库实际 NextPollAt；不使用业务 info.NextPollAt 调度值。校验冻结 core / 完整 contract / upstream / credential / archive 事实和 usage/log 绑定，最后重新读取 DB clock，原 deadline 到期则全部回滚。
- 合法同 token 延期允许使用已提交的新 deadline；过期未换 token、换 owner、非 pending、冻结事实改变均拒绝。未复用其他 owner 或新代授权。
- 保留原 provider metadata、辅助图片和每张 archive data CAS；最终 archive completion 也比较完整已验证 data。事务提交后才更新调用者内存 data，失败不留下假 checkpoint。
- 普通 failed 不再先写 task/outbox 然后用旧 claim 调用 void mark-failed：同事务写 task、自己的 usage/log 并释放原 token。入口已 failed 也必须赢得受保护 outbox 转换后才能进入旧退款流程。合法 owner 的迟到失败遇到 durable completed 时保留完成结果并继续原结算分支，不误退款。
- 保留此前 mark-failed 的缺 log 宽限（无 LogID 或该行不存在）；已存在但身份、operation、mode 或状态不符的 log 拒绝并回滚。缺 log 不构成完整交付或关闭证明。task 上已冻结的 billing operation 作为 log 绑定事实，兼容旧 usage 未携带 operation 的记录。

## 验证

不可变基线导出 `/private/tmp/b2-poll-before-cvzuyp7d/` 加入相同实际 worker 反例：10 个叶场景失败（含父节点 17 fail），覆盖入口过期/换 owner 仍 poll 或退款、status 查询后仍取 result、迟到成功/失败仍写 task。只是本地 HTTP fixtures，没有真实 fal 请求。

最终验证：

| 检查 | 结果 | 证据 |
| --- | --- | --- |
| `go test -race ./model ./task ./common/ownedimage ./relay/adaptor/fal -run 'TestImage\|TestAsyncUsage\|TestPostgresImagePoll\|TestArchive\|TestOwned\|TestStore\|TestReadMetadata\|TestFal' -count=1 -json` | 87 根、504 项：487 pass / 17 PG skip / 0 fail；17 个 PG skip 随后单独实测通过 | `/private/tmp/b2-poll-final-targeted.jsonl` |
| task 全包 race | 171 pass / 4 skip / 0 fail；其后仅新增 invalid stored-count 4 叶场景＋父用例，已纳入上行 | `/private/tmp/b2-poll-worker-final.jsonl` |
| fal / ownedimage 全包 race | 96 / 16 pass，0 skip / fail | `/private/tmp/b2-poll-packages-race.jsonl`；最终源码也在下述中断的大命令中各自完整退出 0 |
| 实际 PostgreSQL16 | 17 叶场景，含父节点 22 pass / 0 skip / fail | `/private/tmp/b2-poll-pg-kk3pvrmk/tests.jsonl`，`cleanup.json` |
| `go build ./...` | 通过 | `/private/tmp/b2-poll-build-final.log` |
| security-scan 脚本＋逐文件 diff 人工审查 | 13 文件 clean，无 HIGH / MEDIUM finding | `/private/tmp/b2-poll-security-staged.log` |

4 个 task 全包 skip：既有 PostgreSQL stop/join 的 `stop` / `parent-cancel` 两个场景，以及 `TestSECWalletIntegration`、`TestUsageAttributionLedgerMatchesLogs`。本批没有重跑这四个外部 fixture，既有验收结果不冒充本次实测。曾启动全 model 包并进入与本批无关的 `TestCallStatusSQLMatchesGo` 长矩阵，已按主控要求停止；`/private/tmp/b2-poll-full-final.jsonl` 仅保留诊断，不称全 model 通过，也不累加其中 model 部分计数。

实际 worker 的 `TestImageQueueMeasuredSettlementPersistsAndReplays/stale_failed_poll_after_durable_completion` 继续通过。新 `TestImagePollInvalidStoredCountFencesFailureReceipt` 验证合法失败、入口过期、换 owner、写后自然到期，失效时 outbox / log 一起保持 pending、钱包零动作。

PG harness：`/private/tmp/run-b2-poll-pg.py`。PostgreSQL 使用缓存的 `postgres:16-alpine` 临时 loopback 实例，独立数据库/schema；最后一轮 `/private/tmp/b2-poll-pg-kk3pvrmk/` 共 17 个真实叶场景，含父节点 22 pass、0 fail、0 skip。覆盖 task 锁等待自然到期、原期限之后同 token 合法延期、同秒换 owner、最后 log 锁等待失租回滚；容器和临时凭据已清理。

SQLite 用例还覆盖写后自然过期回滚、毫秒 deadline、archive stale-data CAS、非法数量失败、missing-log 宽限和错身份 log 回滚。现有正常失败即时退款、图片/辅助图/metadata、既有 completed winner 兼容用例保留。MySQL 仅复用已验收的 DB clock/列精度路径和现有记录驱动回归，未进行 MySQL 实例实测。

## 明确保留的边界

- 本批保证原 claim 失效时不新发本批的 poll/archive 动作、不写其迟到结果/失败回执；实际 worker 在进入完成结算前再次校验。已经开始的远端动作无法撤销，也不据此证明其退出。
- 赢得 durable failed 的调用者仍沿用既有终态提交后的 refund；它的各钱包 action 与回执未被本批 poll claim 事务保护。退款失败仍交既有 ImagePrepaymentRecoveryTask。没有通过 nil recovery handle 降级假造 poll 权限。
- 完成分支 `imageprepayment.Sync` 内的多动作/钱包回执，以及普通 measurement/log/consume 的独立动作授权仍待后续最小批次；此处入口校验不能替代每一步保护。未改 `task/task.go`、native queue/recovery/delivery 或外部钱包协议。
- 原 stop/join 仅等待当前续租 goroutine；protocol6 handoff 是预付款 recovery 的事实，不能冒充图片 poll executor。未补历史 coverage；所有未知 use 继续 open/unproven。poll 持久 executor、全局 billing、联合关闭/safe-disable、完整应用 C2、真实 issuer/全副本围栏/独立恢复水位均未完成。

## 发布约束

未推送、未部署、未访问生产/既有数据库、未执行迁移或付费生成。后续由主控按用户最新顺序：独立分支 → 本地测试 → PR 审核 → 精确提交测试服验收 → 合入 GitHub main → 核对合并版本 → 用户再次明确确认后才可发布正式服。原 A 会话保持暂停。

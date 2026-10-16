# C：图片预付款恢复执行交接

基线：已验收 `913258983ed90676b547d59e799594aa15340c65` 的唯一 claim。本批只覆盖一次实际 `recoverImagePrepayments → imageprepayment.Sync` 的执行接收及返回，不代表完整 C 或排空完成。

## 行为与证据

[实际恢复入口](../core/task/image_prepayment.go) 在取得原有两分钟 claim 后调用 [PrepareImageRecoveryHandoff / Accept](../core/model/channel_admission_image_recovery_handoff.go)，只有唯一一次 Accept 获胜才进入 Sync。新增的 journal 同时记录 offered/accepted/exited 和 active/returned/error_returned；continuation 始终为 unproven。它不是 supplier 执行授权，也不创建新的业务期限。

Sync 及其拥有的同步清理正常返回后，原 sidecar 释放、journal exited 和 execution returned/error_returned 在同一事务提交。普通钱包错误只能证明这次拥有的调用已经返回，不能证明退款、结算或供应商终态。panic、Goexit、未返回的清理、丢租、写失败均保留 active/未证明状态。事务在所有写入及证明/日志锁等待后重新检查数据库时间，使用释放前原 deadline；失败整体回滚，不用新窗口掩盖旧超时。

claim 身份绑定原随机 owner、原截止时间、storage、完整原 TaskHash 和 reservation generation。Prepare 还比较领取时的不可变 core 与完整选中 contract，拒绝读取新状态刷新旧能力。持久记录仅保存授权摘要；nonce 和能力仅在私有 handle 中，JSON 为 `{}`。重复 Prepare/Accept 不会重进 Sync。已有 journal 时，旧 release API 拒绝；即使 Prepare 提交但响应或私有标记丢失，也通过数据库记录阻止降级。

稳定 core 绑定任务 ID/group/token/CreatedAt/usage/fingerprint/billing operation、公开模型及请求模型、原 quote、ExpectedImages 和原公开 contract。contract 只排除正式 failover 路径会变化的顶层 selected_provider_binding；完整当前 contract 仍绑定每次执行范围。当前路由、upstream 和可追加 attempts 不被误认为改变任务代际。

每条历史记录保留原 reservation use 及当时已存在的 attempt use 的完整摘要、index、resource、request、generation 和是否已持久化为 attempt。后续合法 attempt 不追记进旧范围；原 admission fact 缺失、替换或改写、已持久化 attempt 截断、伪造新增 attempt 均拒绝。历史交接存在但 reservation use 缺失时不能退回无日志兼容路径。旧范围的当前路由或原 claim 被替换时也不能补记完成。

证明加载在事务外；新的 proof 在锁内验证并做最终有效期 CAS。历史 snapshot 正常过期不撤销历史事实。同 epoch、同成员的有效 durable anchor 可在源断连时继续使用，不再访问源；新 epoch 或成员必须得到新的有效 proof。没有历史覆盖的同 epoch 冻结业务在临时源断连时返回 nil handle，继续原业务但不生成交接证据；源恢复后只能记录新调用，不能回填断档。显式撤销、冲突、损坏、storage 恢复不允许降级。native 协议实现未改动。

## 可选存储与迁移

[SQLite SQL 草案](../core/migrations/channel-image-recovery-handoff-v1/sqlite.sql) / [PostgreSQL SQL 草案](../core/migrations/channel-image-recovery-handoff-v1/postgres.sql) 新增 channel_admission_image_recovery_handoffs 和 protocol **6/v1**，依赖 image recovery claim **5/v1** 及其已有依赖。没有生产迁移或启动自动安装，两次重放仅在隔离测试库执行。

完全缺失扩展或没有私有 claim 的已有工作继续兼容路径，不追授历史交接。完整扩展若在 claim 写入期间安装，可在随后 Prepare 开始新的范围；若 Sync 已经开始才安装，当前调用不会补建 journal。部分安装、版本错误、缺列或数据库错误均拒绝，不能当作扩展不存在。

## 验证

全部使用合成身份、loopback 钱包、临时 SQLite / PostgreSQL；没有真实付费供应商、bucket 或生产数据库访问。

| 范围 | 结果 | 本地日志 |
| --- | --- | --- |
| 新交接模型及实际入口（包含在五包中） | 14 根 / 91 项全部通过 | `/tmp/b2-image-handoff-regression-final.jsonl` |
| admission/图片/relay/账单/租约五包 race | 188 根 / 755 项：754 pass，1 外部钱包 fixture skip | `/tmp/b2-image-handoff-regression-final.jsonl` |
| native 安全全包 race | 168 根 / 350 项：345 pass，5 外部 fixture skip | `/tmp/b2-image-handoff-native.jsonl` |
| PostgreSQL 两连接及写后超时 race | 2 根 / 24 项全部通过 | `/tmp/b2-image-handoff-postgres.jsonl` |
| 完整 gateway 构建 | go -C core build ./... 退出 0 | `/tmp/b2-image-handoff-build-final.log` |
| 提交前安全脚本及人工 diff 审查 | 无发现；11 个文件 | staged / worktree 扫描 |

最终五包和 PostgreSQL 均在最后一次生产代码补强后重跑。native 验证后仅更改新图片 handoff 的 core 比较与对应测试，没有改 native 调用路径。验证摘要在 `/tmp/b2-image-handoff-verification-summary.json`；临时 PostgreSQL 容器与网络残留均为 0（`/tmp/b2-image-handoff-pg-state.json`）。

实际入口验证同一任务连续三次恢复、两次合法 failover 和两次实际 Activate 的组合：原代际不变，当前 route/contract 可变，旧 journal 完全不变，四条 reservation/attempt use 保持 open。还覆盖实际迟到钱包响应、get/refund 多步回写、第二次回写失败、同 ID 合法重建、实际 claim 重领、同步 cleanup 阻塞、panic/Goexit、offer/accept/release 写失败、钱包普通错误、扩展缺失/晚安装/损坏/源故障。

模型测试覆盖唯一接收、Prepare 丢回复、原 deadline/owner/代际/core/contract/storage、历史 attempts 损坏与伪造、历史 proof 正常过期及断连/恢复/撤销、epoch/member 改变、证明加载不持事务锁。PostgreSQL 使用两个独立连接并观察 pg_stat_activity 的真实 Lock 等待，覆盖 task、journal、proof 锁的有效、自然到期、篡改对照；另验证 offer/accept/release/exit 最后写入后跨截止时间整笔回滚。短期限仅由已有测试编译 helper 缩短原夹具租约，production 始终为原两分钟。

项数包含父/子测试，范围之间有重叠，不相加成独立用例总数。早期新增测试曾有未使用 import、UTC 调度夹具、渠道地址、GORM 回调会话和计数范围问题，已修复并以最终日志为准。故障测试另促成 reservation 历史缺失及 Prepare core 刷新的生产边界补强。

## 限制与后续

- 交接只证明本次被接受的 Sync 及其拥有的同步清理返回，不能推出远端动作撤销、历史执行退出、连续覆盖或任务/账单终态。历史记录不自动 closed，也不能由 sidecar 为空反推排空。
- 图片独立轮询、全局 upstream billing、其他无此 handle 路径仍未纳入；未实现 ProcessingToken 原期限续期与停止 join、终态+账单+活跃执行器联合关闭、safe-disable。
- shadow/probe/SealPrepared、family/revision enable/pin CAS、应用完整 C2 仍待后续；issuer、全副本 fence、独立恢复 watermark 和历史断档仍是启用门槛。B 的真实长转存由主控后续组合。
- 无 MySQL 草案或实例验证；外部集成 fixture 明确跳过。没有 push、互合、部署、付费或生产 SQL；应用端未改动，也未声称其 TypeScript 基线通过。

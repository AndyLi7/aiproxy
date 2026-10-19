# C：图片预付款恢复唯一领取基础

基线：已验收 `205c9bdb4b97be65df100ab4ff036af26f96cd85`。本批仅建立图片预付款恢复的 durable owner，不是 executor/handoff 日志或完整 C 验收。

## 实际行为

[recoverImagePrepayments](../core/task/image_prepayment.go) 原先只按 id + billing_next_check_at 领取，并复用整批开始的 now。现在可选扩展启用后，通过 [ClaimImagePrepaymentRecovery](../core/model/channel_admission_image_recovery_claim.go) 在 control → image task → sidecar 的锁序下重读归属、原代次、冻结任务身份、预付费、未结算和到期条件。在 task 和 sidecar 均锁定后读取新数据库时间，再设置原有的两分钟期限。前一项慢钱包或锁等待不会消耗后一项的窗口。

一次领取原子写入 billing_next_check_at 与 sidecar 的随机 256-bit owner。原 deadline、storage generation、use generation 和任务摘要固定进提交后才附加到 ImageTask 的私有 handle；这些字段不会序列化到 API 或持久化回 ImageTask。sidecar 是数据库事实，不以时间戳或临时内存冒充唯一 owner。

[imageprepayment.Call](../core/common/imageprepayment/prepayment.go) 在本恢复调用的每次钱包请求之前验证原持有权；网络调用不持数据库锁。[withImageTaskGeneration](../core/model/channel_admission_image.go) 对携带该 handle 的所有回执写入，在原 generation 的同一事务前后验证原 owner 和期限，覆盖 SaveImageTaskBilling、neverHeld 的退款记录及 get 返回 refunded 后的结果/账单两次写。原有 task status/attempt 快照保护继续生效。

Sync 正常返回且其拥有的同步清理结束后才释放 sidecar；panic、Goexit、未返回的清理不会通过 defer 默认释放。释放只清原 owner/until，不修改 billing_next_check_at；使用释放前期限做写后新 DB 时间检查，失败全部回滚。旧 handle 不在内存清空，因此不能把随后调用降级到旧无 token 路径。换 owner、同时间不同 token、同 ID 新代、原期限到期均拒绝旧调用。

新领取可替换已到期的当前 owner，但这不是旧执行已退出的证据。正常释放后也不支持旧能力的成功重放；重复释放会拒绝，不能清理新持有权。已发出的远程幂等作用不会被撤销；失租后的远程结果仍须由后续恢复协调。

## 兼容与存储

[SQLite 草案](../core/migrations/channel-image-recovery-claim-v1/sqlite.sql) 与 [PostgreSQL 草案](../core/migrations/channel-image-recovery-claim-v1/postgres.sql) 新增 channel_admission_image_recovery_claims 与 protocol **id=5/version=1**，要求已有 channel-execution-v1（id=1/version=1），不要求 native/dispatch/recovery 子扩展。只在隔离测试库运行，两次 SQL 重放通过；未修改现有/生产 schema，未加入启动自动迁移。

扩展完全缺失或 C 从未激活时保留旧领取路径，不追授 token；扫描、领取或 Sync 中途安装不会补造此前的 owner。部分安装、错版本、缺列、数据库错误不能降级为缺失。证明源暂时故障不阻断已经合法冻结的任务：本批验证既有业务身份与 owner，不授予新的 deployment/executor 权限。DB/LogDB 同库保护沿用现有 admission runtime。

## 验证

全部为合成身份、loopback 钱包和隔离数据库，没有真实付费供应商或 bucket。

| 范围 | 结果 | 本地证据 |
| --- | --- | --- |
| 新增模型 race | 7 根 / 32 项通过 | `/tmp/b2-image-recovery-model.jsonl` |
| 实际恢复入口 + loopback 钱包 race | 4 根 / 22 项通过 | `/tmp/b2-image-recovery-actual.jsonl` |
| 最后新增实际两项批次慢钱包时钟 race | 1 根 / 1 项通过 | `/tmp/b2-image-recovery-batch-clock.jsonl` |
| PostgreSQL 双连接 task/sidecar 锁 race | 2 根 / 24 项通过 | `/tmp/b2-image-recovery-postgres.jsonl` |
| admission/图片/relay/账单/租约相关五包 race | 173 根 / 663 项：662 pass、1 外部钱包 fixture skip | `/tmp/b2-image-recovery-regression.jsonl` |
| native 全包安全 race | 168 根 / 350 项：345 pass、5 外部 fixture skip | `/tmp/b2-image-recovery-native.jsonl` |
| 完整 gateway 构建 | go -C core build ./... 退出 0 | `/tmp/b2-image-recovery-build.log` |

五包回归启动后仅追加单独通过的两项批次时钟用例，生产代码未再变化。根数包含被跳过的外部根测试，项数包含父/子测试；不同范围有重叠，不合并冒充独立用例总量。

实际入口验证了：旧批次 now 不再绑定后项；领取失败不发钱包请求；缺扩展/晚安装/源故障；丢失 owner 后不发下一钱包动作；neverHeld/get-refund 迟到结果拒绝落库；退款结果第一次写成功、第二次账单写失败时保留已提交事实；旧钱包调用期间通过真实 claim API 重领或合法删除/重建同 ID，旧响应不能写新代或释放新 owner；同步回调拥有的 defer 清理阻塞、panic/Goexit 和释放写失败。测试中的旧 owner 丢失为故障注入，随后重领/新代使用正式 API。

模型/PG 验证原 generation、原 owner、同期限 owner 更换、原 deadline、storage、快照、未付费/已结算/未到期拒绝、并发仅一次获胜、SQL重放、写失败回滚。PG 真双连接覆盖 task/sidecar 锁等待中的自然到期、owner/代次变化、有效对照，及旧 claim 在等待中到期后下一 claim 从锁后 DB 时间取得完整两分钟。写期间自然到期回滚使用仅编入 model 测试包的短原租约夹具（production 始终两分钟），不添加生产时钟开关或新超时。临时 PostgreSQL 容器及独立网络均已删除，残留 0。

## 明确限制与后续

- sidecar 仅保存当前 owner，会轮换或清空；它没有历史 executor 日志。空 owner/新 token/正常释放都不能证明历史覆盖、旧执行退出或排空，后续禁止据此补造历史。
- 本批只覆盖实际图片预付款恢复带 handle 的调用；其他独立图片轮询、全局 upstream billing 和不持该 handle 的现有路径仍未纳入。已有异步作用、进程消失或未 join 工作继续未知。
- 未完成 image executor/handoff proof、图片轮询 ProcessingToken 的续期/停止 join、终态+账单+活跃执行器联合关闭、safe-disable、shadow/probe/SealPrepared、family/revision enable/pin CAS 以及应用完整 C2。
- issuer、全副本 fence、独立恢复 watermark、历史断档仍是启用门槛；B 真实长转存由主控后续组合。
- MySQL 无本批草案或实例验证。无 push、互合、部署、付费或生产 SQL；应用端未改，未把已知 TypeScript 基线错误称为通过。

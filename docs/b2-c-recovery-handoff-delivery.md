# C：实际 native recovery 外层接手与原子退出

基线：已验收 dispatch handoff `cc40a6ebab61b6de887405fbdffd626440afe82c`。本批仅推进 C 的一个实际工作范围，不代表完整 C、安全停用或发布门槛完成。

## 交付行为

实际 [RecoverOnce](../core/common/nativetask/recovery.go) 在取得原唯一 recovery claim 后，仅对 **claim 时非 reserved** 的任务调用 Prepare / Accept，在钱包、轮询、交付或退款前完成 offered → accepted。原调用继续执行已有业务，没有新增工作 goroutine 或业务 TTL。

业务提取为 recoverClaim。只有正常返回、同步调用及其拥有的 defer/context 清理完成后，外层才记录退出。panic、runtime.Goexit、进程消失不会通过 defer 默认成功。Poll、Deliver、SyncBilling 及同步超时取消供应商调用都在这一范围；其内部未 join 的 goroutine、远端执行和未来调用不在范围内。

新增 [recovery handoff journal](../core/model/channel_admission_recovery_handoff.go) 和 native execution kind `native_recovery`：

- 复用 native v2 的 use/task/完整 channel identity、storage generation、历史 deployment proof 和私有 capability 校验。handle JSON 为 `{}`，持久化记录不能重建能力。
- Prepare 原子写 execution active 与 handoff offered；唯一 claim hash 绑定 use/task/原随机 owner token。合法延期不能让同一 claim 再次 Prepare 获得第二次授权。
- RecoveryUntil 固定记录原领取截止时间下界。Accept/exit 保留原 token，允许同 token 的当前 deadline 合法延长；重读只更新调度，不替换原 owner/下界。
- Accept 单次授权，重复返回 false。成功响应丢失不能授权重复业务。
- ReleaseAndRecordExit 在同一 control → task 事务中验证能力、身份、历史事实和调度快照，捕获清 owner 前锁定的当前合法 deadline，然后原子清 owner/until、写 next_recovery_at、execution returned/error_returned、handoff exited。所有写完成后用**新的数据库时间**检查释放前 deadline；到期或任一步失败全部回滚。
- 调度快照比较 status、created_at、upstream、queue scope、enqueued_at、dispatch owner/deadline 和账单回执。相同 status 的队列往返也会触发重读重算；最多三次，持续冲突或读失败保留未知，不进入旧无条件释放回退。
- 退出提交后不支持旧能力的释放成功幂等重放；失去原租约后重试会拒绝，包括同秒同 worker 再领取。已提交记录仍可读取，但不能授权再次工作或清理新 claim。
- continuation_state 始终 unproven，use 始终 open；没有账单结清、联合关闭或 safe-disable 推断。

## 存储与兼容

[SQLite 草案](../core/migrations/channel-recovery-handoff-v1/sqlite.sql) 和 [PostgreSQL 草案](../core/migrations/channel-recovery-handoff-v1/postgres.sql) 为独立 additive 扩展：channel_admission_recovery_handoffs + execution protocol **id=4/version=1**，要求 native v2（id=2/version=2），不要求 dispatch id=3。只用于隔离测试，未应用到现有/生产数据库，无历史回填。

完全缺扩展继续原恢复语义；部分安装、缺列、错版本、孤立 protocol、写失败不能降级为缺失。晚安装不补造当前调用此前未登记的接手。已有冻结事实支持证明源暂时故障和合法 epoch 切换；全部扩展晚装且没有历史 execution、源故障时沿用未覆盖业务，不制造记录。

新增 kind 不受更旧二进制识别：未来启用仍须完成全副本兼容/fence 审核。SQL 或测试 synthetic proof 均不是生产 rollout 授权。MySQL 无本批草案/实例验证。

claim 时 reserved 的旧逻辑不登记外层 handoff，包括后来转 waiting/failed 和 definitive-refusal 删除；不能据后续状态补造此前覆盖。缺扩展路径的原释放/回退语义保持原状。

## 验证

全部使用合成钱包/供应商和隔离数据库，无真实付费调用或存储桶。

| 范围 | 结果 | 本地证据 |
| --- | --- | --- |
| 新增模型 + 实际恢复针对性 race | 13 根 / 59 项通过 | `/tmp/b2-recovery-handoff-targeted.jsonl` |
| 最后新增全部扩展晚装 + 源故障的实际恢复 race | 1 根 / 1 项通过 | `/tmp/b2-recovery-handoff-late.jsonl` |
| native 安全全包 race | 167 根 / 349 项：344 通过、5 外部夹具跳过 | `/tmp/b2-recovery-handoff-native.jsonl` |
| model/controller/relay/task/image 相关五包 race | 162 根 / 609 项：608 通过、1 外部钱包夹具跳过 | `/tmp/b2-recovery-handoff-regression.jsonl` |
| 隔离 PostgreSQL race，含已有 dispatch/native execution/extension 回归 | 8 根 / 39 项通过 | `/tmp/b2-recovery-handoff-postgres.jsonl` |
| gateway 完整构建 | go -C core build ./... 退出 0 | `/tmp/b2-recovery-handoff-build.log` |

native 全包启动后只新增了单独通过的 late 用例，生产代码未再变化。根数包含被跳过的外部根测试，项数包含父/子测试；各范围有重叠，不能相加冒充独立用例总数。

实际入口覆盖：Poll 与 wallet execution_finished 的清理阻塞时保持 accepted/active/原租约，清理与 context cancel 后才能退出；panic/Goexit 保留未知；未 join 背景仍活动时只记录 owned scope；合法延期、同秒重领、准备失败无钱包/provider、退出写失败不清租约、reserved 不补覆盖。同 status 新 dispatch deadline 重算通过实际释放 helper 验证，其余通过实际 RecoverOnce。

模型额外覆盖：并发 Accept、延期后重复 Prepare、失回复重放、身份/proof/版本损坏、下界缩短、各次写失败、offer/accept/清租约/exit/return 写期间自然到期全部回滚。SQL 两次执行验证可重放。PostgreSQL 覆盖 task/handoff 行锁阻塞下 accept/release 的 live、自然到期、换 owner、延期或版本损坏；临时容器和独立网络已清理，残留均为 0。

## 限制与剩余工作

- reserved 删除、单独 image/background billing 及全部 worker 生命周期尚未统一闭合；旧 dispatch requeue 清 claim 后无法出具原退出证据的限制仍在。
- B 的真实长转存/延期调用链由主控后续组合；本批不冒充该验收。
- issuer、全副本 fence、独立恢复 watermark、历史缺口仍是启用门槛。
- terminal + billing + active executor 联合关闭、safe-disable、disabled shadow / restricted probe / SealPrepared、family/revision enable/pin CAS，以及完整应用 C2 prepare/live、历史 shared-key、API/UI/drain/revert 仍未交付。
- 未 push、合并、部署或应用生产 SQL。应用端未改，未重跑 pnpm build，也未把已知 TypeScript 基线错误称作通过。

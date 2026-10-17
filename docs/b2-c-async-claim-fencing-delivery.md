# C：原异步领取回执与图片生成超时事务防护

基线 `8dde1d4b78c765a85821495584f015def63ce01c`；独立分支 `codex/b2-async-claim-fencing`。无新增 schema、TTL、公开 API、迁移或报价变更，不修改 native queue/recovery/delivery。

## 复现与修复

`/private/tmp/b2-claim-before.log` 真实 SQLite 复现：Fail、Complete、Touch 在原 deadline 已过但 token 未轮换时仍成功写入，三项均失败。

[async_usage.go](../core/model/async_usage.go) 将 Fail/Complete、Touch/Retry、PrepareSettlement、BalanceConsumeAttempted/Consumed 的本地回执写统一到原领取事务：锁内重读 pending、id/token 和数据库实际 deadline；锁后取新 DB 时间，更新谓词再校验原 deadline，所有写结束再取新 DB 时间校验。失败整笔回滚。业务已改成下次调度时间的 `info.NextPollAt` 不作为当前 lease；同 token 已合法提交的延期可继续使用。无效/null/空 token 不写；bool 入口保持 false/nil，error-only 入口返回未找到原 claim，真实 SQL 错误保留。

严格拒绝非 pending 后发现实际图片生成超时先将 outbox 终态、稍后再次 Fail 清 token 的旧顺序。经主控批准，将 [ExpireImageGeneration](../core/model/image_task.go) 改为必传原 claim：task → usage → log 顺序，重读 task/usage 绑定和 pending/token/原 deadline；task、精确 outbox 终态和原 token 清理、绑定日志回执同事务。日志必须匹配 id/request/group/token/pending 且恰好更新一行，缺失或外组日志不能提交成功。最后日志锁等待期间失租，全部回滚。生成超时的 `now` 仅延用既有任务年龄判断；lease 始终按数据库时钟。

[实际 worker](../core/task/image_task.go) 仅传入原 claim，移除终态后重复 markFailed；退款/取消仍在持久终态提交后由既有机制执行，不在该 lease 事务内。过期或换 owner 的真实 worker 回归确认不触发退款/取消、不写 task/outbox/log 终态。原有人工 token 但无有效 lease 的测试夹具已修正，不放宽生产检查。

## 验证

- model 定向 race：147 pass、0 fail/skip，含七种回执入口的有效/过期未换 owner/换 owner/终态/空期限/nil/空token/合法延期/写后到期；图片超时有效、失租、最后日志写后到期、错task、缺失/外组log；原续租与日志范围回归。证据 `/private/tmp/b2-claim-model.jsonl`。
- task 全包 race：141 pass、4 skip、0 fail；skip 是两个 opt-in PostgreSQL stop/join 子场景和两个外部钱包 fixture，本批未运行这些 fixture。证据 `/private/tmp/b2-claim-task.jsonl`。
- 独立临时 PostgreSQL16：27 pass、0 fail/skip。Fail/Complete/Touch 三入口各验证真实锁等待有效、过期、换owner、非pending、写后自然过期、同token合法延期超过原deadline；图片超时最终log锁有效/过期、task锁过期/换owner。`pg_stat_activity` 确认真实 Lock 等待；证据 `/private/tmp/b2-claim-pg-q3djvq36/tests.jsonl`、`cleanup.json`，容器和临时凭据已清理。
- MySQL 仅真实 GORM 生成 SQL/绑定值与事务记录：FOR UPDATE → 新DB clock → 含原deadline的UPDATE → 最终新DB clock → commit/rollback。采用数据库已保存 DATETIME(3) 原期限，下一调度时间非整毫秒不会被错当原 lease。不是 MySQL 实例锁语义验证。
- `go -C core build ./...` 通过，证据 `/private/tmp/b2-claim-build.log`。重复 timeout 的原 claim 夹具最后微调后专项通过 `/private/tmp/b2-claim-deadline-repeat.jsonl`，不额外重复计数。

提交前 security-scan 脚本扫描 10 文件无发现；人工 diff 检查身份绑定、参数化 SQL、事务回滚及钱包边界，无新增问题。`git diff --cached --check` 通过。

## 限制

这只是原领取本地回执和实际生成超时终态事务的防护。其他图片供应商动作、非超时任务写入、普通日志更新、外部钱包/收据协议、全局账单 worker 的覆盖仍未完整；`BalanceConsumed` 本地标记受保护不等于远端扣费与本地事务原子。清 token 不证明 executor 退出、退款完成或 use 可以关闭。没有新增 executor/journal/use 关闭事实，不改变旧 active/未知/崩溃保守保留规则。完整 poll/handoff、safe-disable、C2、真实 issuer/全副本 fencing/恢复水位仍由主控后续统一验收。

未 push、merge、部署、调用真实供应商、访问真实 bucket 或生产数据库。原 A 与其他 worktree 保持不变。

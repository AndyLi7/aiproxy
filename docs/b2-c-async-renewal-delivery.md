# C：异步 ProcessingToken 续租与停止等待最小修正

基线：已验收 `f43ace39ec6c6da7e03d037ec942389e0b102d8a`。本批不新增 schema、TTL 或执行日志，只修正现有异步 worker 的续租原语及 stop/join。图片 poll 实际动作/回执 fencing 与持久交接后续单独交付。

## 复现与修正

修改前的 `/tmp/b2-poll-lease-before.jsonl` 记录三个定向用例：过期但 token 未变的租约被 Renew 续活、stop 在续租拥有的清理尚未退出时返回，两个用例均失败。同 worker/同秒换随机新 token 后，旧 token 的续租及 touch/retry/fail cleanup 已正确拒绝，作为通过对照保留，未另造 token 机制。

[RenewAsyncUsageClaimContext](../core/model/async_usage.go) 新增可取消入口，原 RenewAsyncUsageClaim 签名保留为 background context 兼容包装。事务锁定 pending + 原 id/token 的 usage 行后读取新数据库时间，要求原 next_poll_at 未过期、提议 deadline 严格晚于原值；相等/缩短提议不会改写期限。合法同 token 连续延期可逐次使用上次已提交的期限。

续租写入后再次用原 deadline 和执行当时数据库时间检查，并校验原 id/token/status 与刚写入的期望值。即使新 deadline 很远，只要原 deadline 在锁等待或最后写入期间过期，整笔事务回滚，返回未续租。写错误、取消和 commit 失败也不能返回成功。PostgreSQL 使用 READ COMMITTED 和 clock_timestamp，避免事务开始时间冒充锁后时间；SQLite/MySQL 保留对应方言支持，不依赖可选 C 表或部署证明。

[startAsyncUsageClaimRenewalAtInterval](../core/task/task.go) 在启动 goroutine 前固定 id/token 和日志字段。续租不再读取后来被业务更改的 info 身份，也不再并发写 info.NextPollAt；该字段留给业务安排下一次 poll/retry。数据库中的同 token deadline 仍正常延期，原三分钟 ProcessingToken 窗口及现有续租间隔未变。

续租使用派生 work context，stop 或父取消会取消数据库锁等待。完成信号在续租循环、事务及同步清理退出后关闭；每个 stop 调用者都在 sync.Once 外等待同一信号，覆盖重复/并发 stop、续租失败和父 context 取消。续租 goroutine 本身不调用 stop，避免自等。这里只 join 自己拥有的续租工作，没有声称取消任意供应商内部 goroutine 或远端作用。

## 验证

| 范围 | 结果 | 本地证据 |
| --- | --- | --- |
| 原失败复现及扩展专项 race | 7 根 / 23 项通过 | `/tmp/b2-poll-lease-targeted.jsonl` |
| model 异步/图片/交接相关 race | 51 根 / 303 项通过 | `/tmp/b2-poll-lease-model.jsonl` |
| task 全包 race | 72 根 / 142 项：138 pass、4 skip | `/tmp/b2-poll-lease-task.jsonl` |
| controller/relay/预付款入口 race | 52 根 / 162 项：161 pass、1 skip | `/tmp/b2-poll-lease-entry.jsonl` |
| PostgreSQL 两连接 race | 2 根 / 10 项通过 | `/tmp/b2-poll-lease-postgres.jsonl` |
| 全量 gateway 构建 | go -C core build ./... 退出 0 | `/tmp/b2-poll-lease-build.log` |

三个回归范围合计 175 根 / 607 项，602 pass、5 skip；专项与这些范围重叠，不重复相加。五个 skip 中两个是未带 DSN 的 PG stop 子用例（随后单独在真实临时 PG 中通过），三个是未启动的外部钱包 fixture。model 使用明确相关用例并排除旧自动启动容器的 Postgres 测试；并非 model 全包验证。摘要：`/tmp/b2-poll-lease-verification-summary.json`。

[模型测试](../core/model/async_usage_renewal_test.go) 验证过期、空 deadline、终态、缺行、错误 token、同秒重领、缩短/相等提议、合法连续延期、取消、写失败及最后写后自然到期。PG 通过 pg_stat_activity 确认两个连接的真实 Lock 等待，覆盖有效、等待中到期、换 owner、终态、取消锁等待及写后到期。

[worker 测试](../core/task/async_usage_renewal_join_test.go) 验证阻塞的 owned cleanup、四个并发 stop、父取消、续租失败，以及业务 info 身份改变后仍只续原 token 且不改业务 cursor。[PG stop 测试](../core/task/async_usage_renewal_postgres_test.go) 在另一连接仍持有 usage 行锁时，确认 stop/父取消能取消等待中的 SQL，并让所有 stop 调用者完成 join；原期限不变。

[MySQL 记录驱动测试](../core/model/async_usage_renewal_mysql_internal_test.go) 执行正式 Renew 代码，核对 FOR UPDATE → 新 DB 时间 → UPDATE → 原 deadline 最终谓词 → commit/rollback 的 SQL 与绑定值。它不连接 MySQL 实例，不能证明 MySQL 引擎锁语义。SQLite 与 PG 使用真实引擎。临时 PG 使用已有镜像、独立 schema/网络/tmpfs，容器与网络残留 0（`/tmp/b2-poll-lease-pg-state.json`）。

提交前按 security-scan 脚本及人工 diff 审查：7 个文件无发现；diff 格式与本地文档链接通过。没有新增依赖、公开 API、凭据或生产迁移。

## 限制与后续

- 本批不会让所有业务步骤自动获得实时 fencing；下一批须将原 token/有效延期贯通到实际图片 poll 的每次供应商/账单动作及迟到回执写入，再记录 owned 执行接手与退出。仅取消 context 不能证明已有远端作用终止。
- TryClaim 的原有调度行为、其他普通异步回执/日志写入原语尚未全面改造；没有把本批描述为完整 original-deadline fencing、safe-disable 或联合关闭。
- 无新的 journal、use 关闭或 continuation 证明；旧 active/历史断档保持未知。其他全局 billing、stop/join 之外的拥有关系、终态/账单/执行器联合关闭与完整 C2 仍待后续。
- MySQL 没有实例验证，外部账本/供应商未调用；未部署、push、互合或执行生产 SQL。既有图片、TTS、普通异步视频兼容性以本地回归范围为限，B 长转存/11 分钟租约由主控后续组合。

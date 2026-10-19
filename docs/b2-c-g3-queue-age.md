# B2 C / G3：队列年龄与 dispatch 租约

基于网关 `3148ef9`，独立工作区 `aiproxy-src/.worktrees/b2-step-c-gateway`。这是 C 的队列安全修正，不是准入屏障或安全停用能力交付。

- `NativeDispatchClaim.WaitTimeout` 接收当前运行时等待窗口，未提供或超过上限时使用既有五分钟上限。候选读取在数量截断之前排除过期等待，认领 CAS 再检查原始入队时间。等待锁和选路期间时间继续推进，最终 UPDATE 使用数据库时钟防止暂停 worker 携带旧 cutoff 执行。
- 缺少 `enqueued_at` 的既有记录使用原 `created_at`，与恢复器一致；恢复和重试不重写原入队时间。失败认领不自造终态，继续由既有互斥退款路径处理。
- 合法认领继续持有独立的两分钟 dispatch 租约。提交 CAS 复查 owner、状态、空 upstream ID 以及调用者和数据库时钟下均有效的 lease；没有重新套用 waiting 的五分钟总时长。生成阶段 `created_at` 语义不变。
- 钱包时钟领先仍保留其原始 admission 时间，既有 fast path 保持通过。这类未来时间不能作为后续 drain 已安全的证明。

## 验证

在 `core` 中执行本机 `/tmp/b2-c-g3-test-command.json` 保存的 `go test -race -json` 参数：42 个根测试、含子测试 66 项通过，无失败或 race 报告。覆盖真实临时 SQLite 重开及第二连接、暂停到 UPDATE 前才过期、旧调用者时钟、迟到 reserved 恢复、尚未运行退款扫描时的拒绝、单次退款、取消/认领互斥、合法 dispatch、过期/迟到钱包回复、运行时调小窗口及开关关闭兼容。记录 `/tmp/b2-c-g3-race.jsonl`。

`go build ./...` 退出 0，记录 `/tmp/b2-c-g3-build.log`。提交前安全脚本与人工审查通过，SQL 使用常量与参数绑定，不增加外部输入、供应商请求或资金计算；记录 `/tmp/b2-c-g3-security-staged.log`。

## 限制

仅运行 SQLite/假钱包/假供应商测试；PostgreSQL 和 MySQL 的数据库时钟语句已编译，但没有真实方言执行验证。为遵守隔离范围，未运行会启动 PostgreSQL 容器的测试。没有宣称验证跨进程 PostgreSQL 竞争、混合版本集群或生产时钟配置。

G1/G2/G4/G5 持久准入、同 key 身份豁免、全部使用证据及原子停用尚未完成；本补丁不能作为开启迁移、达到三十分钟即可停用或提示吊销的依据。无 schema/生产数据库变更，无部署或真实供应商调用。

## G3-R1：等待行锁期间过期

主控 PostgreSQL 复现表明 UPDATE 的时间谓词可能在等待行锁之前求值；若持锁事务只 SELECT FOR UPDATE 而不修改行，解锁后不能依赖谓词重新求值。修复保留 scope 锁顺序，在同一事务先取得目标行锁，再执行带数据库实时时间的状态 CAS。SQLite 使用身份条件 no-op 写获取 writer 锁；PostgreSQL/MySQL 使用 FOR UPDATE。没有把合法 dispatch 的两分钟租约改成总等待 TTL。

新增 `TestPostgresNativeExpiryAfterRowLockWait`，仅显式设置指向本机专用 `g3_queue_test` 数据库的 `B2_G3_TEST_POSTGRES_DSN` 才运行，不自行启动容器。实际 Go `ClaimNativeDispatch` / `StartNativeDispatchSubmission` 各验证等待行锁后过期拒绝、未过期成功、取消/退款已抢占拒绝；通过 `pg_stat_activity` 确认第二连接真实阻塞，过期情景持锁期间不改变行版本。主控授权后以已有 PostgreSQL 16 镜像建立本次临时 tmpfs 数据库和独立网络，仅暴露本机端口，完成六个双连接情景的 `-race` 验证：`/tmp/b2-c-g3-lock-postgres.jsonl`。既有 42 根/66 项 SQLite race 重新通过：`/tmp/b2-c-g3-lock-race.jsonl`。

此次真实方言验证覆盖 PostgreSQL 16 和 SQLite；本机没有已有 MySQL 镜像，没有拉取镜像，MySQL 队列 SQL 未做真实执行验证，不能由 Go 编译推断其锁语义已验收。后续 C 准入协议目前明确仅允许 PostgreSQL/SQLite 且 DB/LogDB 为同一连接对象，MySQL/分库拒绝 C；不把该协议的拒绝范围误写为已验证 MySQL 队列。

MySQL 补充验证使用记录 SQL 驱动调用真实 `StartNativeDispatchSubmission`，断言 BEGIN → SELECT FOR UPDATE（不预先判断时间）→ 带 `UTC_TIMESTAMP(6)` 的独立状态/owner UPDATE → COMMIT；锁失败则 ROLLBACK 且没有 UPDATE。共享 waiting 谓词的 MySQL 渲染检查 `TIMESTAMPADD` 参数。2 项 `-race` 通过，记录 `/tmp/b2-c-g3-lock-mysql-shape.jsonl`；这是生成 SQL/控制流验证，仍不是 MySQL 实例语义证明。按照主控决定，不因 C 尚未支持 MySQL 而关闭原有 MySQL 请求或队列。

修复后 `go build ./...` 退出 0（`/tmp/b2-c-g3-lock-build.log`），提交前安全记录 `/tmp/b2-c-g3-lock-security-staged.log`。本次 PostgreSQL 容器和专用网络删除均退出 0，临时随机凭据文件也已删除，清理证据 `/tmp/b2-c-g3-pg-cleanup.json`；没有删除镜像或其他容器。

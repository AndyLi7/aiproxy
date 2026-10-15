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

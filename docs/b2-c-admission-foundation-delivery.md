# B2 C：持久准入协议基础（待主控审核）

基于已验收 G3 修复 `9861617`。本批只交付独立持久协议、审阅 SQL 草案与部署证据契约；不是完整 G1/G2/G4/G5 或 C2，不具备开启迁移的条件。

## 实现范围

- 完整物理身份摘要，独立 control/proof/family/barrier/use 表；准入事实和任务预留回调可在同一事务提交。数据库控制锁提供跨连接串行顺序，错误回滚不留使用或家族授权。
- 同 key 只授权精确后继，旧摘要别名/新建别名不继承；换 key 封住旧家族；连续迁移和精确回退保持独立历史，带外换 key 不能复用退休物理 ID 逃出屏障。重复操作校验全部结果字段，损坏结果拒绝。
- G5-D1：服务/实例/build/epoch 精确成员与执行者围栏摘要进入本地事实校验；契约覆盖旧连接、暂停调度者、cron/恢复/异步执行者，实际强制机制未核实即不可激活。
- G5-D2/D3：已验证快照在事务外读取，生产契约要求有界本地读取、外部刷新独立进行。control→channel→proof 锁之后读取数据库时间，最终 CAS 再约束 proof 事实/撤销/时效和控制版本；`RecordedAtMs` 不作为 drain 起点。既有记录不等待部署证明服务，也不表示可二次发送。
- G5-D4：内部服务端 `Activate` 校验单次 proofID 消费、单调 sequence、存储世代和独立最低水位；重复返回不刷新许可，旧证明不能重激活。真实 SQLite 旧快照在较高独立水位下拒绝新工作。
- PostgreSQL/SQLite 附加 SQL 只初始化空 epoch，重复执行仍不激活。MySQL/分库新增 C 操作在写入前拒绝；现有请求/队列没有接入本协议，行为不变。

## 验证

- `/tmp/b2-c-admission-test-command.json` 记录合并回归参数；`/tmp/b2-c-admission-combined-race.jsonl`：61 根测试、含子测试 99 项通过，无 race/失败。包含原 G3/MySQL SQL 形状回归，不能把它们全部算作新覆盖。
- `/tmp/b2-c-admission-postgres-final.jsonl`：实际 PostgreSQL 16 下 `Seal` 等待渠道行锁后证明过期/撤销/仍有效三场景，以及 PostgreSQL SQL 草案重复应用保持未激活。2 根测试、含子测试 5 项通过，`-race` 退出 0。第一次 SQL 草案测试使用 prepared statement 执行多语句而失败，已改用已有 pgx simple protocol，最终实际执行通过。
- `/tmp/b2-c-admission-pg-final-state.json`：本次新建临时容器、专用网络清理均退出 0；只使用已有镜像和 tmpfs，随机临时凭据仅在测试进程内，没有删除镜像或其他容器。
- `/tmp/b2-c-admission-build.log`：`go build ./...` 退出 0。提交前 security-scan 脚本和人工 diff 审查记录 `/tmp/b2-c-admission-security-staged.log`，没有待处理发现。

## 必须保留的限制

没有生产签发方/部署控制面适配器，没有 HTTP 管理入口、实际 relay/native/image/trial/async 请求接线、channel 普通更新的身份写保护、已存在任务/旧代码窗口的兼容记录、使用关闭/活跃 lease 证明、原子安全停用、回退专用探测、吊销确认或应用 C2 状态机/UI。当前单元中的 lane/旧执行者情景是协议调用，不是全入口端到端演练。

实际存储恢复的独立水位与执行者围栏必须由可验证控制面持续保证；普通表存在、旧有效签名或自报成员不能替代。内部可注入合成事实的测试不能用作真实激活证据。MySQL 只有拒绝新增 C 协议和既有队列 SQL/控制流检查，无 MySQL 实例锁语义证明。

完整的发布/回滚/备份恢复门槛见[部署证据契约](b2-c-admission-deployment-contract.md)。没有推送、合并、部署、生产迁移或真实供应商调用。

## ADMISSION-R1 审核修复

主控复现了同 key 的 `1→2` 之后普通 `2→1` 可重新开放退休身份。`Seal` 现按目标物理 ID 查询最近一次持久退休记录：已有退休历史的目标必须指定该记录的 `RevertOf`，并与当前来源构成完整身份的精确逆向；后续世代已替代的旧逆向记录不能重复授权。检查不再取决于是否换 key，且发生于家族/control/barrier 写入之前。真正的新物理后继仍可普通迁移，逐步撤销连续迁移和幂等历史结果仍可用。

新增七种拒绝情景：同 key 隐式回退、缺失操作、无关操作、跳过中间世代的隐式/错误显式回退、跨 key 隐式回退、旧的同一身份对逆向记录重放。逐例比较 control/family/barrier/use/proof 全部持久状态，并确认退休目标的新准入仍拒绝且不新增 use。另验证正确逐层显式回退和历史幂等读取不改变当前后继。

增量 `go test -race -json ./model -run '^TestAdmission' -count=1`：18 根/含子测试 39 项通过（`/tmp/b2-c-admission-r1-race.jsonl`）。`go build ./...` 退出 0（`/tmp/b2-c-admission-r1-build.log`）。提交前安全脚本与人工 diff 通过（`/tmp/b2-c-admission-r1-security-staged.log`）。此次未重跑已验收 G3，也未将基线 PostgreSQL 结果冒充新补丁的实例测试；主控已独立验证基线 PG 行锁/SQL 草案，本修复针对持久历史规则补充 SQLite 增量回归。启用限制与剩余范围不变。

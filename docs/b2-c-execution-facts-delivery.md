# C：relay 调用范围的持久执行事实

基于已验收的图片 IMAGE-R1 `7e4e80b`。这一批记录实际调用范围，不关闭 use，不提供 drain/safe-disable 完成结论。

## 记录与实际边界

新增附加协议行和执行日志。执行日志绑定 use 的完整不可变摘要、渠道身份、原 epoch、存储世代、service/instance/build 和开始时间；同一个 relay use 最多一个调用记录。`AdmitRelayExecution` 在既有 control 锁、渠道与 proof 校验事务内原子写入 use 和 active 调用，最终 proof 时间 CAS 失败会一起回滚。只有新准入执行该回调；同请求 ID 的新 attempt 仍使用服务端新代次。

真实 `DoHelper` 在 ConvertRequest 之前建立记录。原处理逻辑在内部函数中执行，响应读取及其 defer（包括响应体 Close）退出后，外层才记录 returned/error_returned/async_returned。嵌套 Handle 各自建立独立 use/调用；并不把响应头到达或开始流式输出当作返回。panic 不执行返回记录；写返回事实失败不改变已经产生的供应商/客户端结果，原 active/use 保留。返回记录用独立有界 DB context，客户端取消不会直接取消记录；五秒仅为该次本地 DB 写超时，不是任务或使用期限。

**范围是调用栈和响应体清理，不是全部执行者退出。** 所有记录的 `continuation_state` 均明确为 `unproven`，包括普通返回；适配器可能有未 join 的扫描 goroutine、后续请求、异步上游任务、poller 和账单 worker。这里没有将这些工作判为完成，也没有把 AsyncUsage 标志当作持久移交证明。后续必须接入其实际注册/接手/退出事实，才能消除该未知状态。不得仅凭 returned（或没有 active 行）判断排空。

只有原进程私有随机 nonce 能记录该调用的返回；日志仅保存绑定摘要，handle 不序列化到 JSON，不新增外部 API。返回事务再次锁 control、扩展协议、执行记录及精确 use，检查原完整事实和存储世代；迟到/丢失/改写/换代不能更新后来的调用。原合法返回不因封口或 proof 过期而被当成新工作，但这不授予供应商再次发送资格。状态重放只接受相同事实及相同返回状态，不刷新原时间。active 没有 TTL，丢进程能力也不自动结束。

## 存储与迁移边界

`core/migrations/channel-execution-v1/{sqlite,postgres}.sql` 是附加、可重复应用的审阅草案。没有运行时 AutoMigrate，没有应用到已有开发库或生产库。SQL 仅在测试新建 SQLite / PostgreSQL schema 中执行。

- 两张扩展表都完全不存在：保持已验收 admission v1 的 open-use 行为；不生成任何执行覆盖或关闭资格。
- 只有一张、缺协议行、版本不是 1、缺列或数据库读写失败：拒绝新 relay 准入，原子回滚此次 use，不静默降级。
- 扩展表单独存在而 admission 基础缺失：仍按不完整 C 存储拒绝，不能当作从未安装。
- 返回过程中丢失/改写扩展或基础事实：不补建，不伪造成功，仍保留未知使用。

原 v1 存储和表不删除、不重置；已有 use 不补写“此前已完整覆盖”。扩展版本不构成全入口/全副本升级证明。MySQL / 分库的既有限制保持。

## 验证

- `/tmp/b2-c-execution-regression.jsonl`：5 包 race 120 根/400 项通过，0 fail。原 `real_wallet_prepaid` 因本轮未启动应用钱包 fixture 跳过 1 项；主控对父提交的独立真实钱包验证仍是父提交证据，不冒充本批重跑。
- `/tmp/b2-c-execution-final-targeted.jsonl`：最后新增 Close 与写失败边界后，race 8 根/36 项通过，0 fail/skip。覆盖完整/缺失/损坏/错误版本扩展、执行写失败和 proof 写后过期原子回滚、原返回跨 seal/revoked proof、存储/use/执行者变化、丢能力；实际 HTTP 正常/截断流式/客户端取消/JSON body 暂停；实际 Handle 嵌套、panic、后台 goroutine 尚活跃的 async 返回、阻塞 Body.Close，以及返回日志写失败时客户结果不被改写。
- `/tmp/b2-c-execution-postgres.jsonl`：真实 PostgreSQL16 双连接 4 场景（1 根/5 项）通过，0 skip：live、等待执行行锁时 executor/use 被改、等待 control 锁时存储世代变化。只有 live 写 returned，其他保留 active；所有 use 继续 open。SQL 草案逐语句在同一真实事务中执行并重放。第一次 harness 直接把多条 SQL 交给 pgx prepared statement 被拒，修正 harness 后重跑通过，未将第一次失败算通过。
- `/tmp/b2-c-execution-pg-state.json`：exit 0，自建容器/网络均 0。只使用已缓存 PostgreSQL16 镜像，未接触现有 DB。
- `/tmp/b2-c-execution-build.log`：`go -C core build ./...` exit 0。
- 提交前 security-scan worktree/staged、全差异人工审阅及 `git diff --check`；扫描证据 `/tmp/b2-c-execution-security-worktree.log`、`/tmp/b2-c-execution-security-staged.log`。

## 后续仍需实现

native/image/后台执行者持久接手、终态/账单/执行者联合关闭、三十分钟起点与安全停用；真实 disabled shadow/受限 probe/SealPrepared、family+revision enable/pin CAS；应用 prepare/live 历史共享 key 取证、adapter/API/UI/drain/revert。真实 issuer/持续全副本执行者围栏及独立恢复水位未证明，生产 C 仍不可激活。不进入 publication B，无 push/merge/deploy、真实付费供应商或 bucket 操作。

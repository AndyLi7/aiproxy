# C：native 调用执行事实与恢复领取身份

基于已验收 relay 执行事实 `820ded4508bd2f2e6157df9762414d9cd2499979`。本批记录 native 调用范围并修复恢复领取身份；不关闭 use，不提供 drain / safe-disable 完成结论。

## 实际入口与绑定

真实 Engine 的 Submit（reserved 路径）、dispatchClaimed、Poll、Deliver 和 SyncBilling 在外部钱包 / 供应商调用前建立独立 active 子记录，原业务函数正常返回后写 returned / error_returned。同一冻结 use 可有多个并发或嵌套调用；内层返回不能覆盖仍在运行的外层。普通客户 Poll / Deliver 不借用数据库里其他 worker 的恢复 owner。原公开签名及业务时长保持。

子记录绑定原 reservation/use、完整渠道身份、任务不可变摘要、调用 kind、存储世代和 service / instance / build；私有随机 nonce 留在进程 handle 中，JSON 序列化为空，不新增客户端授权入口。开始时锁后核对任务快照；返回时核对原 use 世代和不可变事实。现有 reserved / dispatching 到 submitting 会重写 CreatedAt，因此只在开始时核对该快照，不把合法提交后的时间变化当作换代。

恢复调用持有原 claim token 和原期限；dispatch 持有原 dispatch owner 和期限。控制行、协议和任务锁后检查所有权与数据库当前时间，并在记录写入后再次检查。panic、失租、换代、存储恢复、能力丢失、任务被删或返回事实写失败均保留 active / unproven；记录失败不改写已发生的业务结果。五秒仅为返回事实的本地 DB 写超时。

所有 continuation_state 都仍为 unproven，所有 use 保持 open。调用返回、没有 active 行、业务终态、账单完成或租约到期，均不是全执行者退出证明。恢复外层、取消、其他图片 / 后台 poll / 通用账单等入口没有在本批获得完整覆盖；不补写旧任务的历史覆盖。

## 恢复领取与延期契约

原 worker 名称及秒级 recovery_until 会在同一秒的合法释放 / 重领中同时重复，不能作为领取代次。新增反例先在旧实现失败，修复后 SQLite / PostgreSQL 及实际 RecoverOnce 清理路径通过。

ClaimNativeRecovery 的 owner 参数仍是 worker 标签；每次成功领取在既有 recovery_owner 列写入新的随机 claim token，并在返回任务中携带 token 与期限。调用方必须保留返回的 RecoveryOwner，用于后续操作和释放，不再传 worker 标签。Engine、既有释放证据调用及相关测试均已更新；重读 fresh 任务不能替换原 token。旧调用的尾部清理不能清除同 worker、同秒、同期限的新领取。

同 token 的受保护延期保持领取身份。私有上下文保留原 token / until；新子调用要求 DB 期限不小于原期限，返回要求不小于该子记录开始时观察到的期限，且锁后仍有效。缩短期限、换 token、自然过期均拒绝。不会因重新读取较新期限而接管其他领取。

本分支新增 ExtendNativeRecovery(db, id, owner, until)，与 B 使用的接口一致：先锁任务，按数据库时间检查原租约；只单调延长，同值或较小目标不缩短；写后仍以原期限检查数据库时间，等待锁或更新期间过期会回滚，不能用新未来期限复活旧租约。已验证同 token 延长至十一分钟后原调用及嵌套调用可返回，并验证新 token 不能借用旧能力。没有改变业务租约 / archive 时长。

B 的真实长转存 / delivery lease 调用链尚未合并，本批仅验证 A 的延期原语及 native 执行记录组合。B 集成需沿用返回的 claim token，并保留锁后及写后的数据库时间保护；不能将本批结果宣称为 B 实际长转存组合已通过。MySQL 分支未做实例验证；C 扩展仍仅支持 SQLite / PostgreSQL。

## 存储与迁移

[SQLite 草案](../core/migrations/channel-native-execution-v1/sqlite.sql)和 [PostgreSQL 草案](../core/migrations/channel-native-execution-v1/postgres.sql)新增 channel_admission_task_executions 表，并在既有 execution protocol 表增加 id=2 / version=1。须先安装 channel-execution-v1；没有对已有开发库或生产库应用迁移，没有运行时 AutoMigrate。

扩展表及协议行都不存在时，保持已验收 v1 冻结任务行为，且不读取新的执行证明源；只存在部分、缺列、错误版本或读取失败则拒绝，不能静默降级。扩展首次启用不补写在途旧调用；新增记录也不证明全部副本已升级。SQL 只在隔离 SQLite / PostgreSQL 测试库中执行并重放。

## 验证证据

以下日志位于本机 /tmp，未提交生成物；各组测试有重叠，不相加宣称覆盖数量。

- `b2-c-native-execution-final-targeted.jsonl`：race 22 根 / 67 项，0 fail / skip，覆盖模型边界、实际 Engine 入口及原 Native R1 回归。
- `b2-c-native-execution-regression.jsonl`：5 包 race 131 根 / 438 项，0 fail；其中 1 个 real_wallet_prepaid 子项因未启动真实应用钱包 fixture 跳过，437 项 pass。
- `b2-c-native-execution-native-regression.jsonl`：整个 nativetask 包 race，152 根 / 262 项，0 fail；147 根 / 257 项 pass，5 个依赖外部配置的根测试 skip。显式排除会自行启动 Docker 的 postgres 子测试，本轮 PostgreSQL 用下述隔离测试单独验证。
- 上述 5 个跳过项为 NativeCorpusExecution、NativeCrossServiceArchiveRecovery、NativeRealWalletSettlementAndReplay、NativeRealWalletRejectedSubmissionRefund、NativeRealWalletLostBeginAttemptReplyRefundsWithoutPause。未运行付费供应商、真实 bucket 或真实钱包联调；主控对父提交的真实钱包证据不冒充本批重跑。
- `b2-c-native-execution-postgres.jsonl`：真实 PostgreSQL16 race 4 根 / 15 项，0 fail / skip；包括开始 / 返回等待任务锁时 owner 变化、自然过期、同 token 延期，并发 Poll 独立记录，同秒释放重领，以及延期等待锁后不能复活失效或被接管租约。
- `b2-c-native-execution-extension-final.jsonl`：最终增加同目标重放及较小目标不缩短断言后，race 2 根 / 2 项，0 fail / skip。
- `b2-c-native-execution-pg-state.json`：exit 0，自建容器 / 网络均 0。仅使用已缓存 PostgreSQL16 镜像和新建临时隔离资源，没有接触已有数据库。
- `b2-c-native-execution-build.log`：go -C core build ./... exit 0。最后只补充测试断言，没有再改应用代码。
- 提交前 security-scan worktree / staged、人工差异审阅及 git diff --check；检查 SQL 参数绑定、私有能力、所有权与换代边界、敏感信息输出和迁移范围。

首次完整 native 回归发现旧测试仍向 releaseRecovery 传 worker 标签，更新为实际返回的 claim token 后完整重跑通过；没有忽略该失败。最初 same-second 反例的预期失败证据保留在 b2-c-native-execution-same-second-before.log。

## 仍未完成

全入口持久移交与执行者退出、业务终态 / 账单 / 执行者联合关闭、三十分钟起点和安全停用；真实 disabled shadow / 受限 probe / SealPrepared、family+revision enable/pin CAS；应用 prepare/live 历史共享 key 取证、adapter / API / UI / drain / revert。真实 issuer、持续全副本围栏及独立恢复水位仍未证明，生产 C 不可据此激活。未进入 publication B，未 push / merge / deploy。
